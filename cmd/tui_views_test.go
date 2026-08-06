package cmd

// Tests for the TUI views that reach parity with the web UI: snapshots,
// events, the template mark, and CT hardware scaling.
//
// The views shell out through the same seams the web handler tests use, so a
// scripted shell.Fake stands in for the cluster and every assertion is about
// what the TUI did with the answer — the state it moved to, the command it
// issued, and what it drew.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/demo"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

// ── fixtures ──────────────────────────────────────────────────────

// fakeCluster wires a scriptable runner into every seam the TUI can reach and
// puts the demo cluster back afterwards, so a Fake never leaks into a later
// test in this package (the seams are package-level).
func fakeCluster(t *testing.T) *shell.Fake {
	t.Helper()
	f := shell.NewFake()
	kubevirt.SetDefaultRunner(f)
	kubevirt.SetPackageRunner(f)
	kubevirt.SetApplyRunner(f)
	ct.SetRunner(f)
	incus.SetRunner(f)
	snapshot.SetRunner(f)
	t.Cleanup(func() {
		demo.Enable()
		snapshot.SetRunner(shell.Real{})
	})
	return f
}

// kubeVM is a KubeVirt VM with every capability on — the tests that care about
// gating set the fields they are gating on explicitly.
func kubeVM(name string) types.VM {
	return types.VM{
		Name: name, ID: "kubevirt/corral-vms/" + name, Backend: "kubevirt",
		Namespace: "corral-vms", Status: "Running", Running: true, Ready: true,
		CPU: 2, Mem: "4Gi",
		Capabilities: types.InstanceCapabilities{
			SSH: true, VNC: true, Snapshots: true, Migrate: true, Delete: true,
		},
	}
}

func snapshotListJSON(items ...string) string {
	return fmt.Sprintf(`{"items":[%s]}`, strings.Join(items, ","))
}

func snapshotItem(name, vm, created string, ready bool) string {
	return fmt.Sprintf(
		`{"metadata":{"name":%q,"creationTimestamp":%q},"spec":{"source":{"name":%q}},"status":{"readyToUse":%t}}`,
		name, created, vm, ready)
}

// testModel is a model with an initialized list — the real one comes from
// newTUIModel, which lists the whole fleet; these tests drive a single VM, so
// they build the shell by hand and let refreshList find whatever the fake
// serves.
func testModel(vm types.VM) tuiModel {
	th := newTheme(true)
	m := tuiModel{th: th, width: 120, height: 40, selected: vm, errors: map[string]string{}}
	m.list = list.New(nil, vmItemDelegate{th: th}, 80, 20)
	m.actionsList = m.newActionsList()
	return m
}

// modelWithSnapshots opens the Snapshots view on vm, the way pressing enter
// twice in the fleet list does.
func modelWithSnapshots(vm types.VM) tuiModel {
	m := testModel(vm)
	m.state = "snapshots"
	m.snapshots = newSnapshotsState(vm)
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case " ", "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	default:
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
}

// press feeds keys to the model in order and returns the final state.
func press(m tuiModel, keys ...string) tuiModel {
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(tuiModel)
	}
	return m
}

