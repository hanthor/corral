// Package folder is the fleet hierarchy an operator defines: nestable groups
// that hold instances by canonical reference, so one folder can span backends,
// contexts, and peers (ADR-0008).
//
// Every grouping Corral had before this was either about where an instance runs
// (contexts, nodes) or KubeVirt-only and flat (tags). An application stack
// routinely spans a cluster VM, a local qemu VM, and a CT, so none of them could
// hold one. A folder can, because its members are types.InstanceRef — the one
// identity every backend already produces.
//
// The tree lives in Corral's own state rather than in backend labels: a local
// qemu VM managed through systemd units has nowhere to carry a label, and a
// grouping feature that cannot hold a local VM is not one. The consequence is
// stated in the ADR and worth repeating here: the tree belongs to a Corral, not
// to the instances.
package folder

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// Separator divides folder path segments. A path is the nesting: "prod/web" is
// a child of "prod" because of its name, which makes re-parenting a prefix
// rewrite rather than a tree walk.
const Separator = "/"

// MaxDepth caps nesting. A tree deep enough to need scrolling to read is a tree
// nobody can reason about the blast radius of a bulk action in.
const MaxDepth = 8

// Folder is one group. A folder exists independently of its members, so an
// empty one can be created first and dragged into afterwards.
type Folder struct {
	Path    string              `json:"path" yaml:"path"`
	Members []types.InstanceRef `json:"members,omitempty" yaml:"members,omitempty"`
}

// Tree is the whole hierarchy. It is a value type with methods rather than a
// live object graph: the surfaces load it, change it, and save it, and every
// operation is testable without a store.
type Tree struct {
	folders []Folder
}

// New builds a tree from stored folders, normalising as it goes: paths are
// cleaned, duplicates merged, members deduplicated, and everything sorted so a
// saved document does not churn between writes.
func New(folders []Folder) *Tree {
	t := &Tree{}
	for _, f := range folders {
		path, err := CleanPath(f.Path)
		if err != nil {
			// A path that cannot be cleaned is dropped rather than carried
			// forward as an unreachable folder: it would show in the tree and
			// refuse every operation.
			continue
		}
		t.ensure(path)
		for _, ref := range f.Members {
			if ref.Validate() == nil {
				t.assign(ref, path)
			}
		}
	}
	t.sortFolders()
	return t
}

// Folders returns the tree in stored form, sorted by path.
func (t *Tree) Folders() []Folder {
	out := make([]Folder, 0, len(t.folders))
	for _, f := range t.folders {
		members := make([]types.InstanceRef, len(f.Members))
		copy(members, f.Members)
		out = append(out, Folder{Path: f.Path, Members: members})
	}
	return out
}

// Paths returns every folder path, parents before children.
func (t *Tree) Paths() []string {
	paths := make([]string, 0, len(t.folders))
	for _, f := range t.folders {
		paths = append(paths, f.Path)
	}
	return paths
}

// CleanPath validates and canonicalises a folder path. It is deliberately
// strict: a path is an identifier that ends up in URLs, a YAML document, and a
// ConfigMap key, and "prod /web" or "../etc" have no business in any of them.
func CleanPath(path string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(path), Separator)
	if trimmed == "" {
		return "", fmt.Errorf("folder path is empty")
	}
	segments := strings.Split(trimmed, Separator)
	if len(segments) > MaxDepth {
		return "", fmt.Errorf("folder path %q is more than %d levels deep", path, MaxDepth)
	}
	for i, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return "", fmt.Errorf("folder path %q has an empty segment", path)
		}
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("folder path %q may not contain %q", path, segment)
		}
		if err := validSegment(segment); err != nil {
			return "", err
		}
		segments[i] = segment
	}
	return strings.Join(segments, Separator), nil
}

func validSegment(segment string) error {
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ' ':
		default:
			return fmt.Errorf("folder name %q contains %q; use letters, digits, spaces, '-', '_' or '.'", segment, r)
		}
	}
	return nil
}

