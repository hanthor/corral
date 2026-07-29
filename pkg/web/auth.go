package web

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Identity & authorization.
//
// The Tailscale operator Ingress that fronts corral-web proves the caller's
// tailnet identity and forwards it as request headers (Tailscale-User-Login /
// -Name). We surface that identity and gate mutating API calls behind an admin
// allowlist (CORRAL_ADMINS). See docs/adr/0003-identity-source.md.

// caller returns the authenticated tailnet login + display name from the
// Tailscale ingress headers. Both are empty when the request didn't arrive
// through the identity-proving proxy (e.g. local port-forward).
func caller(r *http.Request) (login, name string) {
	if service := strings.TrimSpace(r.Header.Get("Corral-Service-Identity")); service != "" {
		return service, service
	}
	login = strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	name = strings.TrimSpace(r.Header.Get("Tailscale-User-Name"))
	if name == "" {
		name = login
	}
	return login, name
}

// adminLogins parses CORRAL_ADMINS (comma/space separated tailnet logins).
// Empty → nil, which means "no allowlist configured": single-user/open mode.
func adminLogins() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("CORRAL_ADMINS"))
	if raw == "" {
		return nil
	}
	set := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			set[strings.ToLower(f)] = true
		}
	}
	return set
}

// authEnforced reports whether an admin allowlist is configured. When false the
// UI is single-user and every caller is an admin (back-compat default).
func authEnforced() bool { return len(adminLogins()) > 0 }

func authenticationRequired() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CORRAL_AUTH_REQUIRED")))
	return v == "1" || v == "true" || v == "yes"
}

// isAdmin reports whether the caller may perform mutating actions.
func isAdmin(r *http.Request) bool {
	if r.Header.Get("Corral-Service-Identity") != "" {
		return true
	}
	admins := adminLogins()
	if len(admins) == 0 {
		return true // no allowlist → open / single-user
	}
	login, _ := caller(r)
	email := strings.ToLower(strings.TrimSpace(r.Header.Get("Tailscale-User-Email")))
	return login != "" && (admins[strings.ToLower(login)] || (email != "" && admins[email]))
}

// peerServiceAuth validates the optional machine credential used by another
// Corral instance. It marks only constant-time verified requests as trusted;
// client-supplied service identity headers are always removed first.
func peerServiceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Corral-Service-Identity")
		expected := strings.TrimSpace(os.Getenv("CORRAL_PEER_TOKEN"))
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if expected != "" && len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 {
			r.Header.Set("Corral-Service-Identity", "corral-peer")
		}
		next.ServeHTTP(w, r)
	})
}

// adminGate wraps the router: safe (GET/HEAD/OPTIONS) requests always pass;
// mutating requests require an admin. This is defense in depth — the UI also
// hides controls, but the server is the real boundary.
func adminGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, _ := caller(r)
		if authenticationRequired() && strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/whoami" && login == "" {
			errResp(w, http.StatusUnauthorized, fmt.Errorf("authentication required"))
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !isAdmin(r) {
			who := login
			if who == "" {
				who = "an unauthenticated caller"
			}
			errResp(w, http.StatusForbidden,
				errReadOnly(who))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func errReadOnly(who string) error {
	return &readOnlyError{who: who}
}

type readOnlyError struct{ who string }

func (e *readOnlyError) Error() string {
	return "read-only: " + e.who + " is not in CORRAL_ADMINS"
}

// handleWhoami exposes the caller's identity and privilege so the UI can show
// the logged-in user and switch to read-only mode for non-admins.
func handleWhoami(w http.ResponseWriter, r *http.Request) {
	login, name := caller(r)
	jsonResp(w, http.StatusOK, map[string]any{
		"login":    login,
		"name":     name,
		"admin":    isAdmin(r),
		"enforced": authEnforced(),
	})
}
