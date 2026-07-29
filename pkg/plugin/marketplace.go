package plugin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	semver "github.com/Masterminds/semver/v3"
)

const (
	DefaultMarketplaceURL = "https://github.com/tuna-os/corral/releases/download/plugins/marketplace-index.json"
	MarketplaceAPIV2      = "corral.marketplace/v2"
	PluginAPIV1           = "corral.plugin/v1"
	maxArtifactBytes      = 512 << 20
)

// CurrentVersion is set by the root command at startup. Development builds
// leave it unknown, in which case compatibility syntax is validated but the
// range cannot be enforced.
var CurrentVersion = "unknown"

type Build struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature,omitempty"`
}

type Publisher struct {
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	PublicKey string `json:"publicKey,omitempty"`
}

type Entry struct {
	Name              string           `json:"name"`
	Description       string           `json:"description"`
	Version           string           `json:"version"`
	Homepage          string           `json:"homepage,omitempty"`
	License           string           `json:"license,omitempty"`
	PluginAPI         string           `json:"pluginApi,omitempty"`
	Corral            string           `json:"corral,omitempty"`
	Publisher         Publisher        `json:"publisher,omitempty"`
	Capabilities      []string         `json:"capabilities,omitempty"`
	Permissions       []string         `json:"permissions,omitempty"`
	SupportedBackends []string         `json:"supportedBackends,omitempty"`
	Platforms         map[string]Build `json:"platforms"`
	Source            string           `json:"source,omitempty"`
	SourceURL         string           `json:"-"`
	SchemaVersion     string           `json:"-"`
}

type Index struct {
	SchemaVersion string    `json:"schemaVersion,omitempty"`
	Name          string    `json:"name,omitempty"`
	Publisher     Publisher `json:"publisher,omitempty"`
	Plugins       []Entry   `json:"plugins"`
}

type Source struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
	Trusted bool   `json:"trusted,omitempty"`
}

type InstalledState struct {
	Name              string    `json:"name"`
	Version           string    `json:"version"`
	Source            string    `json:"source"`
	SourceURL         string    `json:"sourceUrl"`
	SHA256            string    `json:"sha256"`
	InstalledAt       time.Time `json:"installedAt"`
	Pinned            bool      `json:"pinned"`
	PluginAPI         string    `json:"pluginApi,omitempty"`
	Capabilities      []string  `json:"capabilities,omitempty"`
	Permissions       []string  `json:"permissions,omitempty"`
	SupportedBackends []string  `json:"supportedBackends,omitempty"`
	Publisher         Publisher `json:"publisher,omitempty"`
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var installMu sync.Mutex

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "corral")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "corral")
}
func sourcesPath() string          { return filepath.Join(configDir(), "marketplaces.json") }
func stateDir() string             { return filepath.Join(Dir(), ".installed") }
func statePath(name string) string { return filepath.Join(stateDir(), name+".json") }

func Sources() []Source {
	if raw := strings.TrimSpace(os.Getenv("CORRAL_MARKETPLACE_URLS")); raw != "" {
		var out []Source
		for i, u := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
			if u = strings.TrimSpace(u); u != "" {
				out = append(out, Source{Name: fmt.Sprintf("env-%d", i+1), URL: u, Enabled: true})
			}
		}
		return out
	}
	if legacy := os.Getenv("CORRAL_MARKETPLACE_URL"); legacy != "" {
		return []Source{{Name: "environment", URL: legacy, Enabled: true}}
	}
	out := []Source{{Name: "corral", URL: DefaultMarketplaceURL, Enabled: true, Trusted: true}}
	data, err := os.ReadFile(sourcesPath())
	if err != nil {
		return out
	}
	var custom []Source
	if json.Unmarshal(data, &custom) != nil {
		return out
	}
	return append(out, custom...)
}