// calledWith reports whether the fake saw a call whose full command line
// contains every fragment.
func calledWith(f *shell.Fake, fragments ...string) bool {
	for _, c := range f.Calls() {
		line := c.Name + " " + strings.Join(c.Args, " ")
		all := true
		for _, frag := range fragments {
			if !strings.Contains(line, frag) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// ── snapshots: listing ────────────────────────────────────────────

func TestTUISnapshots_ListsCapturesNewestFirst(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("web-snap-old", "web-prod", "2026-01-01T00:00:00Z", true),
		snapshotItem("web-snap-new", "web-prod", "2026-06-01T00:00:00Z", false),
		snapshotItem("other-vm-snap", "db-prod", "2026-06-02T00:00:00Z", true),
	), nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	if m.snapshots.err != "" {
		t.Fatalf("unexpected error: %s", m.snapshots.err)
	}
	if len(m.snapshots.snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2 (another VM's capture must be filtered out)", len(m.snapshots.snaps))
	}
	if got := m.snapshots.snaps[0].Name; got != "web-snap-new" {
		t.Errorf("first snapshot = %q, want the newest (web-snap-new)", got)
	}

	view := m.snapshotsView()
	for _, want := range []string{"Snapshots", "web-prod", "web-snap-new", "web-snap-old", "ready", "pending"} {
		if !strings.Contains(view, want) {
			t.Errorf("snapshots view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "other-vm-snap") {
		t.Error("snapshots view shows another VM's snapshot")
	}
}

func TestTUISnapshots_EmptyAndErrorStates(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	if view := m.snapshotsView(); !strings.Contains(view, "No snapshots yet") {
		t.Errorf("empty snapshots view = %s", view)
	}

	f.Reset()
	f.AddPrefixResponse("kubectl get vmsnapshot", "", errors.New("connection refused"))
	m = modelWithSnapshots(kubeVM("web-prod"))
	if m.snapshots.err == "" {
		t.Fatal("expected a list error to be recorded")
	}
	if view := m.snapshotsView(); !strings.Contains(view, "connection refused") {
		t.Errorf("error not surfaced in view:\n%s", view)
	}
}

// A backend without an adapter must say so, with the remedy the Unsupported
// error carries — not fail silently the way a kubevirt-only code path did.
func TestTUISnapshots_UnsupportedBackendShowsRefusal(t *testing.T) {
	fakeCluster(t)

	vm := types.VM{Name: "mystery", Backend: "vmware", Status: "Running", Running: true}
	m := modelWithSnapshots(vm)
	if m.snapshots.err == "" {
		t.Fatal("expected an Unsupported refusal for an unknown backend")
	}
	view := m.snapshotsView()
	if !strings.Contains(view, "vmware") {
		t.Errorf("refusal does not name the backend:\n%s", view)
	}
}

// The adapter, not pkg/kubevirt, is what the view talks to — so a non-KubeVirt
// backend snapshots from the TUI, which is the whole point of the parity work.
func TestTUISnapshots_UsesBackendAdapter(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("virsh -c qemu:///system snapshot-list", "snap-a\nsnap-b\n", nil)

	vm := types.VM{
		Name: "fedora", Backend: "libvirt", Context: "qemu:///system",
		Status: "Stopped", Capabilities: types.InstanceCapabilities{Snapshots: true},
	}
	m := modelWithSnapshots(vm)
	if m.snapshots.err != "" {
		t.Fatalf("libvirt list failed: %s", m.snapshots.err)
	}
	if len(m.snapshots.snaps) != 2 {
		t.Fatalf("libvirt snapshots = %d, want 2", len(m.snapshots.snaps))
	}
	if !calledWith(f, "virsh", "snapshot-list", "fedora") {
		t.Error("expected the libvirt adapter's virsh snapshot-list")
	}
}

// ── snapshots: create ─────────────────────────────────────────────

func TestTUISnapshots_CreateNamed(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl apply", "", nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	m = press(m, "n")
	if !m.snapshots.naming {
		t.Fatal("'n' should open the name prompt")
	}
	if view := m.snapshotsView(); !strings.Contains(view, "NAME") {
		t.Errorf("naming prompt not drawn:\n%s", view)
	}

	m = press(m, "b", "a", "s", "e", "enter")
	if m.snapshots.naming {
		t.Error("prompt should close after enter")
	}
	if m.snapshots.err != "" {
		t.Fatalf("create failed: %s", m.snapshots.err)
	}
	if !strings.Contains(m.snapshots.notice, "base") {
		t.Errorf("notice = %q, want the created snapshot's name", m.snapshots.notice)
	}
	// Consistency travels with the confirmation, same as the web UI's toast.
	if !strings.Contains(m.snapshots.notice, "consistent") {
		t.Errorf("notice = %q, want the consistency it achieved", m.snapshots.notice)
	}
	last := f.Calls()
	var applied string
	for _, c := range last {
		if c.Stdin != "" {
			applied = c.Stdin
		}
	}
	if !strings.Contains(applied, `"name": "base"`) && !strings.Contains(applied, `"name":"base"`) {
		t.Errorf("applied manifest does not carry the typed name:\n%s", applied)
	}
}

func TestTUISnapshots_CreateWithBlankNameIsAutoNamed(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl apply", "", nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "n", "enter")
	if m.snapshots.err != "" {
		t.Fatalf("auto-named create failed: %s", m.snapshots.err)
	}
	if !strings.Contains(m.snapshots.notice, "web-prod-snap-") {
		t.Errorf("notice = %q, want a generated name", m.snapshots.notice)
	}
}

func TestTUISnapshots_CreateCancelledByEsc(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "n", "x", "esc")
	if m.snapshots.naming {
		t.Error("esc should close the name prompt")
	}
	if m.state != "snapshots" {
		t.Errorf("state = %q, want to stay in the snapshots view", m.state)
	}
	if calledWith(f, "apply") {
		t.Error("esc must not create a snapshot")
	}
	if m.snapshots.name.Value() != "" {
		t.Error("cancelled prompt should be reset")
	}
}

func TestTUISnapshots_CreateErrorIsShown(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl apply", "", errors.New("no volumesnapshotclass"))

	m := press(modelWithSnapshots(kubeVM("web-prod")), "n", "enter")
	if !strings.Contains(m.snapshots.err, "volumesnapshotclass") {
		t.Errorf("err = %q, want the adapter's reason", m.snapshots.err)
	}
}

// ── snapshots: restore and delete ─────────────────────────────────

func TestTUISnapshots_RestoreNeedsConfirmation(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("web-snap", "web-prod", "2026-06-01T00:00:00Z", true)), nil)
	f.AddPrefixResponse("kubectl apply", "", nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "enter")
	if m.snapshots.confirm != "restore" {
		t.Fatalf("confirm = %q, want 'restore'", m.snapshots.confirm)
	}
	if view := m.snapshotsView(); !strings.Contains(view, "Restore") {
		t.Errorf("confirmation not shown:\n%s", view)
	}
	if calledWith(f, "VirtualMachineRestore") {
		t.Fatal("restore must not run before confirmation")
	}

	// Any key other than y cancels.
	cancelled := press(m, "n")
	if cancelled.snapshots.confirm != "" {
		t.Error("a non-y key should clear the confirmation")
	}

	confirmed := press(m, "y")
	if confirmed.snapshots.confirm != "" {
		t.Error("confirmation should clear after y")
	}
	var applied string
	for _, c := range f.Calls() {
		if strings.Contains(c.Stdin, "VirtualMachineRestore") {
			applied = c.Stdin
		}
	}
	if applied == "" {
		t.Fatal("expected a VirtualMachineRestore to be applied")
	}
	if !strings.Contains(applied, "web-snap") {
		t.Errorf("restore does not reference the selected snapshot:\n%s", applied)
	}
	if !strings.Contains(confirmed.snapshots.notice, "restoring") {
		t.Errorf("notice = %q, want the restore to be reported", confirmed.snapshots.notice)
	}
}

func TestTUISnapshots_DeleteNeedsConfirmation(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("web-snap", "web-prod", "2026-06-01T00:00:00Z", true)), nil)
	f.AddPrefixResponse("kubectl delete vmsnapshot", "", nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "x")
	if m.snapshots.confirm != "delete" {
		t.Fatalf("confirm = %q, want 'delete'", m.snapshots.confirm)
	}
	if calledWith(f, "delete", "vmsnapshot") {
		t.Fatal("delete must not run before confirmation")
	}

	m = press(m, "y")
	if !calledWith(f, "delete", "vmsnapshot", "web-snap") {
		t.Error("expected the selected snapshot to be deleted")
	}
	if !strings.Contains(m.snapshots.notice, "deleted") {
		t.Errorf("notice = %q, want the deletion reported", m.snapshots.notice)
	}
}

