package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds corral configuration.
type Config struct {
	Tailscale TailscaleConfig `yaml:"tailscale"`
	Firmware  FirmwareConfig  `yaml:"firmware"`
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

// DefaultPath returns the default config file path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "corral", "config.yaml")
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
