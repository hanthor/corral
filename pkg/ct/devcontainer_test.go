package ct

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripJSONComments_LineComment(t *testing.T) {
	src := []byte(`{
  "image": "debian:12", // the base image
  "remoteUser": "vscode"
}`)
	var cfg DevContainerConfig
	stripped := stripJSONComments(src)
	if err := json.Unmarshal(stripped, &cfg); err != nil {
		t.Fatalf("parsing after stripJSONComments: %v", err)
	}
	if cfg.Image != "debian:12" || cfg.RemoteUser != "vscode" {
		t.Errorf("got %+v", cfg)
	}
}

func TestStripJSONComments_BlockComment(t *testing.T) {
	src := []byte(`{
  /* block
     comment */
  "image": "fedora:40"
}`)
	var cfg DevContainerConfig
	if err := json.Unmarshal(stripJSONComments(src), &cfg); err != nil {
		t.Fatalf("parsing after stripJSONComments: %v", err)
	}
	if cfg.Image != "fedora:40" {
		t.Errorf("got %+v", cfg)
	}
}

func TestStripJSONComments_TrailingComma(t *testing.T) {
	src := []byte(`{
  "image": "ubuntu:24.04",
  "forwardPorts": [3000, 8080,],
}`)
	var cfg DevContainerConfig
	if err := json.Unmarshal(stripJSONComments(src), &cfg); err != nil {
		t.Fatalf("parsing after stripJSONComments: %v", err)
	}
	if len(cfg.ForwardPorts) != 2 {
		t.Errorf("got %+v", cfg.ForwardPorts)
	}
}

func TestStripJSONComments_SlashesInsideStringsSurvive(t *testing.T) {
	src := []byte(`{"image": "ghcr.io/foo/bar:latest"}`)
	var cfg DevContainerConfig
	if err := json.Unmarshal(stripJSONComments(src), &cfg); err != nil {
		t.Fatalf("parsing after stripJSONComments: %v", err)
	}
	if cfg.Image != "ghcr.io/foo/bar:latest" {
		t.Errorf("URL-shaped string was mangled: %q", cfg.Image)
	}
}

