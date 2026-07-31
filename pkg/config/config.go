package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Config holds corral configuration.
type Config struct {
	Default   DefaultConfig   `yaml:"default"`
	Tailscale TailscaleConfig `yaml:"tailscale"`
	Firmware  FirmwareConfig  `yaml:"firmware"`
	Web       WebConfig       `yaml:"web"`
	CT        CTConfig        `yaml:"ct"`
	Incus     IncusConfig     `yaml:"incus"`
	Kubevirt  KubevirtConfig  `yaml:"kubevirt"`
	Peers     []PeerConfig    `yaml:"peers,omitempty"`
	Libvirt   LibvirtConfig   `yaml:"libvirt"`
	Contexts  []ContextConfig `yaml:"contexts,omitempty"`
	Folders   []FolderConfig  `yaml:"folders,omitempty"`
}

// FolderConfig is one stored folder (ADR-0008): a path and the instance
// selectors it holds. Members are the string form of types.InstanceRef rather
// than the struct, so this package stays free of domain types and pkg/folder —
// which owns the tree semantics — can import config without a cycle.
type FolderConfig struct {
	Path    string   `yaml:"path" json:"path"`
	Members []string `yaml:"members,omitempty" json:"members,omitempty"`
}

type DefaultConfig struct {
	Backend string `yaml:"backend"`
	Context string `yaml:"context,omitempty"`
}