func TestTUISnapshots_CursorMovesAndClampsAndSelectsTarget(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("snap-a", "web-prod", "2026-06-03T00:00:00Z", true),
		snapshotItem("snap-b", "web-prod", "2026-06-02T00:00:00Z", true),
	), nil)
	f.AddPrefixResponse("kubectl delete vmsnapshot", "", nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	m = press(m, "up") // already at the top
	if m.snapshots.cursor != 0 {
		t.Errorf("cursor = %d, want it clamped at 0", m.snapshots.cursor)
	}
	m = press(m, "down", "down") // only two rows
	if m.snapshots.cursor != 1 {
		t.Errorf("cursor = %d, want it clamped at the last row", m.snapshots.cursor)
	}
	m = press(m, "x", "y")
	if !calledWith(f, "delete", "vmsnapshot", "snap-b") {
		t.Error("delete hit the wrong row — the cursor is not driving the target")
	}
}

// Deleting the last capture must not leave the cursor pointing off the end.
func TestTUISnapshots_CursorSurvivesEmptyList(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("only", "web-prod", "2026-06-01T00:00:00Z", true)), nil)
	f.AddPrefixResponse("kubectl delete vmsnapshot", "", nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)
	m = press(m, "x", "y")
	if m.snapshots.cursor != 0 {
		t.Errorf("cursor = %d after emptying the list, want 0", m.snapshots.cursor)
	}
	if _, ok := m.snapshots.selected(); ok {
		t.Error("no snapshot should be selectable in an empty list")
	}
	if m.snapshotsView() == "" {
		t.Error("emptied snapshots view rendered nothing")
	}
}