func TestFindDevContainerJSON_DirectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devcontainer.json")
	os.WriteFile(path, []byte(`{"image":"debian"}`), 0644)

	got, err := FindDevContainerJSON(path)
	if err != nil {
		t.Fatalf("FindDevContainerJSON: %v", err)
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestFindDevContainerJSON_DotDevcontainerDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0755)
	want := filepath.Join(dir, ".devcontainer", "devcontainer.json")
	os.WriteFile(want, []byte(`{"image":"debian"}`), 0644)

	got, err := FindDevContainerJSON(dir)
	if err != nil {
		t.Fatalf("FindDevContainerJSON: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindDevContainerJSON_RootDotFile(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".devcontainer.json")
	os.WriteFile(want, []byte(`{"image":"debian"}`), 0644)

	got, err := FindDevContainerJSON(dir)
	if err != nil {
		t.Fatalf("FindDevContainerJSON: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindDevContainerJSON_NotFound(t *testing.T) {
	if _, err := FindDevContainerJSON(t.TempDir()); err == nil {
		t.Error("expected an error when no devcontainer.json exists")
	}
}

func TestLoadDevContainerConfig_Full(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devcontainer.json")
	os.WriteFile(path, []byte(`{
  // a comment devcontainer.json authors commonly include
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "remoteUser": "vscode",
  "postCreateCommand": "npm install",
  "forwardPorts": [3000, "8080:8080"],
}`), 0644)

	cfg, err := LoadDevContainerConfig(path)
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	if cfg.Image != "mcr.microsoft.com/devcontainers/base:debian" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.RemoteUser != "vscode" {
		t.Errorf("RemoteUser = %q", cfg.RemoteUser)
	}
	if got := cfg.Ports(); len(got) != 2 || got[0] != 3000 || got[1] != 8080 {
		t.Errorf("Ports() = %v, want [3000 8080]", got)
	}
	post, err := cfg.ResolvePostCreate()
	if err != nil {
		t.Fatalf("ResolvePostCreate: %v", err)
	}
	if post.Script != "npm install" {
		t.Errorf("postCreateCommand = %+v", post)
	}
}

func TestResolvePostCreate_Empty(t *testing.T) {
	cfg := &DevContainerConfig{}
	post, err := cfg.ResolvePostCreate()
	if err != nil {
		t.Fatalf("ResolvePostCreate: %v", err)
	}
	if post != nil {
		t.Errorf("expected nil for no postCreateCommand, got %+v", post)
	}
}

func TestResolvePostCreate_ArrayForm(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"postCreateCommand": ["npm", "ci"]}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	post, err := cfg.ResolvePostCreate()
	if err != nil {
		t.Fatalf("ResolvePostCreate: %v", err)
	}
	if len(post.Argv) != 2 || post.Argv[0] != "npm" || post.Argv[1] != "ci" {
		t.Errorf("Argv = %v", post.Argv)
	}
	if post.Script != "" {
		t.Errorf("array form should not also set Script, got %q", post.Script)
	}
}

func TestResolvePostCreate_ObjectForm_NowSupported(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"postCreateCommand": {"install": "npm ci", "build": "go build ./..."}}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	cmds, err := cfg.ResolvePostCreateCommands()
	if err != nil {
		t.Fatalf("ResolvePostCreateCommands: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d: %+v", len(cmds), cmds)
	}
	names := map[string]string{}
	for _, c := range cmds {
		names[c.Name] = c.Script
	}
	if names["install"] != "npm ci" || names["build"] != "go build ./..." {
		t.Errorf("commands = %+v", cmds)
	}
	// Old API still errors for object form — single-command callers use that.
	if _, err := cfg.ResolvePostCreate(); err == nil {
		t.Error("ResolvePostCreate should still error for object form (use ResolvePostCreateCommands)")
	}
}

func TestResolvePostCreateCommands_StringForm(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"postCreateCommand": "npm install"}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	cmds, err := cfg.ResolvePostCreateCommands()
	if err != nil {
		t.Fatalf("ResolvePostCreateCommands: %v", err)
	}
	if len(cmds) != 1 || cmds[0].Name != "" || cmds[0].Script != "npm install" {
		t.Errorf("commands = %+v", cmds)
	}
}

func TestResolvePostStart_Script(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"postStartCommand": "echo ready"}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	post, err := cfg.ResolvePostStart()
	if err != nil {
		t.Fatalf("ResolvePostStart: %v", err)
	}
	if post.Script != "echo ready" {
		t.Errorf("Script = %q, want 'echo ready'", post.Script)
	}
}

func TestResolvePostStart_Empty(t *testing.T) {
	cfg := &DevContainerConfig{}
	post, err := cfg.ResolvePostStart()
	if err != nil {
		t.Fatalf("ResolvePostStart: %v", err)
	}
	if post != nil {
		t.Errorf("expected nil for no postStartCommand, got %+v", post)
	}
}

func TestResolvePostStart_ObjectForm_NowSupported(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"postStartCommand": {"notify": "echo started"}}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	cmds, err := cfg.ResolvePostStartCommands()
	if err != nil {
		t.Fatalf("ResolvePostStartCommands: %v", err)
	}
	if len(cmds) != 1 || cmds[0].Name != "notify" {
		t.Errorf("commands = %+v", cmds)
	}
	// Old API still errors for object form.
	if _, err := cfg.ResolvePostStart(); err == nil {
		t.Error("ResolvePostStart should still error for object form")
	}
}

func TestWithUser_NoopWhenEmpty(t *testing.T) {
	post := &ResolvedPostCreate{Script: "npm install"}
	if got := post.WithUser(""); got != post {
		t.Errorf("WithUser(\"\") should return the same value unchanged, got %+v", got)
	}
}

func TestWithUser_NilSafe(t *testing.T) {
	var post *ResolvedPostCreate
	if got := post.WithUser("vscode"); got != nil {
		t.Errorf("WithUser on a nil *ResolvedPostCreate should stay nil, got %+v", got)
	}
}

func TestWithUser_WrapsScript(t *testing.T) {
	post := &ResolvedPostCreate{Script: "npm install"}
	got := post.WithUser("vscode")
	if got.Argv != nil {
		t.Errorf("expected Argv unset after WithUser, got %v", got.Argv)
	}
	if !strings.Contains(got.Script, "su") || !strings.Contains(got.Script, "vscode") || !strings.Contains(got.Script, "npm install") {
		t.Errorf("WithUser script = %q", got.Script)
	}
}

