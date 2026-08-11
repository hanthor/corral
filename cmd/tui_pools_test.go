package cmd

// TUI pool and move tests (#167).
//
// The assertions worth having are the ones about the asymmetry the web UI
// established: pooling commits, moving asks. A terminal that quietly stopped a
// guest because someone pressed enter on a menu row would be a worse tool than
// one with no move at all.

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/move"
	"github.com/tuna-os/corral/pkg/types"
)

// scratchPools points the TUI at an in-memory tree, so no test writes an
// operator's config file.
func scratchPools(t *testing.T, folders ...folder.Folder) *folder.Store {
	t.Helper()
	store := folder.NewStore(folder.NewMemoryBackend(folders...))
	previous := folderStore
	folderStore = func() *folder.Store { return store }
	t.Cleanup(func() { folderStore = previous })
	return store
}

func poolVM() types.VM {
	return types.VM{Name: "web-1", Backend: "kubevirt", Namespace: "default",
		CPU: 2, Mem: "4Gi", Running: true}
}

// ── assigning to a pool ───────────────────────────────────────────

func TestPoolAssign_PutsTheInstanceInTheChosenPool(t *testing.T) {
	store := scratchPools(t, folder.Folder{Path: "prod"})
	vm := poolVM()
	m := testModel(vm)
	m.pools = newPoolAssignState(vm)
	m.state = "pools"

	// row 0 is "(unassigned)", row 1 is "prod".
	if !m.updatePools(key("down")) {
		t.Fatal("moving the cursor closed the screen")
	}
	if m.updatePools(key("enter")) {
		t.Fatal("choosing a pool should close the screen")
	}

	tree, err := store.Tree()
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.PathOf(vm.Ref()); got != "prod" {
		t.Fatalf("pool = %q, want prod", got)
	}
	if !strings.Contains(m.pools.notice, "prod") {
		t.Errorf("notice %q does not say what happened", m.pools.notice)
	}
}

// Pooling touches no instance, so it commits on enter with no confirmation —
// the same reasoning that lets the web UI act on a drop.
func TestPoolAssign_UnassignRemovesMembership(t *testing.T) {
	vm := poolVM()
	store := scratchPools(t, folder.Folder{Path: "prod", Members: []types.InstanceRef{vm.Ref()}})
	m := testModel(vm)
	m.pools = newPoolAssignState(vm)

	if m.updatePools(key("enter")) { // row 0 = "(unassigned)"
		t.Fatal("choosing unassigned should close the screen")
	}
	tree, _ := store.Tree()
	if got := tree.PathOf(vm.Ref()); got != "" {
		t.Fatalf("still in %q after unassigning", got)
	}
}

func TestPoolAssign_CreatesANestedPoolInline(t *testing.T) {
	store := scratchPools(t)
	vm := poolVM()
	m := testModel(vm)
	m.pools = newPoolAssignState(vm)

	// The last row is "+ new pool…".
	for range len(m.pools.choices) - 1 {
		m.updatePools(key("down"))
	}
	m.updatePools(key("enter"))
	if !m.pools.naming {
		t.Fatal("the new-pool prompt did not open")
	}
	for _, r := range "prod/web" {
		m.updatePools(key(string(r)))
	}
	m.updatePools(key("enter"))

	tree, _ := store.Tree()
	if got := tree.PathOf(vm.Ref()); got != "prod/web" {
		t.Fatalf("pool = %q, want the nested path", got)
	}
}

func TestPoolAssign_MarksTheCurrentPool(t *testing.T) {
	vm := poolVM()
	scratchPools(t, folder.Folder{Path: "prod", Members: []types.InstanceRef{vm.Ref()}})
	m := testModel(vm)
	m.pools = newPoolAssignState(vm)

	var marked bool
	for _, choice := range m.pools.choices {
		if choice.value == "prod" && strings.Contains(choice.detail, "current") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("the pool the instance is already in is not marked: %+v", m.pools.choices)
	}
}