func saveSources(sources []Source) error {
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(sourcesPath(), data, 0600)
}
func AddSource(source Source) error {
	if !validName.MatchString(source.Name) {
		return fmt.Errorf("invalid marketplace name %q", source.Name)
	}
	if err := validateSourceURL(source.URL); err != nil {
		return err
	}
	source.Enabled = true
	var custom []Source
	for _, s := range Sources() {
		if s.Name == "corral" && s.URL == DefaultMarketplaceURL {
			continue
		}
		if s.Name == source.Name {
			return fmt.Errorf("marketplace %q already exists", source.Name)
		}
		custom = append(custom, s)
	}
	custom = append(custom, source)
	return saveSources(custom)
}
func RemoveSource(name string) error {
	if name == "corral" {
		return fmt.Errorf("the built-in corral marketplace cannot be removed")
	}
	var out []Source
	found := false
	for _, s := range Sources() {
		if s.Name == "corral" && s.URL == DefaultMarketplaceURL {
			continue
		}
		if s.Name == name {
			found = true
			continue
		}
		out = append(out, s)
	}
	if !found {
		return fmt.Errorf("marketplace %q not found", name)
	}
	return saveSources(out)
}

func SetSourceEnabled(name string, enabled bool) error {
	if name == "corral" {
		return fmt.Errorf("the built-in corral marketplace is always enabled")
	}
	var out []Source
	found := false
	for _, s := range Sources() {
		if s.Name == "corral" && s.URL == DefaultMarketplaceURL {
			continue
		}
		if s.Name == name {
			s.Enabled, found = enabled, true
		}
		out = append(out, s)
	}
	if !found {
		return fmt.Errorf("marketplace %q not found", name)
	}
	return saveSources(out)
}

func marketplaceURL() string {
	s := Sources()
	if len(s) == 0 {
		return DefaultMarketplaceURL
	}
	return s[0].URL
}
func FetchIndex() (*Index, error) {
	return fetchSource(Source{Name: "marketplace", URL: marketplaceURL(), Enabled: true})
}

func ValidateIndexFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 8<<20))
	dec.DisallowUnknownFields()
	var idx Index
	if err := dec.Decode(&idx); err != nil {
		return err
	}
	if idx.SchemaVersion != MarketplaceAPIV2 {
		return fmt.Errorf("schemaVersion must be %q", MarketplaceAPIV2)
	}
	if idx.Name == "" || idx.Publisher.Name == "" {
		return fmt.Errorf("marketplace name and publisher are required")
	}
	seen := map[string]bool{}
	for i := range idx.Plugins {
		idx.Plugins[i].SchemaVersion = idx.SchemaVersion
		if seen[idx.Plugins[i].Name] {
			return fmt.Errorf("duplicate plugin %q", idx.Plugins[i].Name)
		}
		seen[idx.Plugins[i].Name] = true
		if err := idx.Plugins[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}
func FetchAll() (*Index, []error) {
	merged := &Index{SchemaVersion: MarketplaceAPIV2, Name: "merged"}
	names := map[string]string{}
	var errs []error
	for _, source := range Sources() {
		if !source.Enabled {
			continue
		}
		idx, err := fetchSource(source)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source.Name, err))
			continue
		}
		for _, e := range idx.Plugins {
			key := e.Name
			if previous := names[key]; previous != "" {
				errs = append(errs, fmt.Errorf("plugin %q is published by both %s and %s; use source/name to select one", key, previous, source.Name))
			}
			names[key] = source.Name
			merged.Plugins = append(merged.Plugins, e)
		}
	}
	sort.Slice(merged.Plugins, func(i, j int) bool { return merged.Plugins[i].Name < merged.Plugins[j].Name })
	return merged, errs
}
func fetchSource(source Source) (*Index, error) {
	if err := validateSourceURL(source.URL); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(source.URL)
	if err != nil {
		return nil, fmt.Errorf("fetching marketplace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("marketplace returned %s", resp.Status)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	dec.DisallowUnknownFields()
	var idx Index
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("parsing marketplace: %w", err)
	}
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = "v1"
	}
	for i := range idx.Plugins {
		idx.Plugins[i].Source = source.Name
		idx.Plugins[i].SourceURL = source.URL
		idx.Plugins[i].SchemaVersion = idx.SchemaVersion
		if err := idx.Plugins[i].Validate(); err != nil {
			return nil, fmt.Errorf("plugin %d: %w", i, err)
		}
	}
	return &idx, nil
}

