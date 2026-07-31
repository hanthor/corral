package folder

import (
	"errors"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/types"
)

func ref(backend, context, ns, name string) types.InstanceRef {
	return types.InstanceRef{Backend: backend, Context: context, Namespace: ns, Name: name}
}

// The heterogeneity folders exist for: a cluster VM, a local qemu VM, a CT, and
// an instance on a peer, all holdable by one folder.
var (
	webProd   = ref("kubevirt", "talos", "corral-vms", "web-prod")
	dbProd    = ref("kubevirt", "talos", "corral-vms", "db-prod")
	devFedora = ref("qemu", "local", "", "dev-fedora")
	filesCT   = ref("ct", "talos", "corral-vms", "files")
	peerVM    = types.InstanceRef{Peer: "homelab", Backend: "incus", Context: "lab", Name: "edge"}
)

func paths(t *Tree) string { return strings.Join(t.Paths(), ",") }

// ── paths ─────────────────────────────────────────────────────────

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"prod", "prod", true},
		{"prod/web", "prod/web", true},
		{"/prod/web/", "prod/web", true},
		{"  prod / web  ", "prod/web", true},
		{"App Stack/Web Tier", "App Stack/Web Tier", true},
		{"sla-gold", "sla-gold", true},
		{"v1.2_beta", "v1.2_beta", true},
		{"", "", false},
		{"   ", "", false},
		{"/", "", false},
		{"prod//web", "", false},
		{"prod/../etc", "", false},
		{"prod/.", "", false},
		{"prod/we:b", "", false},
		{"prod/we\tb", "", false},
		{"a/b/c/d/e/f/g/h/i", "", false},
	}
	for _, c := range cases {
		got, err := CleanPath(c.in)
		if c.ok && err != nil {
			t.Errorf("CleanPath(%q) = error %v, want %q", c.in, err, c.want)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("CleanPath(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("CleanPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// "production" must not be treated as a child of "prod".
func TestIsDescendantRespectsSegmentBoundaries(t *testing.T) {
	if isDescendant("production", "prod") {
		t.Error("a name that merely starts with the parent's name is not a child")
	}
	if !isDescendant("prod/web", "prod") {
		t.Error("prod/web is a child of prod")
	}
	if isDescendant("prod", "prod") {
		t.Error("a folder is not its own descendant")
	}
}

// ── creating and removing ─────────────────────────────────────────

func TestEnsureCreatesAncestors(t *testing.T) {
	tree := New(nil)
	if _, err := tree.Ensure("prod/web/frontend"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := paths(tree); got != "prod,prod/web,prod/web/frontend" {
		t.Errorf("paths = %q, want the ancestors created too", got)
	}
	// Ensuring twice is not an error and does not duplicate.
	if _, err := tree.Ensure("prod/web"); err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if got := paths(tree); got != "prod,prod/web,prod/web/frontend" {
		t.Errorf("paths = %q after re-ensuring", got)
	}
	if _, err := tree.Ensure("prod//web"); err == nil {
		t.Error("Ensure accepted an invalid path")
	}
}

func TestRemoveTakesDescendantsAndFreesMembers(t *testing.T) {
	tree := New(nil)
	tree.Ensure("prod/web")
	tree.Assign(webProd, "prod/web")
	tree.Assign(dbProd, "prod")
	tree.Ensure("production") // must survive: not a descendant of "prod"

	if err := tree.Remove("prod"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := paths(tree); got != "production" {
		t.Errorf("paths = %q, want only the unrelated folder left", got)
	}
	// The instances are unfoldered, not deleted — nothing here owns a VM.
	if got := tree.PathOf(webProd); got != "" {
		t.Errorf("web-prod is still in %q after its folder was removed", got)
	}
	if err := tree.Remove("prod"); err == nil {
		t.Error("removing a folder twice should report that it is gone")
	}
}

// ── membership ────────────────────────────────────────────────────

func TestAssignIsSingleParent(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod/web")
	if got := tree.PathOf(webProd); got != "prod/web" {
		t.Fatalf("PathOf = %q, want prod/web", got)
	}

	// Assigning again moves it rather than adding a second membership: folders
	// are a tree, so a bulk action's scope is unambiguous.
	tree.Assign(webProd, "lab")
	if got := tree.PathOf(webProd); got != "lab" {
		t.Errorf("PathOf = %q after reassignment, want lab", got)
	}
	total := 0
	for _, f := range tree.Folders() {
		total += len(f.Members)
	}
	if total != 1 {
		t.Errorf("%d memberships across the tree, want exactly 1", total)
	}
}

func TestAssignHoldsEveryBackendAndPeers(t *testing.T) {
	tree := New(nil)
	for _, r := range []types.InstanceRef{webProd, devFedora, filesCT, peerVM} {
		if err := tree.Assign(r, "stack"); err != nil {
			t.Fatalf("Assign(%s): %v", r, err)
		}
	}
	members := tree.Members("stack", false)
	if len(members) != 4 {
		t.Fatalf("members = %d, want 4 across kubevirt, qemu, ct, and a peer", len(members))
	}
	// A peer's instance keeps its peer, so it stays distinct from a local one
	// with the same name.
	found := false
	for _, m := range members {
		if m.Peer == "homelab" {
			found = true
		}
	}
	if !found {
		t.Error("the peer's instance lost its peer identity")
	}
}

func TestAssignRejectsAnIncompleteRef(t *testing.T) {
	tree := New(nil)
	if err := tree.Assign(types.InstanceRef{Name: "nameless-backend"}, "prod"); err == nil {
		t.Error("Assign accepted a ref with no backend")
	}
	if err := tree.Assign(webProd, ""); err == nil {
		t.Error("Assign accepted an empty path")
	}
}

func TestUnassignIsIdempotent(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod")
	tree.Unassign(webProd)
	if got := tree.PathOf(webProd); got != "" {
		t.Errorf("PathOf = %q after Unassign", got)
	}
	tree.Unassign(webProd) // must not panic or error
	if len(tree.Members("prod", false)) != 0 {
		t.Error("prod still has members")
	}
	// The folder itself survives losing its last member.
	if got := paths(tree); got != "prod" {
		t.Errorf("paths = %q, want the empty folder kept", got)
	}
}

func TestMembersRecursive(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod/web")
	tree.Assign(dbProd, "prod/db")
	tree.Assign(devFedora, "prod")
	tree.Assign(filesCT, "lab")

	if got := len(tree.Members("prod", false)); got != 1 {
		t.Errorf("direct members of prod = %d, want 1", got)
	}
	if got := len(tree.Members("prod", true)); got != 3 {
		t.Errorf("recursive members of prod = %d, want 3", got)
	}
	if got := len(tree.Members("nope", true)); got != 0 {
		t.Errorf("members of a missing folder = %d, want 0", got)
	}
	if got := tree.Members("prod//bad", true); got != nil {
		t.Errorf("members of an invalid path = %v, want nil", got)
	}
}

func TestPathsByRef(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod/web")
	tree.Assign(devFedora, "lab")

	byRef := tree.PathsByRef()
	if byRef[webProd.String()] != "prod/web" {
		t.Errorf("web-prod maps to %q", byRef[webProd.String()])
	}
	if byRef[devFedora.String()] != "lab" {
		t.Errorf("dev-fedora maps to %q", byRef[devFedora.String()])
	}
	if _, ok := byRef[dbProd.String()]; ok {
		t.Error("an unfoldered instance appears in the map")
	}
}

func TestChildren(t *testing.T) {
	tree := New(nil)
	tree.Ensure("prod/web/frontend")
	tree.Ensure("prod/db")
	tree.Ensure("lab")

	if got := strings.Join(tree.Children(""), ","); got != "lab,prod" {
		t.Errorf("roots = %q, want lab,prod", got)
	}
	if got := strings.Join(tree.Children("prod"), ","); got != "prod/db,prod/web" {
		t.Errorf("children of prod = %q", got)
	}
	if got := tree.Children("prod/db"); len(got) != 0 {
		t.Errorf("children of a leaf = %v", got)
	}
}

// ── moving ────────────────────────────────────────────────────────

func TestMoveCarriesTheSubtreeAndItsMembers(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod/web")
	tree.Assign(dbProd, "prod/web/db")
	tree.Ensure("lab")

	if err := tree.Move("prod/web", "lab/web"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := paths(tree); got != "lab,lab/web,lab/web/db,prod" {
		t.Errorf("paths = %q after the move", got)
	}
	if got := tree.PathOf(webProd); got != "lab/web" {
		t.Errorf("web-prod is in %q, want lab/web", got)
	}
	if got := tree.PathOf(dbProd); got != "lab/web/db" {
		t.Errorf("db-prod is in %q, want lab/web/db", got)
	}
}

func TestMoveCreatesMissingParents(t *testing.T) {
	tree := New(nil)
	tree.Ensure("web")
	if err := tree.Move("web", "prod/tier1/web"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := paths(tree); got != "prod,prod/tier1,prod/tier1/web" {
		t.Errorf("paths = %q, want the new ancestors created", got)
	}
}

func TestMoveRefusesTheUnrecoverableCases(t *testing.T) {
	tree := New(nil)
	tree.Ensure("prod/web")
	tree.Ensure("lab")

	if err := tree.Move("prod", "prod/web/inner"); err == nil {
		t.Error("moving a folder into its own descendant should be refused")
	}
	if err := tree.Move("prod", "lab"); err == nil {
		t.Error("moving onto an existing folder should be refused")
	}
	if err := tree.Move("nope", "lab/nope"); err == nil {
		t.Error("moving a folder that does not exist should be refused")
	}
	if err := tree.Move("prod", "prod//web"); err == nil {
		t.Error("moving to an invalid path should be refused")
	}
	// A no-op move is fine.
	if err := tree.Move("lab", "lab"); err != nil {
		t.Errorf("moving a folder onto itself = %v, want no error", err)
	}
	// And the tree survived every refusal intact.
	if got := paths(tree); got != "lab,prod,prod/web" {
		t.Errorf("paths = %q, want the tree unchanged by the refusals", got)
	}
}

func TestMoveRefusesToNestPastTheDepthCap(t *testing.T) {
	tree := New(nil)
	tree.Ensure("a/b/c")                     // a 3-level subtree
	deep := strings.Repeat("x/", MaxDepth-2) // leaves room for 2 more levels
	if _, err := tree.Ensure(deep + "y"); err != nil {
		t.Fatalf("Ensure deep: %v", err)
	}
	if err := tree.Move("a", deep+"y/a"); err == nil {
		t.Error("a move that would nest past MaxDepth should be refused")
	}
}

// ── stale members ─────────────────────────────────────────────────

// A partial fleet is normal in Corral, so a member that did not come back in a
// listing is reported as missing but kept.
func TestMissingReportsWithoutRemoving(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod")
	tree.Assign(dbProd, "prod")

	missing := tree.Missing([]types.InstanceRef{webProd})
	if len(missing) != 1 || missing[0].Name != "db-prod" {
		t.Fatalf("Missing = %v, want just db-prod", missing)
	}
	if got := tree.PathOf(dbProd); got != "prod" {
		t.Errorf("db-prod lost its folder (%q) merely by being absent", got)
	}
}

func TestPruneDropsWhatIsGone(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod")
	tree.Assign(dbProd, "prod")

	if removed := tree.Prune([]types.InstanceRef{webProd}); removed != 1 {
		t.Errorf("Prune removed %d, want 1", removed)
	}
	if got := tree.PathOf(dbProd); got != "" {
		t.Errorf("db-prod survived the prune in %q", got)
	}
	if got := tree.PathOf(webProd); got != "prod" {
		t.Errorf("web-prod was pruned but is live (now %q)", got)
	}
	if removed := tree.Prune([]types.InstanceRef{webProd}); removed != 0 {
		t.Errorf("a second prune removed %d, want 0", removed)
	}
}

// ── normalisation ─────────────────────────────────────────────────

func TestNewNormalisesStoredDocuments(t *testing.T) {
	tree := New([]Folder{
		{Path: "prod/web", Members: []types.InstanceRef{webProd, webProd}},
		{Path: "/prod/", Members: []types.InstanceRef{dbProd}},
		{Path: "prod//broken", Members: []types.InstanceRef{filesCT}},
		{Path: "lab", Members: []types.InstanceRef{{Name: "no-backend"}}},
	})

	// Ancestors implied by a stored child exist; the unusable path is dropped;
	// the tree is sorted.
	if got := paths(tree); got != "lab,prod,prod/web" {
		t.Errorf("paths = %q", got)
	}
	if got := len(tree.Members("prod/web", false)); got != 1 {
		t.Errorf("duplicate member kept: %d members", got)
	}
	if got := tree.PathOf(filesCT); got != "" {
		t.Errorf("a member of an unusable folder was kept in %q", got)
	}
	if got := len(tree.Members("lab", false)); got != 0 {
		t.Errorf("an invalid ref was kept: %v", tree.Members("lab", false))
	}
}

// Folders() must hand back a copy: a caller mutating it cannot reach into the
// tree.
func TestFoldersReturnsACopy(t *testing.T) {
	tree := New(nil)
	tree.Assign(webProd, "prod")

	snapshot := tree.Folders()
	snapshot[0].Members[0] = dbProd
	snapshot[0].Path = "tampered"

	if got := tree.PathOf(webProd); got != "prod" {
		t.Errorf("mutating the returned slice changed the tree (web-prod now %q)", got)
	}
	if got := paths(tree); got != "prod" {
		t.Errorf("paths = %q after tampering with the copy", got)
	}
}

// ── store ─────────────────────────────────────────────────────────

func TestStoreUpdatePersists(t *testing.T) {
	store := NewStore(NewMemoryBackend())

	if err := store.Update(func(tree *Tree) error {
		return tree.Assign(webProd, "prod/web")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	tree, err := store.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if got := tree.PathOf(webProd); got != "prod/web" {
		t.Errorf("PathOf after reload = %q", got)
	}
}

// A mutation that fails leaves the stored tree alone — a half-applied move is
// the failure this design exists to avoid.
func TestStoreUpdateDoesNotSaveOnError(t *testing.T) {
	backend := NewMemoryBackend(Folder{Path: "prod", Members: []types.InstanceRef{webProd}})
	store := NewStore(backend)

	boom := errors.New("boom")
	err := store.Update(func(tree *Tree) error {
		tree.Assign(dbProd, "lab")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want the mutation's own error", err)
	}

	tree, _ := store.Tree()
	if got := tree.PathOf(dbProd); got != "" {
		t.Errorf("a failed update was persisted (db-prod in %q)", got)
	}
	if got := tree.PathOf(webProd); got != "prod" {
		t.Errorf("a failed update disturbed the existing tree (web-prod in %q)", got)
	}
}

func TestStoreSurfacesBackendErrors(t *testing.T) {
	store := NewStore(failingBackend{})
	if _, err := store.Tree(); err == nil {
		t.Error("Tree should surface a load failure")
	}
	if err := store.Update(func(*Tree) error { return nil }); err == nil {
		t.Error("Update should surface a load failure")
	}
}

type failingBackend struct{}

func (failingBackend) Load() ([]Folder, error) { return nil, errors.New("unreadable") }
func (failingBackend) Save([]Folder) error     { return errors.New("unwritable") }

// The config backend round-trips through instance selectors, which is what lets
// pkg/config stay free of domain types.
func TestConfigBackendRoundTripsSelectors(t *testing.T) {
	in := []Folder{{Path: "prod/web", Members: []types.InstanceRef{webProd, peerVM}}}

	// Convert as ConfigBackend.Save would, then read it back as Load does.
	var selectors []string
	for _, ref := range in[0].Members {
		selectors = append(selectors, ref.String())
	}
	var out []types.InstanceRef
	for _, selector := range selectors {
		ref, err := types.ParseInstanceRef(selector)
		if err != nil {
			t.Fatalf("ParseInstanceRef(%q): %v", selector, err)
		}
		out = append(out, ref)
	}

	if len(out) != 2 {
		t.Fatalf("round-tripped %d members, want 2", len(out))
	}
	if out[0] != webProd {
		t.Errorf("round-tripped %+v, want %+v", out[0], webProd)
	}
	if out[1] != peerVM {
		t.Errorf("round-tripped %+v, want %+v — a peer's identity must survive", out[1], peerVM)
	}
}
