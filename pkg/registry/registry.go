package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/tuna-os/corral/pkg/types"
)

// Store manages VM registry persistence.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore creates a registry at the default location (~/.local/share/corral/registry.json).
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(home, ".local", "share", "corral", "registry.json")}, nil
}

// NewStoreAt creates a registry at a custom path (useful for testing).
func NewStoreAt(path string) *Store {
	return &Store{path: path}
}

// Get retrieves a VM's registry entry.
func (s *Store) Get(name string) (types.RegistryEntry, bool) {
	reg := s.readAll()
	e, ok := reg[name]
	return e, ok
}

// Set stores a registry entry for a VM.
func (s *Store) Set(name string, entry types.RegistryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg := s.readAll()
	reg[name] = entry
	return s.writeAll(reg)
}

// Remove deletes a VM's registry entry.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg := s.readAll()
	delete(reg, name)
	return s.writeAll(reg)
}

// SetRef stores an entry under its canonical scoped identity. Legacy Set is
// retained for reading existing registries during migration.
func (s *Store) SetRef(ref types.InstanceRef, entry types.RegistryEntry) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	entry.Backend, entry.Context, entry.Peer = ref.Backend, ref.Context, ref.Peer
	return s.Set(ref.String(), entry)
}

func (s *Store) GetRef(ref types.InstanceRef) (types.RegistryEntry, bool) {
	if entry, ok := s.Get(ref.String()); ok {
		return entry, true
	}
	// Backward-compatible fallback only when the legacy entry describes this
	// same target. It is not used to collapse two scoped identities.
	entry, ok := s.Get(ref.Name)
	if !ok || entry.Backend != ref.Backend || (entry.Context != "" && entry.Context != ref.Context) || (entry.Namespace != "" && entry.Namespace != ref.Namespace) {
		return types.RegistryEntry{}, false
	}
	return entry, true
}

func (s *Store) RemoveRef(ref types.InstanceRef) error { return s.Remove(ref.String()) }

// All returns all registry entries.
func (s *Store) All() map[string]types.RegistryEntry {
	return s.readAll()
}

// Names returns all registered VM names.
func (s *Store) Names() []string {
	reg := s.readAll()
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	return names
}

func (s *Store) readAll() map[string]types.RegistryEntry {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return make(map[string]types.RegistryEntry)
	}
	var reg map[string]types.RegistryEntry
	if json.Unmarshal(data, &reg) != nil {
		return make(map[string]types.RegistryEntry)
	}
	return reg
}

func (s *Store) writeAll(reg map[string]types.RegistryEntry) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	// Write+rename keeps readers from observing truncated JSON. The registry can
	// contain cloud-init passwords, so both temporary and final files are 0600.
	tmp, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	return os.Rename(tmpName, s.path)
}