// Restore and delete are no-ops with nothing selected, rather than a panic.
func TestTUISnapshots_ActionsOnEmptyListDoNothing(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "enter", "x")
	if m.snapshots.confirm != "" {
		t.Errorf("confirm = %q, want no confirmation with nothing selected", m.snapshots.confirm)
	}
	if calledWith(f, "delete", "vmsnapshot") {
		t.Error("delete ran with nothing selected")
	}
}

func TestTUISnapshots_ReloadRefetches(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	before := len(f.Calls())
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("late", "web-prod", "2026-06-01T00:00:00Z", true)), nil)

	m = press(m, "r")
	if len(f.Calls()) <= before {
		t.Error("'r' did not re-list snapshots")
	}
	if len(m.snapshots.snaps) != 1 {
		t.Errorf("snapshots = %d after reload, want 1", len(m.snapshots.snaps))
	}
}

func TestTUISnapshots_RunningVMIsWarnedBeforeRestore(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("snap", "web-prod", "2026-06-01T00:00:00Z", true)), nil)

	running := modelWithSnapshots(kubeVM("web-prod"))
	if !strings.Contains(running.snapshotsView(), "running") {
		t.Errorf("a running VM should be warned about restores:\n%s", running.snapshotsView())
	}

	stopped := kubeVM("web-prod")
	stopped.Running, stopped.Ready, stopped.Status = false, false, "Stopped"
	if strings.Contains(modelWithSnapshots(stopped).snapshotsView(), "VM is running") {
		t.Error("a stopped VM should not be warned about running")
	}
}

// ── snapshots: navigation ─────────────────────────────────────────

func TestTUISnapshots_OpenedFromActionsAndEscapesBack(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl get vms", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl get vmis", `{"items":[]}`, nil)
	f.AddPrefixResponse("kubectl get", `{"items":[]}`, nil)
	f.AddPrefixResponse("incus", "[]", nil)

	m := testModel(kubeVM("web-prod"))
	m.state = "actions"
	// Walk the actions list to Snapshots and open it.
	for {
		item, ok := m.actionsList.SelectedItem().(actionItem)
		if !ok {
			t.Fatal("actions list has no items")
		}
		if item.id == "snapshot" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "snapshots" {
		t.Fatalf("state = %q, want 'snapshots'", m.state)
	}
	if m.snapshots.vmName != "web-prod" {
		t.Errorf("snapshots view opened on %q", m.snapshots.vmName)
	}

	m = press(m, "esc")
	if m.state != "actions" {
		t.Errorf("state = %q after esc, want 'actions'", m.state)
	}
}

