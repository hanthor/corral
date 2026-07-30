package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAuthKey_FromEnv(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-auth-abc123")
	// Ensure no config file interferes
	t.Setenv("HOME", t.TempDir())

	key := AuthKey()
	if key != "tskey-auth-abc123" {
		t.Errorf("AuthKey() = %q, expected %q", key, "tskey-auth-abc123")
	}
}

func TestContextSetAggregatesAndDefaultOnlySelects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CORRAL_INCUS_REMOTE", "")
	t.Setenv("CORRAL_LIBVIRT_URI", "")
	if err := AddContext(ContextConfig{Name: "lab-k8s", Backend: "kubevirt", Context: "lab"}); err != nil {
		t.Fatal(err)
	}
	if err := AddContext(ContextConfig{Name: "hypervisor", Backend: "libvirt", Context: "qemu+ssh://hv/system"}); err != nil {
		t.Fatal(err)
	}
	if err := SetDefaultContext("lab-k8s"); err != nil {
		t.Fatal(err)
	}
	if got := DefaultContext(); got.Name != "lab-k8s" || got.Context != "lab" {
		t.Fatalf("default=%+v", got)
	}
	seen := map[string]bool{}
	for _, target := range Contexts() {
		seen[target.Name] = true
	}
	for _, name := range []string{"local", "lab-k8s", "hypervisor"} {
		if !seen[name] {
			t.Errorf("context %q disappeared after switching default", name)
		}
	}
}

func TestAuthKey_FromFile(t *testing.T) {
	// Clear env var so we fall through to file
	t.Setenv("TS_AUTHKEY", "")

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "test-config.yaml")
	cfg := Config{Tailscale: TailscaleConfig{AuthKey: "tskey-file-xyz789"}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Tailscale.AuthKey != "tskey-file-xyz789" {
		t.Errorf("Tailscale.AuthKey = %q, expected %q", loaded.Tailscale.AuthKey, "tskey-file-xyz789")
	}
}

func TestAuthKey_EnvOverFile(t *testing.T) {
	// AuthKey checks env FIRST, then file. Env should win.
	t.Setenv("TS_AUTHKEY", "tskey-from-env")

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "test-config.yaml")
	cfg := Config{Tailscale: TailscaleConfig{AuthKey: "tskey-from-file"}}
	data, _ := yaml.Marshal(cfg)
	os.WriteFile(configPath, data, 0644)

	// Load directly to confirm file has its own value
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tailscale.AuthKey != "tskey-from-file" {
		t.Fatal("file value not loaded correctly")
	}

	// AuthKey() checks env first — env should win
	key := AuthKey()
	if key != "tskey-from-env" {
		t.Errorf("AuthKey() = %q, expected env value %q", key, "tskey-from-env")
	}
}

func TestAuthKey_None(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	// Point HOME to an empty temp dir so DefaultPath finds nothing
	t.Setenv("HOME", t.TempDir())

	key := AuthKey()
	if key != "" {
		t.Errorf("AuthKey() = %q, expected empty string", key)
	}
}