// Ensure creates a folder and every missing ancestor, so a nested path can be
// created in one call and the tree never contains an orphan.
func (t *Tree) Ensure(path string) (string, error) {
	clean, err := CleanPath(path)
	if err != nil {
		return "", err
	}
	t.ensure(clean)
	t.sortFolders()
	return clean, nil
}

func (t *Tree) ensure(path string) {
	segments := strings.Split(path, Separator)
	for i := range segments {
		ancestor := strings.Join(segments[:i+1], Separator)
		if t.index(ancestor) < 0 {
			t.folders = append(t.folders, Folder{Path: ancestor})
		}
	}
}

// Remove deletes a folder. Its descendants are removed with it, and its members
// simply become unfoldered — deleting a folder must never delete an instance.
func (t *Tree) Remove(path string) error {
	clean, err := CleanPath(path)
	if err != nil {
		return err
	}
	if t.index(clean) < 0 {
		return fmt.Errorf("no folder %q", clean)
	}
	kept := t.folders[:0]
	for _, f := range t.folders {
		if f.Path == clean || isDescendant(f.Path, clean) {
			continue
		}
		kept = append(kept, f)
	}
	t.folders = kept
	return nil
}

// Move re-parents a folder and everything under it. This is what a drag is, and
// it is a prefix rewrite: "prod/web" moved to "lab/web" carries "prod/web/db"
// along as "lab/web/db".
func (t *Tree) Move(from, to string) error {
	src, err := CleanPath(from)
	if err != nil {
		return err
	}
	dst, err := CleanPath(to)
	if err != nil {
		return err
	}
	if t.index(src) < 0 {
		return fmt.Errorf("no folder %q", src)
	}
	if src == dst {
		return nil
	}
	// Moving a folder inside itself would detach the whole subtree from the
	// root, which is unrecoverable from the UI that asked for it.
	if isDescendant(dst, src) {
		return fmt.Errorf("cannot move %q into its own descendant %q", src, dst)
	}
	if t.index(dst) >= 0 {
		return fmt.Errorf("folder %q already exists", dst)
	}
	if depth(dst)+subtreeHeight(t, src) > MaxDepth {
		return fmt.Errorf("moving %q to %q would nest deeper than %d levels", src, dst, MaxDepth)
	}
	t.ensure(parentOf(dst))
	for i := range t.folders {
		switch {
		case t.folders[i].Path == src:
			t.folders[i].Path = dst
		case isDescendant(t.folders[i].Path, src):
			t.folders[i].Path = dst + strings.TrimPrefix(t.folders[i].Path, src)
		}
	}
	t.sortFolders()
	return nil
}

// Assign puts an instance in a folder, creating the folder if needed. An
// instance belongs to at most one folder — folders are a tree, so the scope of a
// bulk action (and of any policy layered on later) is unambiguous — so this
// removes it from wherever it was.
func (t *Tree) Assign(ref types.InstanceRef, path string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	clean, err := CleanPath(path)
	if err != nil {
		return err
	}
	t.ensure(clean)
	t.assign(ref, clean)
	t.sortFolders()
	return nil
}

func (t *Tree) assign(ref types.InstanceRef, path string) {
	t.unassign(ref)
	if i := t.index(path); i >= 0 {
		t.folders[i].Members = append(t.folders[i].Members, ref)
		sortRefs(t.folders[i].Members)
	}
}

// Unassign takes an instance out of whatever folder holds it. Unassigning one
// that is already unfoldered is not an error: the UI and a prune race.
func (t *Tree) Unassign(ref types.InstanceRef) {
	t.unassign(ref)
}

func (t *Tree) unassign(ref types.InstanceRef) {
	key := ref.String()
	for i := range t.folders {
		kept := t.folders[i].Members[:0]
		for _, m := range t.folders[i].Members {
			if m.String() != key {
				kept = append(kept, m)
			}
		}
		t.folders[i].Members = kept
	}
}