func TestTUISnapshots_CtrlCQuits(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", `{"items":[]}`, nil)

	m := press(modelWithSnapshots(kubeVM("web-prod")), "ctrl+c")
	if !m.quitting {
		t.Error("ctrl+c should quit from the snapshots view")
	}
}

// ── events ────────────────────────────────────────────────────────

func eventsJSON(items ...string) string {
	return fmt.Sprintf(`{"items":[%s]}`, strings.Join(items, ","))
}

func eventItem(kind, name, typ, reason, msg, stamp string) string {
	return fmt.Sprintf(
		`{"type":%q,"reason":%q,"message":%q,"lastTimestamp":%q,"involvedObject":{"kind":%q,"name":%q}}`,
		typ, reason, msg, stamp, kind, name)
}

func TestTUIEvents_RendersWarningsAndFiltersOtherObjects(t *testing.T) {
	f := fakeCluster(t)
	stamp := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	f.AddPrefixResponse("kubectl get events", eventsJSON(
		eventItem("VirtualMachine", "web-prod", "Normal", "Started", "VM started", stamp),
		eventItem("Pod", "virt-launcher-web-prod-abcde", "Warning", "FailedMount",
			"MountVolume.SetUp failed for volume\n\"disk\": timed out", stamp),
		eventItem("VirtualMachine", "db-prod", "Normal", "Started", "other VM", stamp),
	), nil)

	m := testModel(kubeVM("web-prod"))
	m.state = "events"
	m.events = newEventsState(m.selected)
	if m.events.err != "" {
		t.Fatalf("events error: %s", m.events.err)
	}
	if len(m.events.events) != 2 {
		t.Fatalf("events = %d, want 2 (the VM's own plus its launcher pod)", len(m.events.events))
	}

	view := m.eventsView()
	for _, want := range []string{"Events", "web-prod", "Started", "FailedMount", "1m"} {
		if !strings.Contains(view, want) {
			t.Errorf("events view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "other VM") {
		t.Error("events view shows another VM's events")
	}
	// A newline inside a cell would tear the table's borders apart.
	if strings.Contains(view, "MountVolume.SetUp failed for volume\n") {
		t.Error("event message was not flattened to one line")
	}
}

func TestTUIEvents_EmptyAndErrorStates(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get events", `{"items":[]}`, nil)

	m := testModel(kubeVM("web-prod"))
	m.events = newEventsState(m.selected)
	if !strings.Contains(m.eventsView(), "No recent events") {
		t.Errorf("empty events view = %s", m.eventsView())
	}

	f.Reset()
	f.AddPrefixResponse("kubectl get events", "", errors.New("forbidden"))
	m.events = newEventsState(m.selected)
	if m.events.err == "" {
		t.Fatal("expected the events error to be recorded")
	}
	if !strings.Contains(m.eventsView(), "forbidden") {
		t.Errorf("events error not surfaced:\n%s", m.eventsView())
	}
}

func TestTUIEvents_ReloadAndBackNavigation(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get events", `{"items":[]}`, nil)

	m := testModel(kubeVM("web-prod"))
	m.state = "events"
	m.events = newEventsState(m.selected)
	before := len(f.Calls())

	m = press(m, "r")
	if len(f.Calls()) <= before {
		t.Error("'r' did not re-fetch events")
	}

	for _, k := range []string{"esc", "q", "enter"} {
		back := press(m, k)
		if back.state != "actions" {
			t.Errorf("state after %q = %q, want 'actions'", k, back.state)
		}
	}

	if quit := press(m, "ctrl+c"); !quit.quitting {
		t.Error("ctrl+c should quit from the events view")
	}
}

func TestTUIEvents_OpenedFromActions(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get events", `{"items":[]}`, nil)

	m := testModel(kubeVM("web-prod"))
	m.state = "actions"
	for {
		item := m.actionsList.SelectedItem().(actionItem)
		if item.id == "events" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "events" {
		t.Fatalf("state = %q, want 'events'", m.state)
	}
	if m.events.vmName != "web-prod" {
		t.Errorf("events opened on %q", m.events.vmName)
	}
}

// ── template mark ─────────────────────────────────────────────────

func TestTUITemplate_LabelFollowsTheMark(t *testing.T) {
	plain := kubeVM("web-prod")
	if got := templateActionLabel(plain); got != "Make template" {
		t.Errorf("label = %q for an unmarked VM", got)
	}
	marked := plain
	marked.IsTemplate = true
	if got := templateActionLabel(marked); got != "Unmark template" {
		t.Errorf("label = %q for a template", got)
	}

	m := testModel(marked)
	m.state = "actions"
	found := false
	for _, item := range m.actionsList.Items() {
		if a := item.(actionItem); a.id == "template" {
			found = true
			if a.label != "Unmark template" {
				t.Errorf("actions menu shows %q for a template", a.label)
			}
		}
	}
	if !found {
		t.Error("actions menu has no template entry for a KubeVirt VM")
	}
}

func TestTUITemplate_TogglesBothWays(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl label", "", nil)

	m := testModel(kubeVM("golden"))
	m.toggleTemplate()
	if m.noticeKind != "ok" || !strings.Contains(m.notice, "marked as a template") {
		t.Errorf("notice = %q (%s), want a mark confirmation", m.notice, m.noticeKind)
	}
	if !calledWith(f, "label", "golden", "corral.dev/template=true") {
		t.Errorf("expected the template label to be set, calls: %v", f.Calls())
	}

	f.Reset()
	f.AddPrefixResponse("kubectl label", "", nil)
	marked := kubeVM("golden")
	marked.IsTemplate = true
	m = testModel(marked)
	m.toggleTemplate()
	if !strings.Contains(m.notice, "no longer a template") {
		t.Errorf("notice = %q, want an unmark confirmation", m.notice)
	}
	if !calledWith(f, "label", "corral.dev/template-") {
		t.Errorf("expected the template label to be removed, calls: %v", f.Calls())
	}
}

func TestTUITemplate_FailureIsReported(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl label", "", errors.New("vm not found"))

	m := testModel(kubeVM("ghost"))
	m.toggleTemplate()
	if m.noticeKind != "error" || !strings.Contains(m.notice, "vm not found") {
		t.Errorf("notice = %q (%s), want the failure reported", m.notice, m.noticeKind)
	}
}

// ── CT hardware ───────────────────────────────────────────────────

func TestTUICTHardware_ScalesThePod(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl patch", "", nil)
	f.AddPrefixResponse("kubectl get pod", `{"spec":{"containers":[{"name":"ct"}]}}`, nil)
	f.AddPrefixResponse("kubectl annotate", "", nil)
	f.AddPrefixResponse("kubectl get pvc", `{"metadata":{"annotations":{}}}`, nil)

	c := ct.CT{Name: "dev-shell", Namespace: "corral-vms", CPU: 1, Mem: "512Mi", Phase: "Running", Ready: true}
	hw := newCTHWEditModel(c)
	if !hw.isCT {
		t.Fatal("CT hardware editor should be marked as a CT")
	}
	if hw.cpu.Value() != "1" || hw.mem.Value() != "512Mi" {
		t.Errorf("form prefilled with %q/%q, want the CT's current shape", hw.cpu.Value(), hw.mem.Value())
	}
	if !strings.Contains(hw.render(), "dev-shell") {
		t.Error("CT hardware form does not name the CT")
	}
	// The note has to describe pod resizing, not VM hotplug.
	if strings.Contains(hw.render(), "hotplug") {
		t.Errorf("CT form promises VM hotplug:\n%s", hw.render())
	}

	hw.cpu.SetValue("4")
	hw.mem.SetValue("2Gi")
	hw.apply()
	if hw.status != "" {
		t.Fatalf("apply reported %q", hw.status)
	}
	if !calledWith(f, "dev-shell") {
		t.Errorf("no call touched the CT, calls: %v", f.Calls())
	}
}

func TestTUICTHardware_UnchangedValuesDoNotScale(t *testing.T) {
	f := fakeCluster(t)

	c := ct.CT{Name: "files", Namespace: "corral-vms", CPU: 1, Mem: "256Mi"}
	hw := newCTHWEditModel(c)
	hw.apply()
	if len(f.Calls()) != 0 {
		t.Errorf("apply with unchanged values issued %d call(s)", len(f.Calls()))
	}
}

func TestTUICTHardware_FailureKeepsTheFormOpen(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl", "", errors.New("pod resize rejected"))

	c := ct.CT{Name: "files", Namespace: "corral-vms", CPU: 1, Mem: "256Mi"}
	hw := newCTHWEditModel(c)
	hw.cpu.SetValue("8")
	next, _ := hw.Update(key("enter"))
	updated := next.(hwEditModel)
	if updated.done {
		t.Error("a failed scale should keep the form open")
	}
	if updated.status == "" {
		t.Fatal("expected the failure to be recorded")
	}
	if !strings.Contains(updated.render(), updated.status) {
		t.Error("failure not drawn in the form")
	}
	_ = f
}

func TestTUICTHardware_OpenedFromCTActions(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl", `{"items":[]}`, nil)

	c := ct.CT{Name: "dev-shell", Namespace: "corral-vms", CPU: 1, Mem: "512Mi", Phase: "Running", Ready: true}
	m := testModel(types.VM{})
	m.state, m.isCT, m.selectedCT = "actions", true, c
	m.actionsList = m.newActionsList()

	ids := map[string]bool{}
	for _, item := range m.actionsList.Items() {
		ids[item.(actionItem).id] = true
	}
	if !ids["hardware"] {
		t.Fatal("CT actions menu has no hardware entry")
	}
	// CT actions must stay the small set — no VM-only concepts.
	for _, absent := range []string{"migrate", "snapshot", "ports", "clone", "template", "events"} {
		if ids[absent] {
			t.Errorf("CT actions menu offers %q, which a pod has no notion of", absent)
		}
	}

	for {
		if m.actionsList.SelectedItem().(actionItem).id == "hardware" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "hwedit" {
		t.Fatalf("state = %q, want 'hwedit'", m.state)
	}
	if !m.hwEdit.isCT || m.hwEdit.ct.Name != "dev-shell" {
		t.Errorf("hardware editor opened on the wrong target: %+v", m.hwEdit.ct)
	}
}

// ── helpers ───────────────────────────────────────────────────────

func TestTUIHumanAge(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name, stamp, want string
	}{
		{"empty", "", "—"},
		{"seconds", now.Add(-10 * time.Second).Format(time.RFC3339), "10s"},
		{"minutes", now.Add(-5 * time.Minute).Format(time.RFC3339), "5m"},
		{"hours", now.Add(-3 * time.Hour).Format(time.RFC3339), "3h"},
		{"days", now.Add(-50 * time.Hour).Format(time.RFC3339), "2d"},
		{"unparseable passes through", "whenever", "whenever"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanAge(c.stamp); got != c.want {
				t.Errorf("humanAge(%q) = %q, want %q", c.stamp, got, c.want)
			}
		})
	}
}

func TestTUIOneLine(t *testing.T) {
	got := oneLine("failed to mount\n  volume  \"disk\"\n")
	if got != `failed to mount volume "disk"` {
		t.Errorf("oneLine = %q", got)
	}
}

func TestTUIConsistencyLabel(t *testing.T) {
	th := newTheme(true)
	cases := map[snapshot.Consistency]string{
		snapshot.Offline:    "offline",
		snapshot.Filesystem: "filesystem",
		snapshot.Crash:      "crash",
		"":                  "—",
		"weird":             "weird",
	}
	for c, want := range cases {
		if got := consistencyLabel(c, th); !strings.Contains(got, want) {
			t.Errorf("consistencyLabel(%q) = %q, want it to contain %q", c, got, want)
		}
	}
}

// ── the operation contract ────────────────────────────────────────

// The behaviour the contract bought: a backend that cannot do something says so.
// The if/else ladder this replaced did nothing at all for pause off KubeVirt —
// the operator pressed a key and the fleet did not change.
func TestTUIBackendAction_RefusesWithTheBackendNamed(t *testing.T) {
	demo.Enable()

	m := testModel(types.VM{
		Name: "incus-demo-vm", Backend: "incus", Context: "local",
		Status: "Running", Running: true,
	})
	// migrate, not pause: pause is implemented on every backend now, so it no
	// longer demonstrates a refusal. Incus cannot migrate between remotes yet,
	// and the matrix names that gap.
	m.performBackendAction("migrate")
	if m.noticeKind != "error" {
		t.Fatalf("notice kind = %q, want an error", m.noticeKind)
	}
	for _, want := range []string{"incus", "migrate", "backend-parity"} {
		if !strings.Contains(m.notice, want) {
			t.Errorf("refusal %q does not mention %q", m.notice, want)
		}
	}
}

func TestTUIBackendAction_UnknownBackendIsReported(t *testing.T) {
	demo.Enable()

	m := testModel(types.VM{Name: "mystery", Backend: "vmware", Status: "Running", Running: true})
	m.performBackendAction("start")
	if m.noticeKind != "error" || !strings.Contains(m.notice, "vmware") {
		t.Errorf("notice = %q (%s), want an error naming the backend", m.notice, m.noticeKind)
	}
}

// A power action that works reports what happened, rather than leaving the
// operator to infer it from the list refreshing.
func TestTUIBackendAction_ReportsSuccess(t *testing.T) {
	demo.Enable()

	m := testModel(types.VM{
		Name: "dev-fedora", Backend: "kubevirt", Namespace: "corral-vms",
		Status: "Stopped",
	})
	m.performBackendAction("start")
	if m.noticeKind != "ok" {
		t.Fatalf("notice = %q (%s), want a success", m.notice, m.noticeKind)
	}
	if !strings.Contains(m.notice, "dev-fedora") || !strings.Contains(m.notice, "started") {
		t.Errorf("notice = %q, want it to name the VM and what happened", m.notice)
	}
}

// Migration asks the backend's own pre-flight first. The refusal carries the
// backend's reason, which is the whole point of CanMigrate returning one.
func TestTUIBackendAction_MigrateRelaysThePreflight(t *testing.T) {
	demo.Enable()

	m := testModel(types.VM{
		Name: "dev-fedora", Backend: "kubevirt", Namespace: "corral-vms",
		Status: "Stopped", LiveMigratable: false,
	})
	m.performBackendAction("migrate")
	if m.noticeKind != "error" {
		t.Fatalf("notice = %q (%s), want the pre-flight refusal", m.notice, m.noticeKind)
	}
	if !strings.Contains(m.notice, "cannot migrate") {
		t.Errorf("notice = %q, want it to say the migration was refused", m.notice)
	}
	// And the reason, not just the verdict.
	if !strings.Contains(m.notice, "migratable") {
		t.Errorf("notice = %q, want the backend's own reason", m.notice)
	}
}

func TestPastTense(t *testing.T) {
	cases := map[string]string{
		"start": "started", "stop": "stopped", "restart": "restarted",
		"delete": "deleted", "pause": "paused", "unpause": "resumed",
		"migrate": "migrating",
	}
	for action, want := range cases {
		if got := pastTense(action); got != want {
			t.Errorf("pastTense(%q) = %q, want %q", action, got, want)
		}
	}
}
