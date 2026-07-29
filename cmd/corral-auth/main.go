// corral-auth is an optional OIDC reverse-proxy plugin for corral web.
// It keeps identity/session dependencies out of the main corral binary.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"github.com/spf13/cobra"
	"github.com/tg123/go-htpasswd"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"golang.org/x/oauth2"
)

type gateway struct {
	oauth     *oauth2.Config
	verifier  *oidc.IDTokenVerifier
	sessions  *sessions.CookieStore
	proxy     *httputil.ReverseProxy
	secure    bool
	basic     *htpasswd.File
	passkeys  *passkeyManager
	peerToken string
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newGateway(ctx context.Context, issuer, clientID, clientSecret, redirectURL, upstream string, cookieKey []byte) (*gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream must be an http(s) URL")
	}
	store := sessions.NewCookieStore(cookieKey)
	secure := strings.HasPrefix(redirectURL, "https://")
	g := &gateway{sessions: store, proxy: httputil.NewSingleHostReverseProxy(u), secure: secure}
	if issuer != "" {
		if clientID == "" || redirectURL == "" {
			return nil, fmt.Errorf("OIDC client ID and redirect URL are required with an issuer")
		}
		provider, err := oidc.NewProvider(ctx, issuer)
		if err != nil {
			return nil, fmt.Errorf("OIDC discovery: %w", err)
		}
		g.oauth = &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: provider.Endpoint(), RedirectURL: redirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
		g.verifier = provider.Verifier(&oidc.Config{ClientID: clientID})
	}
	return g, nil
}

