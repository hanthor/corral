package folder

// Storage for the folder tree.
//
// ADR-0008 puts the tree in Corral's own state: the config file locally, and a
// ConfigMap when `corral web` runs in-cluster — the same split image sources
// already use. Store is the seam that makes both possible, and makes the tree
// testable without touching a disk.
//
// Every mutation goes through Update, which loads, changes, and saves under a
// lock. A folder move rewrites many paths at once, so a partial write is the
// one failure mode worth designing against.

import (
	"sync"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
)

// Backend persists the folder document. Implementations are expected to be
// whole-document: Save replaces what Load returned.
type Backend interface {
	Load() ([]Folder, error)
	Save([]Folder) error
}

// Store serialises access to a Backend and hands out trees.
type Store struct {
	mu      sync.Mutex
	backend Backend
}

// NewStore wraps a backend.
func NewStore(b Backend) *Store { return &Store{backend: b} }

// Tree returns the current hierarchy.
func (s *Store) Tree() (*Tree, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	folders, err := s.backend.Load()
	if err != nil {
		return nil, err
	}
	return New(folders), nil
}

// Update applies a change to the tree and saves it. The mutation runs under the
// store's lock, so two concurrent moves cannot interleave into a tree that
// neither caller asked for.
func (s *Store) Update(mutate func(*Tree) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	folders, err := s.backend.Load()
	if err != nil {
		return err
	}
	tree := New(folders)
	if err := mutate(tree); err != nil {
		return err
	}
	return s.backend.Save(tree.Folders())
}

// ── config backend ────────────────────────────────────────────────

// ConfigBackend stores the tree in the corral config file. Members are held as
// instance selectors (the reversible string form of types.InstanceRef), which is
// what keeps pkg/config free of domain types.
type ConfigBackend struct{}

func (ConfigBackend) Load() ([]Folder, error) {
	stored := config.Folders()
	out := make([]Folder, 0, len(stored))
	for _, f := range stored {
		folder := Folder{Path: f.Path}
		for _, selector := range f.Members {
			ref, err := types.ParseInstanceRef(selector)
			if err != nil {
				// A selector Corral can no longer parse is dropped rather than
				// failing the whole load: one bad line must not cost an
				// operator their entire tree.
				continue
			}
			folder.Members = append(folder.Members, ref)
		}
		out = append(out, folder)
	}
	return out, nil
}

func (ConfigBackend) Save(folders []Folder) error {
	stored := make([]config.FolderConfig, 0, len(folders))
	for _, f := range folders {
		entry := config.FolderConfig{Path: f.Path}
		for _, ref := range f.Members {
			entry.Members = append(entry.Members, ref.String())
		}
		stored = append(stored, entry)
	}
	return config.SetFolders(stored)
}

// MemoryBackend is an in-memory tree, for tests and for a surface that wants a
// scratch hierarchy without persisting one.
type MemoryBackend struct {
	mu      sync.Mutex
	folders []Folder
}

func NewMemoryBackend(folders ...Folder) *MemoryBackend {
	return &MemoryBackend{folders: folders}
}

func (m *MemoryBackend) Load() ([]Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return New(m.folders).Folders(), nil
}

func (m *MemoryBackend) Save(folders []Folder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.folders = New(folders).Folders()
	return nil
}