// ── moving to another backend ─────────────────────────────────────

// countMoves replaces the executor and records how many times it ran, so a
// test can assert that nothing happened rather than that nothing visibly
// happened.
func countMoves(t *testing.T) *int {
	t.Helper()
	var runs int
	previous := executeMove
	executeMove = func(plan move.Plan) (move.Result, error) {
		runs++
		return move.Result{Destination: plan.Destination}, nil
	}
	t.Cleanup(func() { executeMove = previous })
	return &runs
}

// The asymmetry: choosing a destination shows the plan and stops there. A
// terminal that stopped a guest on enter would be a worse tool than one with no
// move at all.
func TestMove_ChoosingADestinationOnlyShowsThePlan(t *testing.T) {
	scratchPools(t)
	runs := countMoves(t)
	vm := poolVM()
	m := testModel(vm)
	m.pools = newMoveDestinationState(vm)

	if !m.updatePools(key("enter")) {
		t.Fatal("choosing a destination closed the screen instead of showing the plan")
	}
	if m.pools.mode != "plan" {
		t.Fatalf("mode = %q, want plan", m.pools.mode)
	}
	view := m.poolsView()
	for _, want := range []string{"preflight", "export", "ingest"} {
		if !strings.Contains(view, want) {
			t.Errorf("the plan does not show the %q step:\n%s", want, view)
		}
	}
	// Warnings are the point of showing a plan at all.
	if !strings.Contains(view, "MAC") {
		t.Errorf("the plan omits the address-change warning:\n%s", view)
	}
	if *runs != 0 {
		t.Fatalf("choosing a destination ran the move %d times — it must only plan", *runs)
	}
}

// And y on an accepted plan is what actually runs it.
func TestMovePlan_ConfirmRunsTheMoveExactlyOnce(t *testing.T) {
	scratchPools(t)
	runs := countMoves(t)
	vm := poolVM()
	m := testModel(vm)
	m.pools = newMoveDestinationState(vm)
	m.updatePools(key("enter")) // to the plan

	if !m.pools.plan.OK() {
		t.Skipf("no runnable destination in this environment: %v", m.pools.plan.Refusals)
	}
	if m.updatePools(key("y")) {
		t.Fatal("confirming should close the screen")
	}
	if *runs != 1 {
		t.Fatalf("the move ran %d times, want exactly 1", *runs)
	}
	if !strings.Contains(m.pools.notice, "created stopped") {
		t.Errorf("the result does not say the instance is not running yet: %q", m.pools.notice)
	}
}

func TestMove_TheSourceBackendIsNotOfferedAsADestination(t *testing.T) {
	scratchPools(t)
	m := testModel(poolVM())
	m.pools = newMoveDestinationState(poolVM())

	for _, choice := range m.pools.choices {
		if choice.value == "kubevirt" {
			t.Fatal("the instance's own backend was offered — that is `corral migrate`, a different operation")
		}
	}
}

// Incus is a move destination now (image-publish Ingester, #164): it must be
// offered enabled, not greyed out, so operators can move guests onto it.
func TestMove_IncusIsAnEnabledDestination(t *testing.T) {
	scratchPools(t)
	m := testModel(poolVM())
	m.pools = newMoveDestinationState(poolVM())

	var incus poolChoice
	for _, choice := range m.pools.choices {
		if choice.value == "incus" {
			incus = choice
		}
	}
	if incus.value == "" {
		t.Fatal("incus is missing from the destination list entirely")
	}
	if !incus.enabled() {
		t.Fatalf("incus can receive a move (image-publish destination) but was offered disabled: %q", incus.reason)
	}
}

func TestMove_SelectingIncusProceedsToThePlan(t *testing.T) {
	scratchPools(t)
	m := testModel(poolVM())
	m.pools = newMoveDestinationState(poolVM())

	for i, choice := range m.pools.choices {
		if choice.value == "incus" {
			m.pools.cursor = i
		}
	}
	m.updatePools(key("enter"))
	if m.pools.mode != "plan" {
		t.Fatalf("selecting an enabled destination should produce a plan: mode=%q err=%q", m.pools.mode, m.pools.err)
	}
}