func (idx *Index) Find(name string) *Entry {
	source, pluginName, qualified := strings.Cut(name, "/")
	var match *Entry
	for i := range idx.Plugins {
		e := &idx.Plugins[i]
		if qualified {
			if e.Source == source && e.Name == pluginName {
				return e
			}
		} else if e.Name == name {
			if match != nil {
				return nil
			}
			match = e
		}
	}
	return match
}
func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

func (e *Entry) Validate() error {
	if !validName.MatchString(e.Name) {
		return fmt.Errorf("invalid name %q", e.Name)
	}
	if e.Version != "" {
		if _, err := semver.NewVersion(e.Version); err != nil {
			return fmt.Errorf("invalid version %q", e.Version)
		}
	} else if e.SchemaVersion == MarketplaceAPIV2 {
		return fmt.Errorf("%s has no version", e.Name)
	}
	if e.PluginAPI != "" && e.PluginAPI != PluginAPIV1 {
		return fmt.Errorf("unsupported plugin API %q", e.PluginAPI)
	}
	if e.Corral != "" {
		if _, err := semver.NewConstraint(e.Corral); err != nil {
			return fmt.Errorf("invalid corral constraint %q", e.Corral)
		}
	}
	if len(e.Platforms) == 0 {
		return fmt.Errorf("%s has no platform artifacts", e.Name)
	}
	if e.SchemaVersion == MarketplaceAPIV2 {
		if e.Publisher.Name == "" {
			return fmt.Errorf("%s has no publisher", e.Name)
		}
		if e.License == "" {
			return fmt.Errorf("%s has no license", e.Name)
		}
		if len(e.SupportedBackends) == 0 {
			return fmt.Errorf("%s has no supportedBackends declaration", e.Name)
		}
		seenBackends := map[string]bool{}
		for _, backend := range e.SupportedBackends {
			if seenBackends[backend] {
				return fmt.Errorf("%s repeats backend declaration %q", e.Name, backend)
			}
			seenBackends[backend] = true
			switch backend {
			case "qemu", "kubevirt", "incus", "libvirt", "all":
			default:
				return fmt.Errorf("%s has unsupported backend declaration %q", e.Name, backend)
			}
		}
		if seenBackends["all"] && len(seenBackends) != 1 {
			return fmt.Errorf("%s cannot combine backend declaration all with specific backends", e.Name)
		}
		for platform, b := range e.Platforms {
			if len(b.SHA256) != 64 {
				return fmt.Errorf("%s artifact %s requires sha256", e.Name, platform)
			}
			if b.Signature != "" {
				if err := verifyBuildSignature(e.Publisher.PublicKey, b.SHA256, b.Signature); err != nil {
					return fmt.Errorf("%s artifact %s: %w", e.Name, platform, err)
				}
			}
		}
	}
	return nil
}

func verifyBuildSignature(publicKey, digest, signature string) error {
	if publicKey == "" {
		return fmt.Errorf("signature has no publisher public key")
	}
	key, err := base64.RawStdEncoding.DecodeString(publicKey)
	if err != nil {
		key, err = base64.StdEncoding.DecodeString(publicKey)
	}
	if err != nil || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid Ed25519 public key")
	}
	sig, err := base64.RawStdEncoding.DecodeString(signature)
	if err != nil {
		sig, err = base64.StdEncoding.DecodeString(signature)
	}
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid Ed25519 signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(strings.ToLower(digest)), sig) {
		return fmt.Errorf("invalid artifact signature")
	}
	return nil
}

