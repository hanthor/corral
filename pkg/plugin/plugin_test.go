package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyBuildSignature(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	sig := ed25519.Sign(private, []byte(digest))
	if err := verifyBuildSignature(base64.RawStdEncoding.EncodeToString(pub), digest, base64.RawStdEncoding.EncodeToString(sig)); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuildSignature(base64.RawStdEncoding.EncodeToString(pub), strings.Repeat("b", 64), base64.RawStdEncoding.EncodeToString(sig)); err == nil {
		t.Fatal("accepted signature for another digest")
	}
}

func TestDiscoveryAndResolve(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)

	if got := Dir(); got != dir {
		t.Fatalf("Dir() = %q, want %q", got, dir)
	}
	if len(Installed()) != 0 {
		t.Fatal("expected no plugins in empty dir")
	}
	if IsInstalled("bootc") {
		t.Fatal("bootc should not be installed")
	}

	// Drop a fake executable plugin and a non-plugin file.
	if err := os.WriteFile(filepath.Join(dir, "corral-bootc"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notaplugin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ps := Installed()
	if len(ps) != 1 || ps[0].Name != "bootc" {
		t.Fatalf("Installed() = %+v, want one plugin 'bootc'", ps)
	}
	if !IsInstalled("bootc") {
		t.Error("bootc should be installed")
	}
	if Resolve("bootc") == "" {
		t.Error("Resolve(bootc) should find the binary")
	}
}

func TestFindEntry(t *testing.T) {
	idx := &Index{Plugins: []Entry{{Name: "bootc"}, {Name: "other"}}}
	if idx.Find("bootc") == nil {
		t.Error("Find(bootc) should return an entry")
	}
	if idx.Find("missing") != nil {
		t.Error("Find(missing) should be nil")
	}
}

func TestDir_CustomEnv(t *testing.T) {
	t.Setenv("CORRAL_PLUGIN_DIR", "/custom/plugin/path")
	if got := Dir(); got != "/custom/plugin/path" {
		t.Errorf("Dir() = %q, want /custom/plugin/path", got)
	}
}

func TestDir_XDGDataHome(t *testing.T) {
	t.Setenv("CORRAL_PLUGIN_DIR", "") // unset custom
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	got := Dir()
	want := filepath.Join("/xdg/data", "corral", "plugins")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestDir_DefaultHome(t *testing.T) {
	t.Setenv("CORRAL_PLUGIN_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/fake/home")
	got := Dir()
	want := filepath.Join("/fake/home", ".local", "share", "corral", "plugins")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestResolve_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)
	// No plugins installed — PATH also won't have corral-nonexistent
	// But exec.LookPath may find it if a binary is in PATH. Use a very unlikely name.
	got := Resolve("zzz-nonexistent-plugin-xyz")
	if got != "" {
		t.Errorf("Resolve(nonexistent) = %q, want empty", got)
	}
}

func TestDispatch_NotFound(t *testing.T) {
	err := Dispatch("zzz-nonexistent-plugin-xyz", nil)
	if err == nil {
		t.Error("Dispatch(nonexistent) should return an error")
	}
}

func TestIsInstalled_NoPluginDir(t *testing.T) {
	t.Setenv("CORRAL_PLUGIN_DIR", t.TempDir())
	if IsInstalled("bootc") {
		t.Error("bootc should not be installed in empty dir")
	}
}

func TestMarketplaceURL_Default(t *testing.T) {
	t.Setenv("CORRAL_MARKETPLACE_URL", "")
	got := marketplaceURL()
	if got != DefaultMarketplaceURL {
		t.Errorf("marketplaceURL() = %q, want %q", got, DefaultMarketplaceURL)
	}
}

func TestMarketplaceURL_EnvOverride(t *testing.T) {
	t.Setenv("CORRAL_MARKETPLACE_URL", "https://example.com/custom.json")
	got := marketplaceURL()
	if got != "https://example.com/custom.json" {
		t.Errorf("marketplaceURL() = %q, want custom URL", got)
	}
}

func TestFetchIndex_InvalidURL(t *testing.T) {
	t.Setenv("CORRAL_MARKETPLACE_URL", "http://127.0.0.1:1/nope.json") // no server here
	_, err := FetchIndex()
	if err == nil {
		t.Error("FetchIndex with invalid URL should return an error")
	}
}

func TestFind_NotInIndex(t *testing.T) {
	idx := &Index{}
	if idx.Find("anything") != nil {
		t.Error("Find on empty index should return nil")
	}
}

func TestInstall_NoPlatform(t *testing.T) {
	e := &Entry{
		Name:      "testplugin",
		Platforms: map[string]Build{}, // no builds at all
	}
	err := e.Install()
	if err == nil {
		t.Error("Install with no platform builds should return an error")
	}
}

func TestInstall_BadURL(t *testing.T) {
	e := &Entry{
		Name: "testplugin",
		Platforms: map[string]Build{
			platformKey(): {URL: "http://127.0.0.1:1/bad.zip"},
		},
	}
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)
	err := e.Install()
	if err == nil {
		t.Error("Install with bad URL should return an error")
	}
}