func TestLoad_Empty(t *testing.T) {
	tmp := t.TempDir()
	missingPath := filepath.Join(tmp, "does-not-exist.yaml")

	cfg, err := Load(missingPath)
	if err != nil {
		t.Errorf("Load of nonexistent file returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load returned nil config")
	}
	if cfg.Tailscale.AuthKey != "" {
		t.Errorf("expected empty auth key, got %q", cfg.Tailscale.AuthKey)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmp := t.TempDir()
	badPath := filepath.Join(tmp, "bad.yaml")
	os.WriteFile(badPath, []byte("{{{ not yaml"), 0644)

	_, err := Load(badPath)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	path := DefaultPath()
	expected := "/home/testuser/.config/corral/config.yaml"
	if path != expected {
		t.Errorf("DefaultPath() = %q, expected %q", path, expected)
	}
}

func TestLoad_DirectoryNotFile(t *testing.T) {
	// A directory path passed to Load should return an error (not IsNotExist)
	tmp := t.TempDir()
	_, err := Load(tmp) // tmp is a directory, not a file
	if err == nil {
		t.Error("expected error when loading a directory as config file")
	}
}

func TestAuthKey_LoadError(t *testing.T) {
	// AuthKey calls Load("") when TS_AUTHKEY is empty.
	// When Load returns an error (not IsNotExist), AuthKey returns "".
	t.Setenv("TS_AUTHKEY", "")

	// Point HOME to a path where DefaultPath can't be read
	tmp := t.TempDir()
	confDir := filepath.Join(tmp, ".config", "corral")
	os.MkdirAll(confDir, 0755)
	// Create the config path as a directory so ReadFile fails
	configPath := filepath.Join(confDir, "config.yaml")
	os.Mkdir(configPath, 0755)
	t.Setenv("HOME", tmp)

	key := AuthKey()
	if key != "" {
		t.Errorf("AuthKey() = %q, expected empty on Load error", key)
	}
}

func TestAuthKey_FileWithoutKey(t *testing.T) {
	// AuthKey loads file successfully but the file has no tailscale.auth_key
	t.Setenv("TS_AUTHKEY", "")

	tmp := t.TempDir()
	confDir := filepath.Join(tmp, ".config", "corral")
	os.MkdirAll(confDir, 0755)
	configPath := filepath.Join(confDir, "config.yaml")
	// Write a valid YAML config without tailscale section
	os.WriteFile(configPath, []byte("other: value\n"), 0644)
	t.Setenv("HOME", tmp)

	key := AuthKey()
	if key != "" {
		t.Errorf("AuthKey() = %q, expected empty when config has no tailscale key", key)
	}
}

func TestAuthKey_FromDefaultPath(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	tmp := t.TempDir()
	confDir := filepath.Join(tmp, ".config", "corral")
	os.MkdirAll(confDir, 0755)
	cfgData, _ := yaml.Marshal(Config{Tailscale: TailscaleConfig{AuthKey: "tskey-default-123"}})
	os.WriteFile(filepath.Join(confDir, "config.yaml"), cfgData, 0644)
	t.Setenv("HOME", tmp)

	key := AuthKey()
	if key != "tskey-default-123" {
		t.Errorf("AuthKey() = %q, expected 'tskey-default-123'", key)
	}
}

func TestDefaultFirmware(t *testing.T) {
	t.Run("default is uefi", func(t *testing.T) {
		t.Setenv("CORRAL_FIRMWARE_DEFAULT", "")
		t.Setenv("HOME", t.TempDir())
		if fw := DefaultFirmware(); fw != "uefi" {
			t.Errorf("DefaultFirmware() = %q, want 'uefi'", fw)
		}
	})

	t.Run("override via env", func(t *testing.T) {
		t.Setenv("CORRAL_FIRMWARE_DEFAULT", "bios")
		if fw := DefaultFirmware(); fw != "bios" {
			t.Errorf("DefaultFirmware() = %q, want 'bios'", fw)
		}
	})

	t.Run("override via config", func(t *testing.T) {
		t.Setenv("CORRAL_FIRMWARE_DEFAULT", "")
		tmp := t.TempDir()
		confDir := filepath.Join(tmp, ".config", "corral")
		os.MkdirAll(confDir, 0755)
		cfgData, _ := yaml.Marshal(Config{Firmware: FirmwareConfig{Default: "bios"}})
		os.WriteFile(filepath.Join(confDir, "config.yaml"), cfgData, 0644)
		t.Setenv("HOME", tmp)

		if fw := DefaultFirmware(); fw != "bios" {
			t.Errorf("DefaultFirmware() = %q, want 'bios'", fw)
		}
	})
}

// A host with no kubeconfig must not be handed a kubevirt context it can never
// reach: every consumer of Contexts (fleet.List, doctor, the web dashboard)
// would then report a permanent failure for a backend the user never selected.
func TestContexts_KubevirtNeedsAKubeconfig(t *testing.T) {
	clearBackendEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("CORRAL_INCUS_REMOTE", "")
		t.Setenv("CORRAL_LIBVIRT_URI", "")
		t.Setenv("CORRAL_KUBE_CONTEXT", "")
	}
	hasKubevirt := func() bool {
		for _, c := range Contexts() {
			if c.Backend == "kubevirt" {
				return true
			}
		}
		return false
	}

	t.Run("absent on a qemu-only host", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("KUBECONFIG", "")
		if hasKubevirt() {
			t.Errorf("kubevirt context offered without a kubeconfig: %+v", Contexts())
		}
	})

	t.Run("present once a kubeconfig exists", func(t *testing.T) {
		clearBackendEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("KUBECONFIG", "")
		kube := filepath.Join(home, ".kube")
		if err := os.MkdirAll(kube, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kube, "config"), []byte("apiVersion: v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !hasKubevirt() {
			t.Errorf("kubevirt context missing with ~/.kube/config present: %+v", Contexts())
		}
	})

	// KUBECONFIG naming a file that isn't there is the same as having none.
	t.Run("absent when KUBECONFIG points at nothing", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
		if hasKubevirt() {
			t.Errorf("kubevirt context offered for an unreadable KUBECONFIG: %+v", Contexts())
		}
	})

	// An explicitly configured cluster is honoured whatever the filesystem says.
	t.Run("present when explicitly configured", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("KUBECONFIG", "")
		if err := SetKubeContext("lab"); err != nil {
			t.Fatal(err)
		}
		if !hasKubevirt() {
			t.Errorf("configured kubevirt context dropped: %+v", Contexts())
		}
	})
}
