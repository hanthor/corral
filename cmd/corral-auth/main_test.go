package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tg123/go-htpasswd"
	"golang.org/x/crypto/bcrypt"
)

func TestClaimPreference(t *testing.T) {
	m := map[string]any{"sub": "1", "email": "a@example.com"}
	if got := claim(m, "email", "sub"); got != "a@example.com" {
		t.Fatalf("claim=%q", got)
	}
}

func TestBasicAuthUsesLibraryAndStripsCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Tailscale-User-Login") != "basic|alice" {
			t.Errorf("identity header=%q", r.Header.Get("Tailscale-User-Login"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("Basic credential leaked upstream")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "users.htpasswd")
	if err := os.WriteFile(path, []byte("alice:"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := newGateway(context.Background(), "", "", "", "", upstream.URL, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	g.basic, err = htpasswd.New(path, htpasswd.DefaultSystems, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.handler())
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/vms", nil)
	req.SetBasicAuth("alice", "correct horse")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestPasskeyManagerRequiresRelyingParty(t *testing.T) {
	if _, err := newPasskeyManager(filepath.Join(t.TempDir(), "keys.json"), "", "", ""); err == nil {
		t.Fatal("accepted incomplete relying-party configuration")
	}
	if _, err := newPasskeyManager(filepath.Join(t.TempDir(), "keys.json"), "corral.example.com", "https://corral.example.com", "bootstrap"); err != nil {
		t.Fatal(err)
	}
}
func TestCookieKey(t *testing.T) {
	if _, err := cookieKey("short"); err == nil {
		t.Fatal("short key accepted")
	}
	if b, err := cookieKey("01234567890123456789012345678901"); err != nil || len(b) != 32 {
		t.Fatalf("key: %v %d", err, len(b))
	}
}