func (e *Entry) Compatible() error {
	if e.Corral == "" || CurrentVersion == "" || CurrentVersion == "unknown" || strings.HasPrefix(CurrentVersion, "git-") {
		return nil
	}
	v, err := semver.NewVersion(strings.TrimPrefix(CurrentVersion, "v"))
	if err != nil {
		return nil
	}
	c, _ := semver.NewConstraint(e.Corral)
	if !c.Check(v) {
		return fmt.Errorf("%s %s requires corral %s (running %s)", e.Name, e.Version, e.Corral, CurrentVersion)
	}
	return nil
}

func (e *Entry) Install() error { return e.InstallPinned(false) }
func (e *Entry) InstallPinned(pin bool) error {
	installMu.Lock()
	defer installMu.Unlock()
	if err := e.Validate(); err != nil {
		return err
	}
	if err := e.Compatible(); err != nil {
		return err
	}
	b, ok := e.Platforms[platformKey()]
	if !ok {
		return fmt.Errorf("%s has no build for %s", e.Name, platformKey())
	}
	if err := validateArtifactURL(b.URL); err != nil {
		return err
	}
	if e.SchemaVersion == MarketplaceAPIV2 && len(b.SHA256) != 64 {
		return fmt.Errorf("marketplace v2 requires a SHA-256 digest")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(b.URL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", e.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading %s: %s", e.Name, resp.Status)
	}
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir(), ".corral-"+e.Name+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxArtifactBytes+1))
	if copyErr != nil {
		tmp.Close()
		return copyErr
	}
	if n > maxArtifactBytes {
		tmp.Close()
		return fmt.Errorf("%s artifact exceeds %d MiB", e.Name, maxArtifactBytes>>20)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if b.SHA256 != "" && !strings.EqualFold(sum, b.SHA256) {
		tmp.Close()
		return fmt.Errorf("checksum mismatch for %s (got %s)", e.Name, sum)
	}
	if err := tmp.Chmod(0755); err != nil {
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
	dst := filepath.Join(Dir(), "corral-"+e.Name)
	backup := dst + ".rollback"
	_ = os.Remove(backup)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("preparing rollback: %w", err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Rename(backup, dst)
		return fmt.Errorf("installing %s: %w", e.Name, err)
	}
	state := InstalledState{Name: e.Name, Version: e.Version, Source: e.Source, SourceURL: e.SourceURL, SHA256: sum, InstalledAt: time.Now().UTC(), Pinned: pin, PluginAPI: e.PluginAPI, Capabilities: e.Capabilities, Permissions: e.Permissions, SupportedBackends: e.SupportedBackends, Publisher: e.Publisher}
	if err := writeState(state); err != nil {
		_ = os.Remove(dst)
		_ = os.Rename(backup, dst)
		return fmt.Errorf("recording installed state (rolled back plugin): %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

func ReadState(name string) (InstalledState, error) {
	data, err := os.ReadFile(statePath(name))
	if err != nil {
		return InstalledState{}, err
	}
	var state InstalledState
	err = json.Unmarshal(data, &state)
	return state, err
}
func writeState(state InstalledState) error {
	if err := os.MkdirAll(stateDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(statePath(state.Name), data, 0600)
}
func SetPinned(name string, pinned bool) error {
	state, err := ReadState(name)
	if err != nil {
		return fmt.Errorf("%s has no installed-state manifest", name)
	}
	state.Pinned = pinned
	return writeState(state)
}
func Remove(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid plugin name")
	}
	dst := filepath.Join(Dir(), "corral-"+name)
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("%s is not installed", name)
	}
	if err := os.Remove(dst); err != nil {
		return err
	}
	_ = os.Remove(statePath(name))
	return nil
}

func validateSourceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid marketplace URL %q", raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return fmt.Errorf("marketplace URL must use HTTPS")
	}
	return nil
}
func validateArtifactURL(raw string) error {
	if err := validateSourceURL(raw); err != nil {
		return fmt.Errorf("artifact: %w", err)
	}
	return nil
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
