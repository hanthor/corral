package config

import (
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds corral configuration.
type Config struct {
	Tailscale TailscaleConfig `yaml:"tailscale"`
	Firmware  FirmwareConfig  `yaml:"firmware"`
	Web       WebConfig       `yaml:"web"`
	CT        CTConfig        `yaml:"ct"`
}

// CTConfig holds Container defaults.
type CTConfig struct {
	Backend string `yaml:"backend"` // "kubevirt", "incus", or "qemu" — auto-detected on first use
}

// WebConfig holds web UI theme and branding overrides.
type WebConfig struct {
	Accent        string `yaml:"accent"`
	Accent2       string `yaml:"accent_2"`
	BrandTitle    string `yaml:"brand_title"`
	BrandEmoji    string `yaml:"brand_emoji"`
	BrandSubtitle string `yaml:"brand_subtitle"`
	CustomCSS     string `yaml:"custom_css"`
}

// FirmwareConfig holds firmware boot defaults.
type FirmwareConfig struct {
	Default string `yaml:"default"` // "uefi" (default) or "bios"
}

// TailscaleConfig holds Tailscale-specific settings.
type TailscaleConfig struct {
	AuthKey string `yaml:"auth_key"`
	// Expose makes every new VM a tailnet device automatically: corral
	// deploys the proxy Service (tailscale operator annotations) for
	// SSH/VNC/RDP on create — no agent needed inside the guest.
	Expose bool `yaml:"expose"`
	// Tags applied to exposed VM devices, e.g. "tag:corral-vm".
	Tags string `yaml:"tags"`
}

// ConfigDir returns the directory containing config.yaml.
func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "corral")
}

// DefaultPath returns the default config file path.
func DefaultPath() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

// Load reads the config file from path. Returns empty config if file doesn't exist.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// AuthKey returns the Tailscale auth key from config or the TS_AUTHKEY env var.
func AuthKey() string {
	// Check env var first
	if key := os.Getenv("TS_AUTHKEY"); key != "" {
		return key
	}
	// Fall back to config file
	cfg, err := Load("")
	if err == nil && cfg.Tailscale.AuthKey != "" {
		return cfg.Tailscale.AuthKey
	}
	return ""
}

// TailnetExpose reports whether new VMs should be exposed on the tailnet by
// default (CORRAL_TAILNET_EXPOSE=true/1 or tailscale.expose in config.yaml).
func TailnetExpose() bool {
	if v := os.Getenv("CORRAL_TAILNET_EXPOSE"); v != "" {
		return v == "1" || v == "true" || v == "yes"
	}
	cfg, err := Load("")
	return err == nil && cfg.Tailscale.Expose
}

// TailnetTags returns the device tags for exposed VMs
// (CORRAL_TAILNET_TAGS or tailscale.tags in config.yaml).
func TailnetTags() string {
	if v := os.Getenv("CORRAL_TAILNET_TAGS"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil {
		return cfg.Tailscale.Tags
	}
	return ""
}

// DefaultFirmware returns the firmware boot default ("uefi" by default, or CORRAL_FIRMWARE_DEFAULT / firmware.default).
func DefaultFirmware() string {
	if v := os.Getenv("CORRAL_FIRMWARE_DEFAULT"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil && cfg.Firmware.Default != "" {
		return cfg.Firmware.Default
	}
	return "uefi"
}

// CTBackend returns the default CT backend: config file → auto-detect → save.
// Probes: kubevirt (kubectl + cluster) → incus → qemu. Persists the result
// so subsequent corral ct create calls default to the same backend.
func CTBackend() string {
	// Env override.
	if v := os.Getenv("CORRAL_CT_BACKEND"); v != "" {
		return v
	}
	// Config file.
	cfg, err := Load("")
	if err == nil && cfg.CT.Backend != "" {
		return cfg.CT.Backend
	}
	// Auto-detect.
	backend := detectCTBackend()
	// Persist for next time.
	if err == nil {
		cfg.CT.Backend = backend
		saveConfig(cfg)
	}
	return backend
}

func detectCTBackend() string {
	// kubevirt: kubectl is configured and can reach a cluster.
	if _, err := exec.LookPath("kubectl"); err == nil {
		// Quick check: can we list nodes?
		cmd := exec.Command("kubectl", "get", "nodes", "--request-timeout=2s")
		if cmd.Run() == nil {
			return "kubevirt"
		}
	}
	// incus: daemon socket is active.
	if _, err := os.Stat("/var/lib/incus/unix.socket"); err == nil {
		return "incus"
	}
	if _, err := exec.LookPath("incus"); err == nil {
		cmd := exec.Command("incus", "info")
		if cmd.Run() == nil {
			return "incus"
		}
	}
	// Fallback: local QEMU (always returns something).
	return "qemu"
}

func saveConfig(cfg *Config) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return
	}
	os.MkdirAll(ConfigDir(), 0o700)
	os.WriteFile(DefaultPath(), data, 0o600)
}