// A refused plan has no confirm key, the same way the web dialog has no
// confirm button.
func TestMovePlan_RefusedPlanOffersNoConfirmation(t *testing.T) {
	scratchPools(t)
	vm := poolVM()
	vm.Backend = "qemu" // a guest cannot be moved onto its own backend
	m := testModel(vm)
	m.pools = newMoveDestinationState(vm)
	m.pools.planMove("qemu")

	if m.pools.plan.OK() {
		t.Fatal("expected a refused plan for a backend that cannot export")
	}
	view := m.poolsView()
	if strings.Contains(view, "y move") {
		t.Errorf("a refused plan offered the confirm key:\n%s", view)
	}
	if !strings.Contains(view, "cannot run") {
		t.Errorf("the plan does not say it is refused:\n%s", view)
	}
	// Pressing it anyway must do nothing — and "nothing" means the executor
	// was never reached, not merely that the screen stayed put.
	runs := countMoves(t)
	if !m.updatePools(key("y")) {
		t.Fatal("y closed the screen on a refused plan")
	}
	if *runs != 0 {
		t.Fatalf("a refused plan was executed %d times", *runs)
	}
}

// ── the menu ──────────────────────────────────────────────────────

// move and migrate must be distinguishable in the menu: one stops the guest,
// the other does not, and a mixed-up choice is somebody's production VM.
func TestActionsMenu_MoveAndMigrateAreLabelledDistinctly(t *testing.T) {
	labels := map[string]string{}
	for _, item := range actionsListItems {
		labels[item.id] = item.label
	}
	move, hasMove := labels["move"]
	migrate, hasMigrate := labels["migrate"]
	if !hasMove || !hasMigrate {
		t.Fatalf("both verbs should be offered, got move=%v migrate=%v", hasMove, hasMigrate)
	}
	if !strings.Contains(move, "backend") {
		t.Errorf("move's label %q does not say it crosses backends", move)
	}
	if !strings.Contains(migrate, "live") || !strings.Contains(migrate, "same") {
		t.Errorf("migrate's label %q does not distinguish it from move", migrate)
	}
	if _, ok := labels["pool"]; !ok {
		t.Error("the pool action is not in the menu")
	}
}

func TestVMRow_ShowsThePoolWhenThereIsOne(t *testing.T) {
	vm := poolVM()
	scratchPools(t, folder.Folder{Path: "prod/web", Members: []types.InstanceRef{vm.Ref()}})
	if !strings.Contains(vmToItem(vm, newTheme(true)).display, "prod/web") {
		t.Fatal("the row does not show the instance's pool")
	}
}

// An unorganised fleet should not carry a column of "(none)".
func TestVMRow_UnpooledRowIsUnchanged(t *testing.T) {
	scratchPools(t)
	vm := poolVM()
	display := vmToItem(vm, newTheme(true)).display
	if strings.Contains(display, "none") {
		t.Fatalf("an unpooled row gained a placeholder: %q", display)
	}
}

func TestPoolPicker_CountsReadAsEnglish(t *testing.T) {
	vm := poolVM()
	scratchPools(t,
		folder.Folder{Path: "one", Members: []types.InstanceRef{vm.Ref()}},
		folder.Folder{Path: "none"},
	)
	m := testModel(vm)
	m.pools = newPoolAssignState(vm)

	for _, choice := range m.pools.choices {
		switch choice.value {
		case "one":
			if !strings.HasPrefix(choice.detail, "1 instance ") && choice.detail != "1 instance · current" {
				t.Errorf("single member reads %q", choice.detail)
			}
		case "none":
			if !strings.HasPrefix(choice.detail, "0 instances") {
				t.Errorf("empty pool reads %q", choice.detail)
			}
		}
	}
}