// PathOf returns the folder holding an instance, or "" when it is unfoldered.
func (t *Tree) PathOf(ref types.InstanceRef) string {
	key := ref.String()
	for _, f := range t.folders {
		for _, m := range f.Members {
			if m.String() == key {
				return f.Path
			}
		}
	}
	return ""
}

// PathsByRef maps every foldered instance to its folder, for callers that
// annotate a whole inventory in one pass rather than searching per instance.
func (t *Tree) PathsByRef() map[string]string {
	out := make(map[string]string)
	for _, f := range t.folders {
		for _, m := range f.Members {
			out[m.String()] = f.Path
		}
	}
	return out
}

// Members returns a folder's instances. With recursive set, descendants are
// included — which is what "reboot this folder" means for a folder that has
// children.
func (t *Tree) Members(path string, recursive bool) []types.InstanceRef {
	clean, err := CleanPath(path)
	if err != nil {
		return nil
	}
	var out []types.InstanceRef
	for _, f := range t.folders {
		if f.Path == clean || (recursive && isDescendant(f.Path, clean)) {
			out = append(out, f.Members...)
		}
	}
	sortRefs(out)
	return out
}

// Children returns the folders directly under a path. An empty path returns the
// roots.
func (t *Tree) Children(path string) []string {
	var out []string
	for _, f := range t.folders {
		if parentOf(f.Path) == path {
			out = append(out, f.Path)
		}
	}
	return out
}

// Prune drops members that are not in the live fleet. It is called on write,
// never on read: a stopped VM, or one on a context that is currently
// unreachable, must not silently lose its folder just because a partial fleet
// was listed — partial fleets are normal here.
func (t *Tree) Prune(live []types.InstanceRef) int {
	alive := make(map[string]bool, len(live))
	for _, ref := range live {
		alive[ref.String()] = true
	}
	removed := 0
	for i := range t.folders {
		kept := t.folders[i].Members[:0]
		for _, m := range t.folders[i].Members {
			if alive[m.String()] {
				kept = append(kept, m)
				continue
			}
			removed++
		}
		t.folders[i].Members = kept
	}
	return removed
}

// Missing reports members that no longer resolve to a live instance. The UI
// shows these rather than hiding them, so an operator can see that a folder
// refers to something gone instead of wondering where it went.
func (t *Tree) Missing(live []types.InstanceRef) []types.InstanceRef {
	alive := make(map[string]bool, len(live))
	for _, ref := range live {
		alive[ref.String()] = true
	}
	var out []types.InstanceRef
	for _, f := range t.folders {
		for _, m := range f.Members {
			if !alive[m.String()] {
				out = append(out, m)
			}
		}
	}
	sortRefs(out)
	return out
}

func (t *Tree) index(path string) int {
	for i, f := range t.folders {
		if f.Path == path {
			return i
		}
	}
	return -1
}

func (t *Tree) sortFolders() {
	sort.Slice(t.folders, func(i, j int) bool { return t.folders[i].Path < t.folders[j].Path })
}

func sortRefs(refs []types.InstanceRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
}

// isDescendant reports whether path sits under ancestor. The separator check
// matters: "production" is not a child of "prod".
func isDescendant(path, ancestor string) bool {
	return strings.HasPrefix(path, ancestor+Separator)
}

func parentOf(path string) string {
	if i := strings.LastIndex(path, Separator); i >= 0 {
		return path[:i]
	}
	return ""
}

func depth(path string) int {
	if path == "" {
		return 0
	}
	return len(strings.Split(path, Separator))
}

// subtreeHeight is how many levels a folder spans, itself included.
func subtreeHeight(t *Tree, path string) int {
	height := 1
	base := depth(path)
	for _, f := range t.folders {
		if isDescendant(f.Path, path) {
			if h := depth(f.Path) - base + 1; h > height {
				height = h
			}
		}
	}
	return height
}