// ContextConfig is one inventory target. All enabled contexts are aggregated
// simultaneously; the default only selects the destination for unqualified
// creates and commands.
type ContextConfig struct {
	Name    string `yaml:"name" json:"name"`
	Backend string `yaml:"backend" json:"backend"`
	Context string `yaml:"context,omitempty" json:"context,omitempty"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

func (c ContextConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func Contexts() []ContextConfig {
	cfg, err := Load("")
	if err != nil {
		return []ContextConfig{{Name: "local", Backend: "qemu"}}
	}
	out := []ContextConfig{{Name: "local", Backend: "qemu"}}
	seen := map[string]bool{"qemu\x00": true}
	for _, c := range cfg.Contexts {
		if !c.IsEnabled() || c.Name == "" || c.Backend == "" {
			continue
		}
		key := c.Backend + "\x00" + c.Context
		if !seen[key] {
			out, seen[key] = append(out, c), true
		}
	}
	// Legacy single-context settings remain visible until explicitly migrated,
	// but an empty config must not invent three remote targets. Selecting a
	// backend is also an explicit opt-in and preserves the old workflow.
	var legacy []ContextConfig
	// The active kubeconfig context has always been Corral's cluster target and
	// remains discoverable without explicit migration — but only when there is
	// a kubeconfig to act on. A qemu-only or Incus-only host must not be handed
	// a kubevirt target it can never reach, because every consumer of this list
	// (fleet.List, doctor, the web dashboard) then reports a permanent failure
	// for a backend the user never asked for. Incus/libvirt have no equivalent
	// ubiquitous client default, so those require opt-in below.
	if cfg.Kubevirt.Context != "" || cfg.Default.Backend == "kubevirt" ||
		os.Getenv("CORRAL_KUBE_CONTEXT") != "" || forceKubevirt.Load() || KubeconfigPresent() {
		legacy = append(legacy, ContextConfig{Name: "kubevirt", Backend: "kubevirt", Context: cfg.Kubevirt.Context})
	}
	if cfg.Incus.Remote != "" || cfg.Default.Backend == "incus" || os.Getenv("CORRAL_INCUS_REMOTE") != "" {
		legacy = append(legacy, ContextConfig{Name: "incus", Backend: "incus", Context: IncusRemote()})
	}
	if cfg.Libvirt.URI != "" || cfg.Default.Backend == "libvirt" {
		legacy = append(legacy, ContextConfig{Name: "libvirt", Backend: "libvirt", Context: LibvirtURI()})
	}
	for _, c := range legacy {
		key := c.Backend + "\x00" + c.Context
		if !seen[key] {
			out, seen[key] = append(out, c), true
		}
	}
	return out
}

func FindContext(name string) (ContextConfig, bool) {
	for _, c := range Contexts() {
		if c.Name == name {
			return c, true
		}
	}
	return ContextConfig{}, false
}

func AddContext(c ContextConfig) error {
	if c.Name == "" || c.Backend == "" {
		return fmt.Errorf("context name and backend are required")
	}
	if c.Backend == "qemu" && c.Context != "" {
		return fmt.Errorf("qemu is local and does not take a context")
	}
	switch c.Backend {
	case "qemu", "kubevirt", "incus", "libvirt":
	default:
		return fmt.Errorf("unsupported backend %q", c.Backend)
	}
	cfg, err := Load("")
	if err != nil {
		return err
	}
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == c.Name {
			cfg.Contexts[i] = c
			return Save(cfg)
		}
	}
	cfg.Contexts = append(cfg.Contexts, c)
	return Save(cfg)
}

func RemoveContext(name string) error {
	if name == "local" {
		return fmt.Errorf("the local qemu context cannot be removed")
	}
	cfg, err := Load("")
	if err != nil {
		return err
	}
	out := cfg.Contexts[:0]
	for _, c := range cfg.Contexts {
		if c.Name != name {
			out = append(out, c)
		}
	}
	cfg.Contexts = out
	if cfg.Default.Context == name {
		cfg.Default.Context = ""
	}
	return Save(cfg)
}

// Folders returns the stored folder tree, empty when none is configured.
func Folders() []FolderConfig {
	cfg, err := Load(DefaultPath())
	if err != nil {
		return nil
	}
	return cfg.Folders
}

// SetFolders replaces the stored folder tree. The whole document is written at
// once because a folder move rewrites many paths, and a per-folder API would
// leave the tree half-moved if a write failed midway.
func SetFolders(folders []FolderConfig) error {
	cfg, err := Load(DefaultPath())
	if err != nil {
		return err
	}
	cfg.Folders = folders
	return Save(cfg)
}

func SetDefaultContext(name string) error {
	c, ok := FindContext(name)
	if !ok {
		return fmt.Errorf("unknown context %q", name)
	}
	cfg, err := Load("")
	if err != nil {
		return err
	}
	cfg.Default.Backend, cfg.Default.Context = c.Backend, c.Name
	return Save(cfg)
}

func DefaultContext() ContextConfig {
	if cfg, err := Load(""); err == nil && cfg.Default.Context != "" {
		if c, ok := FindContext(cfg.Default.Context); ok {
			return c
		}
	}
	b := DefaultBackend()
	for _, c := range Contexts() {
		if c.Backend == b {
			return c
		}
	}
	return ContextConfig{Name: "local", Backend: "qemu"}
}

// DefaultBackend returns the backend used by unqualified create commands.
// Explicit command flags always take precedence.
func DefaultBackend() string {
	if v := os.Getenv("CORRAL_DEFAULT_BACKEND"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil && cfg.Default.Backend != "" {
		return cfg.Default.Backend
	}
	return "qemu"
}

func SetDefaultBackend(backend string) error {
	switch backend {
	case "qemu", "kubevirt", "incus", "libvirt":
	default:
		return fmt.Errorf("unsupported backend %q (want qemu, kubevirt, incus, or libvirt)", backend)
	}
	cfg, err := Load("")
	if err != nil {
		return err
	}
	cfg.Default.Backend = backend
	return Save(cfg)
}

// IncusConfig holds the default Incus remote. It is intentionally separate
// from the Incus CLI's active remote: Corral never calls `incus remote switch`.
type IncusConfig struct {
	Remote string `yaml:"remote"`
}
type KubevirtConfig struct {
	Context string `yaml:"context"`
}
type LibvirtConfig struct {
	URI string `yaml:"uri"`
}

func LibvirtURI() string {
	if v := os.Getenv("CORRAL_LIBVIRT_URI"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil && cfg.Libvirt.URI != "" {
		return cfg.Libvirt.URI
	}
	return "qemu:///system"
}
func SetLibvirtURI(uri string) error {
	cfg, err := Load("")
	if err != nil {
		return err
	}
	cfg.Libvirt.URI = uri
	return Save(cfg)
}

type PeerConfig struct {
	Name  string `yaml:"name" json:"name"`
	URL   string `yaml:"url" json:"url"`
	Token string `yaml:"token,omitempty" json:"-"`
}

func Peers() []PeerConfig {
	cfg, err := Load("")
	if err != nil {
		return nil
	}
	return cfg.Peers
}
func SetPeer(name, rawURL string) error {
	return SetPeerWithToken(name, rawURL, "")
}
func SetPeerWithToken(name, rawURL, token string) error {
	if name == "" || rawURL == "" {
		return fmt.Errorf("peer name and URL are required")
	}
	cfg, err := Load("")
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Peers {
		if cfg.Peers[i].Name == name {
			cfg.Peers[i].URL = strings.TrimRight(rawURL, "/")
			if token != "" {
				cfg.Peers[i].Token = token
			}
			found = true
		}
	}
	if !found {
		cfg.Peers = append(cfg.Peers, PeerConfig{Name: name, URL: strings.TrimRight(rawURL, "/"), Token: token})
	}
	return Save(cfg)
}
func RemovePeer(name string) error {
	cfg, err := Load("")
	if err != nil {
		return err
	}
	out := cfg.Peers[:0]
	for _, p := range cfg.Peers {
		if p.Name != name {
			out = append(out, p)
		}
	}
	cfg.Peers = out
	return Save(cfg)
}

// forceKubevirt makes Contexts offer the kubevirt target unconditionally.
var forceKubevirt atomic.Bool

// SetForceKubevirtContext declares that this process can reach a KubeVirt
// cluster whatever the filesystem says. Demo mode is the caller: it replaces
// every command runner with an in-memory cluster, so kubeconfig presence tells
// us nothing about reachability, and without this the demo fleet would be empty
// on a machine that has never run kubectl. Takes a value rather than latching,
// so tests that enable demo mode can put the process back as they found it.
func SetForceKubevirtContext(on bool) { forceKubevirt.Store(on) }

// KubeconfigPresent reports whether a kubeconfig this host can actually read
// exists — KUBECONFIG naming at least one readable file, or ~/.kube/config.
// It is the "does this machine talk to a cluster at all" test; it says nothing
// about whether that cluster is up or has KubeVirt installed.
func KubeconfigPresent() bool {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		for _, p := range filepath.SplitList(v) {
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".kube", "config"))
	return err == nil
}

func KubeContext() string {
	if v := os.Getenv("CORRAL_KUBE_CONTEXT"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil {
		return cfg.Kubevirt.Context
	}
	return ""
}
func SetKubeContext(context string) error {
	cfg, err := Load("")
	if err != nil {
		return err
	}
	cfg.Kubevirt.Context = context
	return Save(cfg)
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

// IncusRemote returns Corral's default Incus remote.
// Precedence is CORRAL_INCUS_REMOTE, config.yaml, then "local".
func IncusRemote() string {
	if v := os.Getenv("CORRAL_INCUS_REMOTE"); v != "" {
		return v
	}
	if cfg, err := Load(""); err == nil && cfg.Incus.Remote != "" {
		return cfg.Incus.Remote
	}
	return "local"
}

// SetIncusRemote persists Corral's default without changing Incus CLI state.
func SetIncusRemote(remote string) error {
	cfg, err := Load("")
	if err != nil {
		return err
	}
	cfg.Incus.Remote = remote
	return Save(cfg)
}

// Save writes config.yaml with private permissions.
func Save(cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(ConfigDir(), ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, DefaultPath())
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
	_ = Save(cfg)
}