func (g *gateway) session(r *http.Request) (*sessions.Session, error) {
	s, err := g.sessions.Get(r, "corral_oidc")
	if err == nil {
		s.Options = &sessions.Options{Path: "/", MaxAge: 8 * 60 * 60, HttpOnly: true, Secure: g.secure, SameSite: http.SameSiteLaxMode}
	}
	return s, err
}
func (g *gateway) login(w http.ResponseWriter, r *http.Request) {
	if g.oauth == nil {
		http.Error(w, "OIDC is not configured", http.StatusNotFound)
		return
	}
	s, err := g.session(r)
	if err != nil {
		http.Error(w, "invalid session", 400)
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	s.Values["state"], s.Values["nonce"], s.Values["verifier"] = state, nonce, verifier
	if err := s.Save(r, w); err != nil {
		http.Error(w, "saving session", 500)
		return
	}
	http.Redirect(w, r, g.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}
func (g *gateway) callback(w http.ResponseWriter, r *http.Request) {
	s, err := g.session(r)
	if err != nil {
		http.Error(w, "invalid session", 400)
		return
	}
	state, _ := s.Values["state"].(string)
	nonce, _ := s.Values["nonce"].(string)
	verifier, _ := s.Values["verifier"].(string)
	if state == "" || r.URL.Query().Get("state") != state {
		http.Error(w, "invalid OIDC state", 400)
		return
	}
	tok, err := g.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		http.Error(w, "OIDC exchange failed", 401)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	id, err := g.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "invalid ID token", 401)
		return
	}
	var claims map[string]any
	if err := id.Claims(&claims); err != nil {
		http.Error(w, "invalid claims", 401)
		return
	}
	gotNonce, _ := claims["nonce"].(string)
	if gotNonce != nonce {
		http.Error(w, "invalid OIDC nonce", 401)
		return
	}
	subject := claim(claims, "sub")
	issuer := claim(claims, "iss")
	login := subject
	if issuer != "" {
		login = issuer + "|" + subject
	}
	name := claim(claims, "name", "email", "preferred_username")
	if login == "" {
		http.Error(w, "OIDC identity has no usable subject", 401)
		return
	}
	s.Values = map[interface{}]interface{}{"login": login, "name": name, "email": claim(claims, "email")}
	if err := s.Save(r, w); err != nil {
		http.Error(w, "saving session", 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
func claim(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
func (g *gateway) logout(w http.ResponseWriter, r *http.Request) {
	s, _ := g.session(r)
	s.Options.MaxAge = -1
	_ = s.Save(r, w)
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}
func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if g.peerToken != "" && len(provided) == len(g.peerToken) && subtle.ConstantTimeCompare([]byte(provided), []byte(g.peerToken)) == 1 {
		r.Header.Del("Tailscale-User-Login")
		r.Header.Del("Tailscale-User-Name")
		r.Header.Del("Tailscale-User-Email")
		g.proxy.ServeHTTP(w, r)
		return
	}
	login, name, email := g.identity(r)
	if login == "" {
		if g.basic != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="Corral", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
		} else if g.passkeys != nil {
			http.Redirect(w, r, "/auth/passkey", http.StatusFound)
		} else {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
		}
		return
	}
	r.Header.Del("Tailscale-User-Login")
	r.Header.Del("Tailscale-User-Name")
	r.Header.Del("Tailscale-User-Email")
	r.Header.Set("Tailscale-User-Login", login)
	r.Header.Set("Tailscale-User-Name", name)
	if email != "" {
		r.Header.Set("Tailscale-User-Email", email)
	}
	r.Header.Del("Authorization")
	g.proxy.ServeHTTP(w, r)
}
func (g *gateway) identity(r *http.Request) (login, name, email string) {
	if s, err := g.session(r); err == nil {
		login, _ = s.Values["login"].(string)
		name, _ = s.Values["name"].(string)
		email, _ = s.Values["email"].(string)
		if login != "" {
			return
		}
	}
	if g.basic != nil {
		if user, password, ok := r.BasicAuth(); ok && g.basic.Match(user, password) {
			return "basic|" + user, user, ""
		}
	}
	return "", "", ""
}
func (g *gateway) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /auth/login", g.login)
	m.HandleFunc("GET /auth/callback", g.callback)
	m.HandleFunc("POST /auth/logout", g.logout)
	if g.passkeys != nil {
		g.registerPasskeyRoutes(m)
	}
	m.HandleFunc("/", g.serve)
	return securityHeaders(m)
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func cookieKey(raw string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil && len(b) >= 32 {
		return b, nil
	}
	if len(raw) >= 32 {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("cookie secret must be at least 32 bytes (literal or base64url)")
}
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "auth", Version: "0.2.0", Description: "OIDC, Basic Auth, and passkey gateway", Capabilities: []string{"auth-gateway", "oidc", "basic-auth", "passkeys"}, Permissions: []string{"listen on network", "proxy Corral HTTP traffic", "store encrypted sessions and passkeys"}, SupportedBackends: []string{"all"}}) {
		return
	}
	var addr, upstream, issuer, clientID, clientSecret, redirectURL, secret, basicFile, passkeyFile, passkeyRPID, passkeyOrigin, enrollToken, peerToken string
	c := &cobra.Command{Use: "corral-auth", Short: "OIDC authentication gateway for corral web", RunE: func(*cobra.Command, []string) error {
		k, err := cookieKey(secret)
		if err != nil {
			return err
		}
		g, err := newGateway(context.Background(), issuer, clientID, clientSecret, redirectURL, upstream, k)
		if err != nil {
			return err
		}
		if basicFile != "" {
			g.basic, err = htpasswd.New(basicFile, htpasswd.DefaultSystems, nil)
			if err != nil {
				return fmt.Errorf("basic auth file: %w", err)
			}
		}
		if passkeyFile != "" {
			g.passkeys, err = newPasskeyManager(passkeyFile, passkeyRPID, passkeyOrigin, enrollToken)
			if err != nil {
				return err
			}
			if strings.HasPrefix(passkeyOrigin, "https://") {
				g.secure = true
			}
		}
		g.peerToken = peerToken
		if g.oauth == nil && g.basic == nil && g.passkeys == nil {
			return fmt.Errorf("configure at least one of OIDC, --basic-htpasswd, or --passkey-store")
		}
		srv := &http.Server{Addr: addr, Handler: g.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
		fmt.Fprintf(os.Stderr, "corral-auth: %s -> %s\n", addr, upstream)
		return srv.ListenAndServe()
	}}
	f := c.Flags()
	f.StringVar(&addr, "addr", env("CORRAL_AUTH_ADDR", ":8443"), "listen address")
	f.StringVar(&upstream, "upstream", env("CORRAL_AUTH_UPSTREAM", "http://127.0.0.1:8006"), "corral web URL")
	f.StringVar(&issuer, "issuer", env("CORRAL_OIDC_ISSUER", ""), "OIDC issuer URL")
	f.StringVar(&clientID, "client-id", env("CORRAL_OIDC_CLIENT_ID", ""), "OIDC client ID")
	f.StringVar(&clientSecret, "client-secret", env("CORRAL_OIDC_CLIENT_SECRET", ""), "OIDC client secret")
	f.StringVar(&redirectURL, "redirect-url", env("CORRAL_OIDC_REDIRECT_URL", ""), "OIDC callback URL")
	f.StringVar(&secret, "cookie-secret", env("CORRAL_AUTH_COOKIE_SECRET", ""), "32+ byte session encryption key")
	f.StringVar(&basicFile, "basic-htpasswd", env("CORRAL_BASIC_HTPASSWD", ""), "Apache htpasswd file for HTTP Basic authentication")
	f.StringVar(&passkeyFile, "passkey-store", env("CORRAL_PASSKEY_STORE", ""), "Passkey credential JSON store")
	f.StringVar(&passkeyRPID, "passkey-rp-id", env("CORRAL_PASSKEY_RP_ID", ""), "WebAuthn relying-party domain")
	f.StringVar(&passkeyOrigin, "passkey-origin", env("CORRAL_PASSKEY_ORIGIN", ""), "WebAuthn HTTPS origin")
	f.StringVar(&enrollToken, "passkey-enroll-token", env("CORRAL_PASSKEY_ENROLL_TOKEN", ""), "Bootstrap token for first passkey enrollment")
	f.StringVar(&peerToken, "peer-token", env("CORRAL_PEER_TOKEN", ""), "Scoped Corral-to-Corral service token passed to core")
	_ = c.MarkFlagRequired("cookie-secret")
	if err := c.Execute(); err != nil {
		b, _ := json.Marshal(err.Error())
		fmt.Fprintln(os.Stderr, string(b))
		os.Exit(1)
	}
}