func TestWithUser_WrapsArgv(t *testing.T) {
	post := &ResolvedPostCreate{Argv: []string{"npm", "ci"}}
	got := post.WithUser("vscode")
	if !strings.Contains(got.Script, "npm") || !strings.Contains(got.Script, "ci") {
		t.Errorf("WithUser should fold argv into the wrapped script, got %q", got.Script)
	}
}

func TestFlexPort_PlainNumber(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"forwardPorts": [5000]}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	if got := cfg.Ports(); len(got) != 1 || got[0] != 5000 {
		t.Errorf("Ports() = %v", got)
	}
}

func TestFlexPort_HostContainerString(t *testing.T) {
	cfg, err := LoadDevContainerConfig(writeTempConfig(t, `{"forwardPorts": ["9000:9001"]}`))
	if err != nil {
		t.Fatalf("LoadDevContainerConfig: %v", err)
	}
	if got := cfg.Ports(); len(got) != 1 || got[0] != 9001 {
		t.Errorf("Ports() = %v, want the container-side port 9001", got)
	}
}

func TestFlexPort_InvalidString(t *testing.T) {
	if _, err := LoadDevContainerConfig(writeTempConfig(t, `{"forwardPorts": ["not-a-port"]}`)); err == nil {
		t.Error("expected an error for a non-numeric forwardPorts entry")
	}
}

func TestExecArgv_PrivilegedScript(t *testing.T) {
	argv := execArgv(true, nil, "npm install")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "chroot") || !strings.Contains(joined, rootfsMountPath) || !strings.Contains(joined, "npm install") {
		t.Errorf("execArgv(privileged, script) = %v", argv)
	}
}

func TestExecArgv_UnprivilegedScript(t *testing.T) {
	argv := execArgv(false, nil, "npm install")
	if strings.Contains(strings.Join(argv, " "), "chroot") {
		t.Errorf("unprivileged execArgv should not chroot, got %v", argv)
	}
	if argv[0] != "sh" || argv[1] != "-c" || argv[2] != "npm install" {
		t.Errorf("execArgv(unprivileged, script) = %v", argv)
	}
}

func TestExecArgv_ArgvFormBypassesShell(t *testing.T) {
	argv := execArgv(false, []string{"npm", "ci"}, "")
	if len(argv) != 2 || argv[0] != "npm" || argv[1] != "ci" {
		t.Errorf("execArgv with argv form should run it directly, got %v", argv)
	}
}

// Exec's spec lookup goes through the mockable runner (see specFromPVC),
// but the actual `kubectl exec` it streams stdout/stderr through is a real
// exec.Command, same as Console — not mockable without deeper surgery, and
// there's no existing test for Console either. This covers what is
// testable: a CT that doesn't exist (no spec annotation) fails clearly
// rather than the whole chain silently reporting success.
func TestExec_UnknownCTErrors(t *testing.T) {
	withFake(t).AddResponseKV("kubectl", []string{
		"get", "pvc", "ghost-data", "-n", "corral-ct", "-o",
		`jsonpath={.metadata.annotations.corral\.ct/spec}`,
	}, "", fmt.Errorf("not found"))

	if err := Exec("ghost", "corral-ct", nil, "npm install"); err == nil {
		t.Error("Exec against a nonexistent CT should error, not silently no-op")
	}
}

func TestWaitReady_ReturnsOnceReady(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "pvc", "-A", "-l", labelCT + "=true", "-o", "json"},
		`{"items":[{"metadata":{"name":"myproj-data","namespace":"corral-ct","labels":{"corral.dev/ct":"true","corral.dev/ct-name":"myproj"},"annotations":{}}}]}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "pods", "-A", "-l", labelCT + "=true", "-o", "json"},
		`{"items":[{"metadata":{"name":"myproj","namespace":"corral-ct"},"spec":{"nodeName":"node1"},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}}]}`, nil)

	if err := WaitReady("myproj", "corral-ct", 5*1e9); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devcontainer.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp devcontainer.json: %v", err)
	}
	return path
}
