package main

// Tests for the pure helper functions of the corral-auth gateway (token
// generation, env fallback, passkey store persistence, gateway construction,
// ceremony bookkeeping). These ran at ~0% before this file; the OIDC HTTP
// flows (login/callback) need a live provider and are covered by
// TestBasicAuthUsesLibraryAndStripsCredential / manual e2e instead.

// package-level imports used above
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
)

func TestRandomToken(t *testing.T) {
	tok := randomToken()
	// 32 random bytes, base64url, no padding → 43 chars.
	if len(tok) != 43 {
		t.Fatalf("randomToken() length = %d, want 43", len(tok))
	}
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Fatalf("randomToken() not valid base64url: %v", err)
	}
	// Tokens must be unique.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := randomToken()
		if seen[tok] {
			t.Fatalf("randomToken() collided at iteration %d", i)
		}
		seen[tok] = true
	}
}

func TestRandomID(t *testing.T) {
	if got := randomID(24); len(got) != 24 {
		t.Fatalf("randomID(24) length = %d, want 24", len(got))
	}
	if got := randomID(0); len(got) != 0 {
		t.Fatalf("randomID(0) length = %d, want 0", len(got))
	}
	// Uniqueness.
	a, b := randomID(16), randomID(16)
	if string(a) == string(b) {
		t.Fatal("randomID() produced identical values")
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("CORRAL_AUTH_TEST_VAR", "present")
	if got := env("CORRAL_AUTH_TEST_VAR", "fallback"); got != "present" {
		t.Errorf("env() = %q, want %q", got, "present")
	}
	t.Setenv("CORRAL_AUTH_TEST_VAR", "")
	if got := env("CORRAL_AUTH_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("env() with empty var = %q, want fallback", got)
	}
	if got := env("CORRAL_AUTH_NEVER_SET_9f2c", "fallback"); got != "fallback" {
		t.Errorf("env() unset = %q, want fallback", got)
	}
}

func TestSaveLocked_WritesUsersWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "passkeys.json")
	pm := &passkeyManager{
		path: path,
		users: map[string]*passkeyUser{
			"alice": {ID: []byte("id-1"), Name: "alice"},
		},
		ceremonies: map[string]passkeyCeremony{},
	}
	if err := pm.saveLocked(); err != nil {
		t.Fatalf("saveLocked(): %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("saved passkey file mode = %v, want 0600", fi.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var users []*passkeyUser
	if err := json.Unmarshal(data, &users); err != nil {
		t.Fatalf("saved file is not valid JSON: %v\n%s", err, data)
	}
	if len(users) != 1 || users[0].Name != "alice" || string(users[0].ID) != "id-1" {
		t.Errorf("saved users = %+v, want [alice/id-1]", users)
	}
}

func TestSaveLocked_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "passkeys.json")
	pm := &passkeyManager{path: path, users: map[string]*passkeyUser{}, ceremonies: map[string]passkeyCeremony{}}
	if err := pm.saveLocked(); err != nil {
		t.Fatalf("saveLocked() into missing dir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestNewGateway_RejectsInvalidUpstream(t *testing.T) {
	for _, upstream := range []string{"", "not a url", "ftp://example.com", "http://"} {
		_, err := newGateway(context.Background(), "", "", "", "", upstream, []byte("01234567890123456789012345678901"))
		if err == nil {
			t.Errorf("newGateway(upstream=%q) expected error", upstream)
		}
	}
}

func TestNewGateway_OidcIssuerRequiresClientID(t *testing.T) {
	// Issuer set but no client ID/redirect URL → must fail before any
	// OIDC discovery network call.
	_, err := newGateway(context.Background(), "https://issuer.example.com", "", "", "", "http://127.0.0.1:8080", []byte("01234567890123456789012345678901"))
	if err == nil || !strings.Contains(err.Error(), "client ID and redirect URL are required") {
		t.Fatalf("newGateway(issuer, no clientID) error = %v, want missing-clientID error", err)
	}
}

func TestNewGateway_ValidUpstreamAndSecureFlag(t *testing.T) {
	key := []byte("01234567890123456789012345678901")

	g, err := newGateway(context.Background(), "", "", "", "https://corral.example.com/auth/callback", "http://127.0.0.1:8080", key)
	if err != nil {
		t.Fatalf("newGateway(): %v", err)
	}
	if g == nil || g.sessions == nil || g.proxy == nil {
		t.Fatal("newGateway() left nil fields")
	}
	if !g.secure {
		t.Error("secure = false, want true for https redirect URL")
	}
	if u := g.proxy.Director; u == nil {
		t.Error("proxy director not configured")
	}

	// http redirect URL → insecure sessions.
	g2, err := newGateway(context.Background(), "", "", "", "http://corral.example.com/auth/callback", "http://127.0.0.1:8080", key)
	if err != nil {
		t.Fatalf("newGateway() http: %v", err)
	}
	if g2.secure {
		t.Error("secure = true, want false for http redirect URL")
	}
}

func TestNewGateway_ProxyTargetsUpstream(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	g, err := newGateway(context.Background(), "", "", "", "https://corral.example.com", "http://10.0.0.5:9090", key)
	if err != nil {
		t.Fatalf("newGateway(): %v", err)
	}
	req, err := http.NewRequest("GET", "https://corral.example.com/foo", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.proxy.Director(req)
	if req.URL.Host != "10.0.0.5:9090" || req.URL.Scheme != "http" {
		t.Errorf("proxied request host = %s (%s), want 10.0.0.5:9090 (http)", req.URL.Host, req.URL.Scheme)
	}
}

func TestCeremony_StoresEntryWithExpiry(t *testing.T) {
	pm := &passkeyManager{ceremonies: map[string]passkeyCeremony{}}
	id := pm.ceremony("login", "alice", &wa.SessionData{Challenge: "ch"})
	if id == "" {
		t.Fatal("ceremony() returned empty id")
	}
	c, ok := pm.ceremonies[id]
	if !ok {
		t.Fatalf("ceremony %q not stored", id)
	}
	if c.Kind != "login" || c.User != "alice" {
		t.Errorf("stored ceremony = %+v, want login/alice", c)
	}
	remaining := time.Until(c.Expires)
	if remaining <= 0 || remaining > 5*time.Minute {
		t.Errorf("ceremony expiry = %v from now, want ~5 minutes", remaining)
	}
	if c.Data.Challenge != "ch" {
		t.Errorf("ceremony data lost: %+v", c.Data)
	}
	// Different ceremonies get different ids.
	if id2 := pm.ceremony("register", "bob", &wa.SessionData{}); id2 == id {
		t.Error("ceremony() reused the same id")
	}
}

// ── OIDC HTTP handlers (no live provider needed) ─────────────────────────────

func testGateway() *gateway {
	return &gateway{
		sessions: sessions.NewCookieStore([]byte("01234567890123456789012345678901")),
		oauth: &oauth2.Config{
			ClientID:    "test-client",
			RedirectURL: "https://corral.example.com/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://issuer.invalid/authorize",
				TokenURL: "https://issuer.invalid/token",
			},
		},
	}
}

func TestLogin_RejectsWhenOidcNotConfigured(t *testing.T) {
	g := &gateway{} // oauth == nil
	rec := httptest.NewRecorder()
	g.login(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("login without OIDC = %d, want 404", rec.Code)
	}
}

func TestLogin_RedirectsToAuthorizeWithState(t *testing.T) {
	g := testGateway()
	rec := httptest.NewRecorder()
	g.login(rec, httptest.NewRequest("GET", "/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://issuer.invalid/authorize?") {
		t.Fatalf("Location = %q, want issuer authorize URL", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client" || q.Get("redirect_uri") != "https://corral.example.com/auth/callback" {
		t.Errorf("authorize query missing client/redirect: %v", q)
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Errorf("authorize query missing state/nonce/code_challenge: %v", q)
	}
	// A session cookie was set so callback can verify state.
	if len(rec.Result().Cookies()) == 0 {
		t.Error("login did not set a session cookie")
	}
}

func TestCallback_RejectsMissingSession(t *testing.T) {
	g := testGateway()
	rec := httptest.NewRecorder()
	g.callback(rec, httptest.NewRequest("GET", "/auth/callback?state=abc&code=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback without session = %d, want 400", rec.Code)
	}
}

func TestCallback_RejectsStateMismatch(t *testing.T) {
	g := testGateway()
	// Establish a session with a known state.
	req := httptest.NewRequest("GET", "/auth/login", nil)
	rec := httptest.NewRecorder()
	g.login(rec, req)
	cookies := rec.Result().Cookies()

	cb := httptest.NewRequest("GET", "/auth/callback?state=wrong&code=x", nil)
	for _, c := range cookies {
		cb.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	g.callback(rec2, cb)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("callback with mismatched state = %d, want 400", rec2.Code)
	}
}

func TestCallback_ExchangeFailureUnauthorized(t *testing.T) {
	g := testGateway()
	req := httptest.NewRequest("GET", "/auth/login", nil)
	rec := httptest.NewRecorder()
	g.login(rec, req)
	cookies := rec.Result().Cookies()

	// Pull the real state out of the session cookie so the check passes and
	// the flow proceeds to the (failing) token exchange against the dummy
	// issuer URL.
	cb := httptest.NewRequest("GET", "/auth/callback?state=placeholder&code=x", nil)
	for _, c := range cookies {
		cb.AddCookie(c)
	}
	store := g.sessions
	s, err := store.Get(cb, "corral_oidc")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	state, _ := s.Values["state"].(string)
	cb.URL.RawQuery = "state=" + url.QueryEscape(state) + "&code=not-a-real-code"

	rec2 := httptest.NewRecorder()
	g.callback(rec2, cb)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("callback with failing exchange = %d, want 401", rec2.Code)
	}
}

func TestLogout_ClearsSessionAndRedirects(t *testing.T) {
	g := testGateway()
	req := httptest.NewRequest("GET", "/auth/login", nil)
	rec := httptest.NewRecorder()
	g.login(rec, req)

	logoutReq := httptest.NewRequest("GET", "/auth/logout", nil)
	for _, c := range rec.Result().Cookies() {
		logoutReq.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	g.logout(rec2, logoutReq)
	if rec2.Code != http.StatusFound || rec2.Header().Get("Location") != "/auth/login" {
		t.Errorf("logout = %d -> %q, want 302 -> /auth/login", rec2.Code, rec2.Header().Get("Location"))
	}
}
