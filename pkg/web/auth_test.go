package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithLogin(method, login string) *http.Request {
	r := httptest.NewRequest(method, "/api/vms/ns/x/start", nil)
	if login != "" {
		r.Header.Set("Tailscale-User-Login", login)
	}
	return r
}

func TestIsAdmin_OpenWhenNoAllowlist(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "")
	if !isAdmin(reqWithLogin("POST", "")) {
		t.Error("with no allowlist, every caller should be admin (single-user mode)")
	}
	if authEnforced() {
		t.Error("authEnforced should be false with no allowlist")
	}
}

func TestIsAdmin_AllowlistGatesMutations(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "Alice@github, bob@github")
	if !authEnforced() {
		t.Fatal("authEnforced should be true with an allowlist")
	}
	if !isAdmin(reqWithLogin("POST", "alice@github")) { // case-insensitive
		t.Error("alice should be admin")
	}
	if isAdmin(reqWithLogin("POST", "mallory@github")) {
		t.Error("mallory should not be admin")
	}
	if isAdmin(reqWithLogin("POST", "")) {
		t.Error("an unidentified caller should not be admin when enforced")
	}
}

func TestAdminGate_AllowsSafeMethods(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "alice@github") // enforced
	called := false
	gate := adminGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest("GET", "/api/vms", nil))
	if !called || rec.Code != 200 {
		t.Errorf("GET should pass the gate; called=%v code=%d", called, rec.Code)
	}
}

func TestAdminGate_BlocksNonAdminMutation(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "alice@github")
	called := false
	gate := adminGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, reqWithLogin("POST", "mallory@github"))
	if called {
		t.Error("handler should not run for a non-admin mutation")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestAdminGate_AllowsAdminMutation(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "alice@github")
	called := false
	gate := adminGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, reqWithLogin("POST", "alice@github"))
	if !called || rec.Code != 200 {
		t.Errorf("admin mutation should pass; called=%v code=%d", called, rec.Code)
	}
}

func TestWhoami_EndToEnd(t *testing.T) {
	t.Setenv("CORRAL_ADMINS", "alice@github")
	fx := NewTestFixture()
	defer fx.Server.Close()

	// Non-admin caller: identified but read-only, and a mutation is rejected.
	req, _ := http.NewRequest("GET", fx.Server.URL+"/api/whoami", nil)
	req.Header.Set("Tailscale-User-Login", "mallory@github")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who struct {
		Login    string `json:"login"`
		Admin    bool   `json:"admin"`
		Enforced bool   `json:"enforced"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if who.Login != "mallory@github" || who.Admin || !who.Enforced {
		t.Errorf("whoami = %+v; want identified non-admin under enforcement", who)
	}

	// And the gate blocks their mutation through the real router.
	mreq, _ := http.NewRequest("POST", fx.Server.URL+"/api/vms/ns/x/start", nil)
	mreq.Header.Set("Tailscale-User-Login", "mallory@github")
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin mutation through the router: got %d, want 403", mresp.StatusCode)
	}
}

func TestCaller_NameFallsBackToLogin(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Tailscale-User-Login", "alice@github")
	login, name := caller(r)
	if login != "alice@github" || name != "alice@github" {
		t.Errorf("caller() = %q,%q; want login used as name fallback", login, name)
	}
}

func TestAuthenticationRequiredProtectsReads(t *testing.T) {
	t.Setenv("CORRAL_AUTH_REQUIRED", "true")
	called := false
	gate := adminGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/vms", nil))
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: called=%v status=%d", called, rec.Code)
	}
}

func TestPeerServiceTokenMarksTrustedMachine(t *testing.T) {
	t.Setenv("CORRAL_PEER_TOKEN", "secret-token")
	t.Setenv("CORRAL_AUTH_REQUIRED", "true")
	called := false
	h := peerServiceAuth(adminGate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = r.Header.Get("Corral-Service-Identity") == "corral-peer"
	})))
	r := httptest.NewRequest(http.MethodPost, "/api/vms/ns/x/start", nil)
	r.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("service request: called=%v status=%d", called, rec.Code)
	}
}