func TestRemove_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)
	err := Remove("nonexistent")
	if err == nil {
		t.Error("Remove(nonexistent) should return an error")
	}
}

func TestRemove_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)
	// Create a fake plugin file
	p := filepath.Join(dir, "corral-testrm")
	os.WriteFile(p, []byte("x"), 0755)

	err := Remove("testrm")
	if err != nil {
		t.Errorf("Remove should succeed: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("plugin file should be removed")
	}
}

func TestInstalled_NonExecutable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)
	// Create a non-executable file named corral-<name>
	os.WriteFile(filepath.Join(dir, "corral-testplugin"), []byte("x"), 0644)
	ps := Installed()
	if len(ps) != 0 {
		t.Fatalf("Installed() must ignore non-executable files, got %+v", ps)
	}
}

func TestPlatformKey_NonEmpty(t *testing.T) {
	k := platformKey()
	if k == "" || !strings.ContainsAny(k, "/") {
		t.Errorf("platformKey() = %q, want GOOS/GOARCH", k)
	}
}

func TestFetchIndex_FromTestServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"plugins":[{"name":"bootc","description":"Boot container as VM","version":"0.1.0","platforms":{"linux/amd64":{"url":"https://example.com/bootc"}}}]}`))
	}))
	defer srv.Close()
	t.Setenv("CORRAL_MARKETPLACE_URL", srv.URL)

	idx, err := FetchIndex()
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(idx.Plugins))
	}
	if idx.Plugins[0].Name != "bootc" {
		t.Errorf("expected bootc, got %s", idx.Plugins[0].Name)
	}
	if idx.Find("bootc") == nil {
		t.Error("Find(bootc) should not be nil")
	}
}

func TestFetchIndex_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("CORRAL_MARKETPLACE_URL", srv.URL)

	_, err := FetchIndex()
	if err == nil {
		t.Error("FetchIndex with 500 status should return error")
	}
}

func TestFetchIndex_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	t.Setenv("CORRAL_MARKETPLACE_URL", srv.URL)

	_, err := FetchIndex()
	if err == nil {
		t.Error("FetchIndex with invalid JSON should return error")
	}
}

func TestInstall_FromTestServer(t *testing.T) {
	// Serve a fake binary
	fakeBinary := []byte("#!/bin/sh\necho hello\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeBinary)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)

	e := &Entry{
		Name: "testbin",
		Platforms: map[string]Build{
			platformKey(): {URL: srv.URL},
		},
	}
	err := e.Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Verify the file was created
	dst := filepath.Join(dir, "corral-testbin")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading installed plugin: %v", err)
	}
	if string(got) != string(fakeBinary) {
		t.Errorf("installed binary = %q, want %q", string(got), string(fakeBinary))
	}

	// Verify it's executable
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0111 == 0 {
		t.Error("installed binary should be executable")
	}
}

func TestInstall_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)

	e := &Entry{
		Name: "testbin",
		Platforms: map[string]Build{
			platformKey(): {URL: srv.URL},
		},
	}
	err := e.Install()
	if err == nil {
		t.Error("Install with 404 should return error")
	}
}

func TestInstall_ChecksumMatch(t *testing.T) {
	fakeBinary := []byte("binary with checksum")
	expectedSum := "87beee01d1183816f64d742e1a82895c201750002ee9647b7e21a5024b7b06ff"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeBinary)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)

	e := &Entry{
		Name: "testbin",
		Platforms: map[string]Build{
			platformKey(): {URL: srv.URL, SHA256: expectedSum},
		},
	}
	err := e.Install()
	if err != nil {
		t.Fatalf("Install with correct checksum: %v", err)
	}

	// Verify it was installed
	if _, err := os.Stat(filepath.Join(dir, "corral-testbin")); os.IsNotExist(err) {
		t.Error("plugin should be installed when checksum matches")
	}
}

func TestInstall_ChecksumMismatch(t *testing.T) {
	fakeBinary := []byte("wrong binary")
	badSum := "0000000000000000000000000000000000000000000000000000000000000000"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeBinary)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("CORRAL_PLUGIN_DIR", dir)

	e := &Entry{
		Name: "testbin",
		Platforms: map[string]Build{
			platformKey(): {URL: srv.URL, SHA256: badSum},
		},
	}
	err := e.Install()
	if err == nil {
		t.Error("Install with wrong checksum should return error")
	}
	// Verify the file was NOT written
	if _, err := os.Stat(filepath.Join(dir, "corral-testbin")); !os.IsNotExist(err) {
		t.Error("plugin should NOT be installed when checksum mismatches")
	}
}
