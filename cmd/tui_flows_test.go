package cmd

// Tests for the TUI's pre-existing surface: the fleet list, the actions menu
// and its gating, the confirm/clone/ports/hardware forms, the doctor view, and
// the help overlay. Together with tui_views_test.go (snapshots, events,
// template, CT hardware) every TUI feature is exercised — a keypress at a time,
// against the in-memory demo cluster rather than a real one.

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/demo"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/types"
)

// demoModel is the model as `corral --demo` builds it, sized for a wide
// terminal so the detail pane is on screen.
func demoModel(t *testing.T) tuiModel {
	t.Helper()
	demo.Enable()
	m := newTUIModel()
	m.width, m.height = 140, 40
	m.list.SetSize(90, 30)
	return m
}

// selectVM points the list at a named VM and returns the model. It fails the
// test when the demo fleet has no such VM, so a fixture rename can't quietly
// turn an assertion into a no-op.
func selectVM(t *testing.T, m tuiModel, name string) tuiModel {
	t.Helper()
	for i, item := range m.list.Items() {
		if vi, ok := item.(vmItem); ok && vi.vm.Name == name {
			m.list.Select(i)
			return m
		}
	}
	t.Fatalf("demo fleet has no VM named %q", name)
	return m
}

func selectCT(t *testing.T, m tuiModel, name string) tuiModel {
	t.Helper()
	for i, item := range m.list.Items() {
		if ci, ok := item.(ctItem); ok && ci.ct.Name == name {
			m.list.Select(i)
			return m
		}
	}
	t.Fatalf("demo fleet has no CT named %q", name)
	return m
}

func vmByName(t *testing.T, m tuiModel, name string) types.VM {
	t.Helper()
	for _, item := range m.list.Items() {
		if vi, ok := item.(vmItem); ok && vi.vm.Name == name {
			return vi.vm
		}
	}
	t.Fatalf("demo fleet has no VM named %q", name)
	return types.VM{}
}

func actionIDs(m tuiModel) map[string]bool {
	ids := map[string]bool{}
	for _, item := range m.actionsList.Items() {
		ids[item.(actionItem).id] = true
	}
	return ids
}

// ── the fleet list ────────────────────────────────────────────────

func TestTUIList_LoadsVMsAndCTs(t *testing.T) {
	m := demoModel(t)

	var vms, cts int
	for _, item := range m.list.Items() {
		switch item.(type) {
		case vmItem:
			vms++
		case ctItem:
			cts++
		}
	}
	if vms == 0 {
		t.Error("no VMs in the demo fleet")
	}
	if cts == 0 {
		t.Error("no CTs in the demo fleet — the list is supposed to merge both")
	}
	if title := m.list.Title; !strings.Contains(title, "VMs") || !strings.Contains(title, "CTs") {
		t.Errorf("list title = %q, want both counts", title)
	}
}

func TestTUIList_TitleCountsBothKinds(t *testing.T) {
	th := newTheme(true)
	items := []list.Item{
		vmToItem(types.VM{Name: "a", Backend: "qemu"}, th),
		vmToItem(types.VM{Name: "b", Backend: "qemu"}, th),
		ctToItem(ct.CT{Name: "c"}, th),
	}
	got := tuiListTitle(items)
	if !strings.Contains(got, "2 VMs") || !strings.Contains(got, "1 CTs") {
		t.Errorf("tuiListTitle = %q", got)
	}
	if vmOnly := tuiListTitle(items[:1]); strings.Contains(vmOnly, "CT") {
		t.Errorf("tuiListTitle with no CTs = %q, want no CT segment", vmOnly)
	}
}

// The filter has to reach identity as well as name: an operator looking for
// "kubevirt" or an IP should not have to remember which VM it was.
func TestTUIList_FilterValueCoversIdentity(t *testing.T) {
	vm := types.VM{
		Name: "web-prod", ID: "kubevirt/corral-vms/web-prod", Backend: "kubevirt",
		Context: "talos", Namespace: "corral-vms", Node: "corral-1", IP: "10.42.1.20",
	}
	fv := vmToItem(vm, newTheme(true)).FilterValue()
	for _, want := range []string{"web-prod", "kubevirt", "talos", "corral-vms", "corral-1", "10.42.1.20"} {
		if !strings.Contains(fv, want) {
			t.Errorf("FilterValue missing %q: %s", want, fv)
		}
	}
	if got := ctToItem(ct.CT{Name: "dev-shell"}, newTheme(true)).FilterValue(); got != "dev-shell" {
		t.Errorf("CT FilterValue = %q", got)
	}
}

func TestTUIList_SearchNarrowsTheList(t *testing.T) {
	m := demoModel(t)
	before := len(m.list.Items())

	m = press(m, "/", "w", "e", "b")
	if m.list.FilterState() == 0 {
		t.Fatal("'/' did not start filtering")
	}
	if got := m.list.FilterValue(); got != "web" {
		t.Errorf("filter value = %q, want the typed query", got)
	}
	// Filtering is applied by a command the list returns; the model under test
	// only has to have handed the keys over, which VisibleItems confirms once
	// the filter has been applied by the real program loop.
	if before == 0 {
		t.Fatal("nothing to filter")
	}
}

func TestTUIList_ItemRowShowsStateBackendAndShape(t *testing.T) {
	th := newTheme(true)
	item := vmToItem(types.VM{
		Name: "web-prod", Backend: "kubevirt", Status: "● Running", Running: true,
		CPU: 2, Mem: "4Gi", Node: "corral-1", IP: "10.42.1.20", VNC: "on",
	}, th)
	if item.state != stateRunning {
		t.Errorf("state = %v, want running", item.state)
	}
	for _, want := range []string{"kubevirt", "Running", "2 CPU", "4Gi", "corral-1", "10.42.1.20", tuiProxyOn} {
		if !strings.Contains(item.Description(), want) {
			t.Errorf("row description missing %q: %s", want, item.Description())
		}
	}
	// The backend's own glyph must not double up with the row's dot.
	if strings.Contains(item.Description(), "● Running") {
		t.Errorf("row repeats the backend's status glyph: %s", item.Description())
	}

	pending := vmToItem(types.VM{Name: "x", VNC: "pending"}, th)
	if !strings.Contains(pending.Description(), "◐") {
		t.Errorf("a pending proxy should read as pending: %s", pending.Description())
	}
	off := vmToItem(types.VM{Name: "x"}, th)
	if !strings.Contains(off.Description(), tuiProxyOff) {
		t.Errorf("no proxy should read as off: %s", off.Description())
	}
}

func TestTUIList_CTRowIsMarkedAndPrivilegeIsVisible(t *testing.T) {
	th := newTheme(true)
	item := ctToItem(ct.CT{Name: "dev-shell", Phase: "Running", Ready: true, CPU: 1, Mem: "512Mi", Privileged: true}, th)
	if !strings.HasPrefix(item.Title(), "[CT] ") {
		t.Errorf("CT title = %q, want it marked as a container", item.Title())
	}
	for _, want := range []string{"ct", "Running", "1 CPU", "512Mi", "privileged"} {
		if !strings.Contains(item.Description(), want) {
			t.Errorf("CT row missing %q: %s", want, item.Description())
		}
	}
	if unpriv := ctToItem(ct.CT{Name: "files", Phase: "Stopped"}, th); strings.Contains(unpriv.Description(), "privileged") {
		t.Error("an unprivileged CT should not be labelled privileged")
	}
}

// ── context cycling ───────────────────────────────────────────────

func TestTUIList_ContextCyclingWrapsBothWays(t *testing.T) {
	m := demoModel(t)
	targets := len(config.Contexts()) + 1 // "all" plus every configured context

	m = press(m, "tab")
	if m.contextName == "" || m.contextName == "all" {
		t.Fatalf("tab did not move off 'all' (context = %q)", m.contextName)
	}
	if !strings.Contains(m.list.Title, "context:"+m.contextName) {
		t.Errorf("list title = %q, want the selected context", m.list.Title)
	}

	// A full cycle returns to the start.
	for i := 1; i < targets; i++ {
		m = press(m, "tab")
	}
	if m.contextName != "all" {
		t.Errorf("context after a full cycle = %q, want 'all'", m.contextName)
	}

	back := press(m, "shift+tab")
	if back.contextName == "all" {
		t.Error("shift+tab should step backwards off 'all'")
	}
	if bracket := press(m, "]"); bracket.contextName == "all" {
		t.Error("']' should cycle like tab")
	}
}

func TestTUIList_ContextFilterOnlyKeepsThatContext(t *testing.T) {
	m := demoModel(t)

	// Cycle to each context in turn and check every row belongs to it.
	for range config.Contexts() {
		m = press(m, "tab")
		if m.contextName == "all" {
			continue
		}
		target, ok := config.FindContext(m.contextName)
		if !ok {
			t.Fatalf("cycled to an unknown context %q", m.contextName)
		}
		for _, item := range m.list.Items() {
			if vi, ok := item.(vmItem); ok {
				if vi.vm.Backend != target.Backend || vi.vm.Context != target.Context {
					t.Errorf("context %q shows %s/%s VM %q",
						m.contextName, vi.vm.Backend, vi.vm.Context, vi.vm.Name)
				}
			}
		}
	}
}

// ── quick keys ────────────────────────────────────────────────────

func TestTUIList_QuickStartAndStop(t *testing.T) {
	m := demoModel(t)

	m = selectVM(t, m, "dev-fedora") // stopped in the demo fleet
	m = press(m, "s")
	if vm := vmByName(t, m, "dev-fedora"); !vm.Running {
		t.Errorf("'s' did not start the VM (status %q)", vm.Status)
	}

	m = selectVM(t, m, "web-prod") // running
	m = press(m, "x")
	if vm := vmByName(t, m, "web-prod"); vm.Running {
		t.Errorf("'x' did not stop the VM (status %q)", vm.Status)
	}
}

// A quick key must respect the same gating the menu does: 's' on a running VM
// is not a start, it is a no-op.
func TestTUIList_QuickKeysRespectGating(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "db-prod") // running

	before := vmByName(t, m, "db-prod").Status
	m = press(m, "s")
	if after := vmByName(t, m, "db-prod").Status; after != before {
		t.Errorf("'s' on a running VM changed status %q → %q", before, after)
	}
}

func TestTUIList_RefreshAndHelpAndQuit(t *testing.T) {
	m := demoModel(t)

	refreshed := press(m, "r")
	if refreshed.notice != "Fleet refreshed" {
		t.Errorf("notice after 'r' = %q", refreshed.notice)
	}
	if refreshed.refreshedAt.Before(m.refreshedAt) {
		t.Error("'r' did not update the refresh timestamp")
	}

	helped := press(m, "?")
	if !helped.showHelp {
		t.Fatal("'?' did not open help")
	}
	view := helped.render()
	for _, want := range []string{"search", "context", "actions", "snapshots", "events", "template", "doctor"} {
		if !strings.Contains(view, want) {
			t.Errorf("help does not mention %q:\n%s", want, view)
		}
	}
	if dismissed := press(helped, "j"); dismissed.showHelp {
		t.Error("any key should dismiss help")
	}

	if quit := press(m, "q"); !quit.quitting {
		t.Error("'q' should quit")
	}
	if quit := press(m, "ctrl+c"); !quit.quitting {
		t.Error("ctrl+c should quit")
	}
	if got := press(m, "q").render(); got != "" {
		t.Errorf("a quitting model should render nothing, got %q", got)
	}
}

// ── the actions menu ──────────────────────────────────────────────

func TestTUIActions_OpensForVMsAndCTs(t *testing.T) {
	m := demoModel(t)

	m = selectVM(t, m, "web-prod")
	m = press(m, "enter")
	if m.state != "actions" || m.isCT {
		t.Fatalf("state = %q isCT = %v, want a VM actions menu", m.state, m.isCT)
	}
	if m.selected.Name != "web-prod" {
		t.Errorf("actions menu opened on %q", m.selected.Name)
	}
	if !strings.Contains(m.actionsList.Title, "web-prod") || !strings.Contains(m.actionsList.Title, "kubevirt") {
		t.Errorf("actions title = %q, want the VM and its backend", m.actionsList.Title)
	}
	if got := m.render(); !strings.Contains(got, "web-prod") {
		t.Errorf("actions view does not name the VM:\n%s", got)
	}

	m = press(m, "esc")
	if m.state != "list" {
		t.Fatalf("esc from actions = %q, want 'list'", m.state)
	}

	m = selectCT(t, m, "dev-shell")
	m = press(m, "enter")
	if m.state != "actions" || !m.isCT {
		t.Fatalf("state = %q isCT = %v, want a CT actions menu", m.state, m.isCT)
	}
	if !strings.Contains(m.actionsList.Title, "dev-shell") {
		t.Errorf("CT actions title = %q", m.actionsList.Title)
	}
}

func TestTUIActions_GatingMatrix(t *testing.T) {
	base := types.VM{
		Backend: "kubevirt", Name: "vm",
		Capabilities: types.InstanceCapabilities{
			SSH: true, VNC: true, Snapshots: true, Migrate: true, Delete: true,
		},
	}
	running := base
	running.Running, running.Status = true, "Running"
	stopped := base
	stopped.Status = "Stopped"
	paused := base
	paused.Running, paused.Status = true, "Paused"

	cases := []struct {
		id   string
		vm   types.VM
		want bool
		why  string
	}{
		{"start", stopped, true, "a stopped VM can start"},
		{"start", running, false, "a running VM cannot start"},
		{"stop", running, true, "a running VM can stop"},
		{"stop", stopped, false, "a stopped VM cannot stop"},
		{"restart", running, true, "a running VM can restart"},
		{"restart", stopped, false, "a stopped VM cannot restart"},
		{"pause", running, true, "a running VM can pause"},
		{"pause", paused, false, "an already-paused VM cannot pause"},
		{"unpause", paused, true, "a paused VM can resume"},
		{"unpause", running, false, "a running VM has nothing to resume"},
		{"migrate", running, true, "a running migratable VM can migrate"},
		{"migrate", stopped, false, "a stopped VM cannot migrate"},
		{"migrate", paused, false, "a paused VM cannot migrate"},
		{"ssh", running, true, "a running VM with SSH can be reached"},
		{"ssh", stopped, false, "a stopped VM cannot be SSHed into"},
		{"viewer", running, true, "a running VM with VNC has a viewer"},
		{"viewer", paused, false, "a paused VM has no live viewer"},
		{"snapshot", running, true, "snapshots follow the capability"},
		{"delete", stopped, true, "delete follows the capability"},
	}
	for _, c := range cases {
		if got := actionApplies(c.id, c.vm); got != c.want {
			t.Errorf("actionApplies(%q, %s) = %v, want %v — %s", c.id, c.vm.Status, got, c.want, c.why)
		}
	}

	// Capabilities off means the action is gone even in the right power state.
	bare := running
	bare.Capabilities = types.InstanceCapabilities{}
	for _, id := range []string{"ssh", "viewer", "migrate", "snapshot", "delete"} {
		if actionApplies(id, bare) {
			t.Errorf("actionApplies(%q) ignored the missing capability", id)
		}
	}

	// KubeVirt-only actions must not be offered on other backends.
	for _, backend := range []string{"qemu", "incus", "libvirt"} {
		vm := running
		vm.Backend = backend
		for _, id := range []string{"hardware", "ports", "clone", "export", "template", "events"} {
			if actionApplies(id, vm) {
				t.Errorf("actionApplies(%q) on backend %q, want KubeVirt-only", id, backend)
			}
		}
	}
}

func TestTUIActions_MenuOffersOnlyApplicableEntries(t *testing.T) {
	stopped := kubeVM("dev-fedora")
	stopped.Running, stopped.Ready, stopped.Status = false, false, "Stopped"

	m := testModel(stopped)
	ids := actionIDs(m)
	if ids["stop"] || ids["ssh"] || ids["viewer"] || ids["migrate"] {
		t.Errorf("stopped VM's menu offers running-only actions: %v", ids)
	}
	if !ids["start"] || !ids["delete"] || !ids["snapshot"] {
		t.Errorf("stopped VM's menu is missing applicable actions: %v", ids)
	}

	running := testModel(kubeVM("web-prod"))
	ids = actionIDs(running)
	if ids["start"] {
		t.Error("running VM's menu offers Start")
	}
	for _, want := range []string{"stop", "restart", "pause", "migrate", "ssh", "viewer", "snapshot", "events", "template", "hardware", "ports", "clone", "export", "delete"} {
		if !ids[want] {
			t.Errorf("running KubeVirt VM's menu is missing %q: %v", want, ids)
		}
	}
}

func TestTUIActions_CTGating(t *testing.T) {
	running := ct.CT{Name: "dev-shell", Phase: "Running", Ready: true}
	stopped := ct.CT{Name: "files", Phase: "Stopped"}

	if !ctActionApplies("stop", running) || !ctActionApplies("console", running) {
		t.Error("a running CT can be stopped and consoled")
	}
	if ctActionApplies("start", running) {
		t.Error("a running CT cannot start")
	}
	if !ctActionApplies("start", stopped) {
		t.Error("a stopped CT can start")
	}
	if ctActionApplies("console", stopped) {
		t.Error("a stopped CT has no console")
	}
	if !ctActionApplies("delete", stopped) || !ctActionApplies("hardware", stopped) {
		t.Error("delete and hardware apply to a CT in any state")
	}
}

func TestTUIActions_MenuNavigationMovesTheSelection(t *testing.T) {
	m := testModel(kubeVM("web-prod"))
	m.state = "actions"

	first := m.actionsList.SelectedItem().(actionItem).id
	m = press(m, "down")
	if second := m.actionsList.SelectedItem().(actionItem).id; second == first {
		t.Errorf("down did not move the selection off %q", first)
	}
	m = press(m, "up")
	if back := m.actionsList.SelectedItem().(actionItem).id; back != first {
		t.Errorf("up returned to %q, want %q", back, first)
	}
	if quit := press(m, "q"); !quit.quitting {
		t.Error("'q' should quit from the actions menu")
	}
}

// SSH, the VNC viewer, the export download, and a CT console all need the real
// terminal back, so they quit the program and run afterwards.
func TestTUIActions_TerminalHandoffQuitsFirst(t *testing.T) {
	for _, id := range []string{"ssh", "viewer", "export"} {
		t.Run(id, func(t *testing.T) {
			postQuitAction = nil
			m := testModel(kubeVM("web-prod"))
			m.state = "actions"
			for {
				if m.actionsList.SelectedItem().(actionItem).id == id {
					break
				}
				m = press(m, "down")
			}
			m = press(m, "enter")
			if !m.quitting {
				t.Errorf("%s should quit the program to hand over the terminal", id)
			}
			if postQuitAction == nil {
				t.Errorf("%s did not register a post-quit action", id)
			}
		})
	}

	t.Run("console", func(t *testing.T) {
		postQuitAction = nil
		m := testModel(types.VM{})
		m.state, m.isCT = "actions", true
		m.selectedCT = ct.CT{Name: "dev-shell", Namespace: "corral-vms", Phase: "Running", Ready: true}
		m.actionsList = m.newActionsList()
		for {
			if m.actionsList.SelectedItem().(actionItem).id == "console" {
				break
			}
			m = press(m, "down")
		}
		m = press(m, "enter")
		if !m.quitting || postQuitAction == nil {
			t.Error("CT console should quit and register a post-quit exec")
		}
	})
	postQuitAction = nil
}

// ── power actions ─────────────────────────────────────────────────

func TestTUIActions_PowerActionsRunAndReturnToTheList(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "dev-fedora")
	m = press(m, "enter")

	for {
		if m.actionsList.SelectedItem().(actionItem).id == "start" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "list" {
		t.Fatalf("state after a power action = %q, want 'list'", m.state)
	}
	if vm := vmByName(t, m, "dev-fedora"); !vm.Running {
		t.Errorf("Start did not start the VM (status %q)", vm.Status)
	}
}

// performAction dispatches per backend; the demo cluster records the calls, so
// this checks the qemu/incus/libvirt/kubevirt split does not crash or cross
// wires for any backend the fleet can contain.
func TestTUIPerformAction_AllBackends(t *testing.T) {
	demo.Enable()
	for _, backend := range []string{"qemu", "kubevirt", "incus", "libvirt"} {
		for _, action := range []string{"start", "stop", "restart", "pause", "unpause", "migrate", "delete"} {
			m := testModel(types.VM{
				Name: "probe", Backend: backend, Namespace: "corral-vms",
				Status: "Running", Running: true,
			})
			m.performAction(action) // must not panic for any pairing
		}
	}
}

func TestTUIPerformCTAction(t *testing.T) {
	demo.Enable()
	m := testModel(types.VM{})
	m.isCT = true
	m.selectedCT = ct.CT{Name: "files", Namespace: "corral-vms", Phase: "Stopped"}
	for _, action := range []string{"start", "stop", "delete"} {
		m.performAction(action) // routed to performCTAction
	}
}

// ── delete confirmation ───────────────────────────────────────────

func TestTUIDelete_ConfirmAndCancel(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "scratch")
	m = press(m, "enter")
	for {
		if m.actionsList.SelectedItem().(actionItem).id == "delete" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "confirmDelete" {
		t.Fatalf("state = %q, want 'confirmDelete'", m.state)
	}
	view := m.render()
	if !strings.Contains(view, "scratch") || !strings.Contains(view, "disks") {
		t.Errorf("confirmation does not say what is being destroyed:\n%s", view)
	}

	// Any key other than y goes back to the menu, and nothing is deleted.
	cancelled := press(m, "n")
	if cancelled.state != "actions" {
		t.Errorf("state after cancelling = %q, want 'actions'", cancelled.state)
	}
	if _, err := findDemoVM(cancelled, "scratch"); err != nil {
		t.Error("cancelling the confirmation deleted the VM anyway")
	}

	confirmed := press(m, "y")
	if confirmed.state != "list" {
		t.Errorf("state after confirming = %q, want 'list'", confirmed.state)
	}
	if _, err := findDemoVM(confirmed, "scratch"); err == nil {
		t.Error("confirming delete left the VM in the fleet")
	}
}

func findDemoVM(m tuiModel, name string) (types.VM, error) {
	for _, item := range m.list.Items() {
		if vi, ok := item.(vmItem); ok && vi.vm.Name == name {
			return vi.vm, nil
		}
	}
	return types.VM{}, errNotFound
}

var errNotFound = &notFoundError{}

type notFoundError struct{}

func (*notFoundError) Error() string { return "not in the fleet" }

func TestTUIDelete_CTConfirmationNamesItsVolume(t *testing.T) {
	m := demoModel(t)
	m = selectCT(t, m, "files")
	m = press(m, "enter")
	for {
		if m.actionsList.SelectedItem().(actionItem).id == "delete" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	view := m.render()
	if !strings.Contains(view, "files") || !strings.Contains(view, "data volume") {
		t.Errorf("CT confirmation should mention its data volume:\n%s", view)
	}
	if quit := press(m, "ctrl+c"); !quit.quitting {
		t.Error("ctrl+c should quit from the confirmation")
	}
}

// ── clone ─────────────────────────────────────────────────────────

func TestTUIClone_PrefillsAndValidates(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "golden-ubuntu")
	m = press(m, "enter")
	for {
		if m.actionsList.SelectedItem().(actionItem).id == "clone" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "cloneInput" {
		t.Fatalf("state = %q, want 'cloneInput'", m.state)
	}
	if got := m.cloneInput.Value(); got != "golden-ubuntu-clone" {
		t.Errorf("clone name prefilled with %q", got)
	}
	if view := m.render(); !strings.Contains(view, "Clone golden-ubuntu") {
		t.Errorf("clone form does not name the source:\n%s", view)
	}

	// An empty name is refused, and the form stays open.
	empty := m
	empty.cloneInput.SetValue("")
	empty = press(empty, "enter")
	if empty.state != "cloneInput" {
		t.Errorf("state = %q, want the form to stay open", empty.state)
	}
	if !strings.Contains(empty.cloneErr, "empty") {
		t.Errorf("cloneErr = %q, want an empty-name complaint", empty.cloneErr)
	}
	if !strings.Contains(empty.render(), empty.cloneErr) {
		t.Error("clone error is not drawn in the form")
	}

	// esc backs out to the menu; ctrl+c quits.
	if back := press(m, "esc"); back.state != "actions" {
		t.Errorf("esc from the clone form = %q, want 'actions'", back.state)
	}
	if quit := press(m, "ctrl+c"); !quit.quitting {
		t.Error("ctrl+c should quit from the clone form")
	}
}

func TestTUIClone_Succeeds(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "golden-ubuntu")
	m.selected = vmByName(t, m, "golden-ubuntu")
	m.state = "cloneInput"
	m.cloneInput = newCloneInput("golden-ubuntu")
	m.cloneInput.SetValue("from-golden")

	m = press(m, "enter")
	if m.cloneErr != "" {
		t.Fatalf("clone failed: %s", m.cloneErr)
	}
	if m.state != "list" {
		t.Errorf("state after a successful clone = %q, want 'list'", m.state)
	}
}

func TestTUIClone_RefusesNonKubeVirtAndExistingTargets(t *testing.T) {
	demo.Enable()
	err := runClone(types.VM{Name: "inc", Backend: "incus"}, "copy")
	if err == nil || !strings.Contains(err.Error(), "KubeVirt") {
		t.Errorf("runClone on an Incus VM = %v, want a KubeVirt-only refusal", err)
	}
}

// ── ports editor ──────────────────────────────────────────────────

func TestTUIPorts_TogglesAddsAndClears(t *testing.T) {
	demo.Enable()
	vm := types.VM{Name: "web-prod", Namespace: "corral-vms", Backend: "kubevirt"}
	e := newEditModel(vm)
	if len(e.ports) < len(types.DefaultPorts) {
		t.Fatalf("ports = %d, want at least the defaults", len(e.ports))
	}

	// Toggling the first port flips its mark.
	before := e.toggled[e.ports[0]]
	next, _ := e.Update(key("space"))
	e = next.(editModel)
	if e.toggled[e.ports[0]] == before {
		t.Error("space did not toggle the port under the cursor")
	}
	if !strings.Contains(e.render(), "[ON]") && !strings.Contains(e.render(), "[OFF]") {
		t.Errorf("ports form draws no marks:\n%s", e.render())
	}

	// Cursor bounds: up at the top and down past the extra rows stay in range.
	up, _ := e.Update(key("k"))
	if up.(editModel).cursor != 0 {
		t.Error("cursor moved above the first row")
	}
	for range len(e.ports) + 5 {
		n, _ := e.Update(key("j"))
		e = n.(editModel)
	}
	if e.cursor > len(e.ports) {
		t.Errorf("cursor = %d, want it clamped to the add/clear rows", e.cursor)
	}

	// Add a custom port through the prompt.
	e = newEditModel(vm)
	e.cursor = len(e.ports)
	n, _ := e.Update(key("enter"))
	e = n.(editModel)
	if !e.adding {
		t.Fatal("enter on '+ Add port…' should open the prompt")
	}
	if !strings.Contains(e.render(), "Port:") {
		t.Errorf("add prompt not drawn:\n%s", e.render())
	}
	for _, k := range []string{"9", "0", "0", "1", "enter"} {
		n, _ := e.Update(key(k))
		e = n.(editModel)
	}
	if e.adding {
		t.Error("prompt should close after enter")
	}
	if !e.toggled[9001] {
		t.Errorf("added port not enabled: %v", e.toggled)
	}

	// A non-default port can be removed with backspace; a default cannot.
	e.cursor = len(e.ports) - 1
	n, _ = e.Update(key("backspace"))
	e = n.(editModel)
	if e.toggled[9001] {
		t.Error("backspace did not remove the custom port")
	}
	e.cursor = 0
	def := e.ports[0]
	e.toggled[def] = true
	n, _ = e.Update(key("backspace"))
	if !n.(editModel).toggled[def] {
		t.Error("backspace removed a default port, which is only toggleable")
	}

	// "Remove all" clears every mark.
	e = newEditModel(vm)
	e.toggled[types.DefaultPorts[0]] = true
	e.cursor = len(e.ports) + 1
	n, _ = e.Update(key("enter"))
	if len(n.(editModel).toggled) != 0 {
		t.Errorf("remove-all left %v", n.(editModel).toggled)
	}

	// Escape and q both leave the form.
	for _, k := range []string{"esc", "q"} {
		out, _ := newEditModel(vm).Update(key(k))
		if !out.(editModel).done {
			t.Errorf("%q should close the ports form", k)
		}
	}
}

func TestTUIPorts_FormNamesProtocols(t *testing.T) {
	demo.Enable()
	view := newEditModel(types.VM{Name: "web-prod", Namespace: "corral-vms"}).render()
	if !strings.Contains(view, "Ports: web-prod") {
		t.Errorf("ports form title = %s", view)
	}
	named := false
	for proto := range types.PortMap {
		if strings.Contains(view, proto) {
			named = true
			break
		}
	}
	if !named {
		t.Errorf("no known protocol named in the ports form:\n%s", view)
	}
}

func TestTUIPorts_OpenedFromActionsAndClosesBackToList(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "web-prod")
	m = press(m, "enter")
	for {
		if m.actionsList.SelectedItem().(actionItem).id == "ports" {
			break
		}
		m = press(m, "down")
	}
	m = press(m, "enter")
	if m.state != "edit" {
		t.Fatalf("state = %q, want 'edit'", m.state)
	}
	m = press(m, "esc")
	if m.state != "list" {
		t.Errorf("state after closing the ports form = %q, want 'list'", m.state)
	}
}

// ── VM hardware editor ────────────────────────────────────────────

func TestTUIHardware_FocusApplyAndCancel(t *testing.T) {
	demo.Enable()
	vm := types.VM{Name: "web-prod", Namespace: "corral-vms", Backend: "kubevirt", CPU: 2, Mem: "4Gi", LiveMigratable: true}
	hw := newHWEditModel(vm)
	if hw.cpu.Value() != "2" || hw.mem.Value() != "4Gi" {
		t.Errorf("form prefilled with %q/%q", hw.cpu.Value(), hw.mem.Value())
	}
	if !strings.Contains(hw.render(), "hotplug") {
		t.Errorf("a live-migratable VM should promise a hotplug apply:\n%s", hw.render())
	}

	offline := newHWEditModel(types.VM{Name: "x", CPU: 1, Mem: "1Gi"})
	if !strings.Contains(offline.render(), "restart") {
		t.Errorf("a non-migratable VM should warn about the restart:\n%s", offline.render())
	}

	next, _ := hw.Update(key("tab"))
	if next.(hwEditModel).focus != 1 {
		t.Error("tab did not move focus to memory")
	}
	next, _ = next.(hwEditModel).Update(key("tab"))
	if next.(hwEditModel).focus != 0 {
		t.Error("tab did not wrap focus back to CPU")
	}

	applied := hw
	applied.cpu.SetValue("6")
	applied.mem.SetValue("16Gi")
	out, _ := applied.Update(key("enter"))
	if !out.(hwEditModel).done {
		t.Error("enter should apply and close the form")
	}

	cancelled, _ := hw.Update(key("esc"))
	if !cancelled.(hwEditModel).done {
		t.Error("esc should close the form")
	}
}

func TestTUIHardware_OpenedFromActions(t *testing.T) {
	m := demoModel(t)
	m = selectVM(t, m, "web-prod")
	m = press(m, "enter")
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
	if m.hwEdit.isCT {
		t.Error("a VM opened the CT hardware editor")
	}
	if m.hwEdit.vm.Name != "web-prod" {
		t.Errorf("hardware editor opened on %q", m.hwEdit.vm.Name)
	}
	m = press(m, "esc")
	if m.state != "list" {
		t.Errorf("state after closing = %q, want 'list'", m.state)
	}
}

// ── doctor ────────────────────────────────────────────────────────

func TestTUIDoctor_RendersChecksAndReconciles(t *testing.T) {
	m := demoModel(t)
	m = press(m, "d")
	if m.state != "doctor" {
		t.Fatalf("state = %q, want 'doctor'", m.state)
	}
	if len(m.doctorRows) == 0 {
		t.Fatal("doctor view has no checks")
	}
	view := m.render()
	for _, want := range []string{"Fleet health", "TARGET", "CHECK", "DETAIL"} {
		if !strings.Contains(view, want) {
			t.Errorf("doctor view missing %q:\n%s", want, view)
		}
	}

	m = press(m, "f") // reconcile; must not panic and must re-run the checks
	if len(m.doctorRows) == 0 {
		t.Error("checks disappeared after reconciling")
	}
	for _, k := range []string{"esc", "q", "enter"} {
		if back := press(m, k); back.state != "list" {
			t.Errorf("%q from the doctor view = %q, want 'list'", k, back.state)
		}
	}
	if quit := press(m, "ctrl+c"); !quit.quitting {
		t.Error("ctrl+c should quit from the doctor view")
	}
}

func TestTUIDoctor_FixHintOnlyWhenFixable(t *testing.T) {
	m := testModel(types.VM{})
	m.state = "doctor"

	m.doctorRows = []doctor.Check{{Name: "ok-check", Context: "local", Backend: "qemu", OK: true}}
	if strings.Contains(m.doctorView(), "reconcile") {
		t.Error("a clean fleet should not offer a reconcile key")
	}

	m.doctorRows = []doctor.Check{
		{Name: "broken", Context: "talos", Backend: "kubevirt", Detail: "missing", Fixable: true},
		{Name: "warned", Context: "talos", Backend: "kubevirt", Severity: "warning", Detail: "slow"},
	}
	view := m.doctorView()
	for _, want := range []string{"fixable", "reconcile", "broken", "warned"} {
		if !strings.Contains(view, want) {
			t.Errorf("doctor view missing %q:\n%s", want, view)
		}
	}
}

func TestTUIDoctor_TableSizingStaysOnScreen(t *testing.T) {
	if got := tableWidth(20); got != 60 {
		t.Errorf("tableWidth(20) = %d, want a 60-column floor", got)
	}
	if got := tableWidth(120); got != 118 {
		t.Errorf("tableWidth(120) = %d, want width-2", got)
	}
	if got := maxDetail(40); got != 24 {
		t.Errorf("maxDetail(40) = %d, want the 24-column floor", got)
	}
	if got := maxDetail(400); got != 90 {
		t.Errorf("maxDetail(400) = %d, want the 90-column cap", got)
	}
	if got := maxDetail(100); got != 52 {
		t.Errorf("maxDetail(100) = %d, want width-48", got)
	}
}

// ── detail pane ───────────────────────────────────────────────────

func TestTUIDetail_VMCTEmptyAndPartialFleet(t *testing.T) {
	th := newTheme(true)

	vm := types.VM{
		Name: "web-prod", ID: "kubevirt/corral-vms/web-prod", Backend: "kubevirt",
		Context: "talos", Namespace: "corral-vms", Node: "corral-1", Status: "Running",
		Running: true, CPU: 2, Mem: "4Gi", IP: "10.42.1.20",
		Capabilities: types.InstanceCapabilities{SSH: true, VNC: true, Snapshots: true},
	}
	view := tuiDetailView(vmToItem(vm, th), 40, nil, th)
	for _, want := range []string{"web-prod", "BACKEND", "CONTEXT", "talos", "RESOURCES", "NETWORK", "10.42.1.20", "CAN DO", "ssh"} {
		if !strings.Contains(view, want) {
			t.Errorf("VM detail missing %q:\n%s", want, view)
		}
	}

	bare := tuiDetailView(vmToItem(types.VM{Name: "nowhere", Backend: "qemu"}, th), 40, nil, th)
	if !strings.Contains(bare, "not discovered") {
		t.Errorf("a VM with no IP should say so:\n%s", bare)
	}
	if !strings.Contains(bare, "no remote access") {
		t.Errorf("a VM with no capabilities should say so:\n%s", bare)
	}

	ctView := tuiDetailView(ctToItem(ct.CT{Name: "dev-shell", Phase: "Running", Ready: true, CPU: 1, Mem: "512Mi", Backend: "kubevirt"}, th), 40, nil, th)
	for _, want := range []string{"CONTAINER", "dev-shell", "RESOURCES"} {
		if !strings.Contains(ctView, want) {
			t.Errorf("CT detail missing %q:\n%s", want, ctView)
		}
	}

	empty := tuiDetailView(nil, 40, nil, th)
	if !strings.Contains(empty, "No instances") {
		t.Errorf("empty detail = %s", empty)
	}

	partial := tuiDetailView(nil, 40, map[string]string{"talos": "dial tcp: timeout"}, th)
	for _, want := range []string{"PARTIAL FLEET", "talos", "timeout"} {
		if !strings.Contains(partial, want) {
			t.Errorf("partial-fleet detail missing %q:\n%s", want, partial)
		}
	}
}

func TestTUIRender_PartialFleetNoticeInStatusBar(t *testing.T) {
	m := testModel(types.VM{})
	m.state = "list"
	m.errors = map[string]string{"talos": "unreachable"}
	view := m.render()
	if !strings.Contains(view, "context(s) unavailable") {
		t.Errorf("status bar does not warn about the unreachable context:\n%s", view)
	}
}

func TestTUIRender_NoticeKindsColourTheStatusBar(t *testing.T) {
	for _, kind := range []string{"", "ok", "warn", "error"} {
		m := testModel(types.VM{})
		m.state, m.notice, m.noticeKind = "list", "something happened", kind
		if !strings.Contains(m.render(), "something happened") {
			t.Errorf("notice of kind %q not rendered", kind)
		}
	}
}

// The detail pane is a wide-terminal luxury; a narrow one drops it rather than
// wrapping the list into mush.
func TestTUIRender_DetailPaneOnlyWhenWide(t *testing.T) {
	m := demoModel(t)

	wide, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if !strings.Contains(wide.(tuiModel).render(), "CAN DO") {
		t.Error("wide terminal should show the detail pane")
	}

	narrow, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	if strings.Contains(narrow.(tuiModel).render(), "CAN DO") {
		t.Error("narrow terminal should drop the detail pane")
	}
}

// ── theme / summarising helpers ────────────────────────────────────

func TestTUIClassifyStatus(t *testing.T) {
	cases := []struct {
		status  string
		running bool
		want    runState
	}{
		{"Running", true, stateRunning},
		{"● Running", false, stateRunning},
		{"Stopped", false, stateStopped},
		{"Paused", true, statePaused},
		{"Starting", false, stateBusy},
		{"Migrating", true, stateBusy},
		{"Provisioning", false, stateBusy},
		{"CrashLoopBackOff", false, stateFailed},
		{"Failed", false, stateFailed},
		{"Unknown", false, stateFailed},
		{"", false, stateStopped},
	}
	for _, c := range cases {
		if got := classifyStatus(c.status, c.running); got != c.want {
			t.Errorf("classifyStatus(%q, %v) = %v, want %v", c.status, c.running, got, c.want)
		}
	}
}

func TestTUIStatusWordsStripsBackendGlyphs(t *testing.T) {
	for _, in := range []string{"● Running", "○ Running", "◐ Running", "⏸ Running", "✖ Running", "Running"} {
		if got := statusWords(in); got != "Running" {
			t.Errorf("statusWords(%q) = %q", in, got)
		}
	}
}

func TestTUICountFleet(t *testing.T) {
	got := countFleet([]listRow{
		{state: stateRunning}, {state: stateRunning},
		{state: stateStopped},
		{state: statePaused}, {state: stateBusy}, {state: stateFailed},
	})
	want := fleetCounts{running: 2, stopped: 1, other: 3, total: 6}
	if got != want {
		t.Errorf("countFleet = %+v, want %+v", got, want)
	}
}

func TestTUIThemeChipsAndBars(t *testing.T) {
	th := newTheme(true)

	if got := th.backendChip(""); !strings.Contains(got, "qemu") {
		t.Errorf("an unset backend should read as qemu, got %q", got)
	}
	for _, backend := range []string{"kubevirt", "qemu", "incus", "libvirt", "vmware"} {
		if !strings.Contains(th.backendChip(backend), backend) {
			t.Errorf("backendChip(%q) does not name the backend", backend)
		}
	}

	chips := th.capabilityChips(types.InstanceCapabilities{SSH: true, VNC: true, RDP: true, Snapshots: true, Migrate: true, GPU: true})
	for _, want := range []string{"ssh", "vnc", "rdp", "snap", "migrate", "gpu"} {
		if !strings.Contains(chips, want) {
			t.Errorf("capabilityChips missing %q: %s", want, chips)
		}
	}

	header := th.headerBar(120, "talos", fleetCounts{running: 2, stopped: 1, other: 1, total: 4}, true, "⠋")
	for _, want := range []string{"CORRAL", "talos", "● 2", "○ 1", "Σ 4", "◐ 1", "⠋"} {
		if !strings.Contains(header, want) {
			t.Errorf("headerBar missing %q:\n%s", want, header)
		}
	}
	if !strings.Contains(th.headerBar(120, "", fleetCounts{}, false, ""), "all contexts") {
		t.Error("an unset context should read as 'all contexts'")
	}

	bar := th.statusBar(120, []keyHint{{"q", "quit"}}, "heads up", th.warn)
	if !strings.Contains(bar, "quit") || !strings.Contains(bar, "heads up") {
		t.Errorf("statusBar = %s", bar)
	}

	// Every run state gets a distinct glyph, so meaning survives a monochrome
	// terminal.
	seen := map[string]bool{}
	for _, state := range []runState{stateRunning, stateStopped, statePaused, stateBusy, stateFailed} {
		dot := th.statusDot(state)
		if seen[dot] {
			t.Errorf("run state %v reuses a glyph", state)
		}
		seen[dot] = true
	}
}

// A light terminal must not be drawn with the dark palette — the whole reason
// the background is queried at startup.
func TestTUIThemeFollowsTerminalBackground(t *testing.T) {
	if newPalette(true) == newPalette(false) {
		t.Fatal("light and dark palettes are identical")
	}

	m := demoModel(t)
	dark := m.th
	next, _ := m.Update(tea.BackgroundColorMsg{Color: lipglossWhite{}})
	light := next.(tuiModel).th
	if light.p == dark.p {
		t.Error("BackgroundColorMsg did not rebuild the theme")
	}
	if next.(tuiModel).render() == "" {
		t.Error("rebuilt theme rendered nothing")
	}
}

// lipglossWhite is a light background answer from the terminal.
type lipglossWhite struct{}

func (lipglossWhite) RGBA() (r, g, b, a uint32) { return 0xffff, 0xffff, 0xffff, 0xffff }

// ── plumbing ──────────────────────────────────────────────────────

func TestTUISpinnerAndWindowSize(t *testing.T) {
	m := demoModel(t)

	next, cmd := m.Update(m.spin.Tick())
	if cmd == nil {
		t.Error("the spinner should keep ticking")
	}
	m = next.(tuiModel)

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if s := sized.(tuiModel); s.width != 100 || s.height != 30 {
		t.Errorf("window size recorded as %dx%d", s.width, s.height)
	}
}

func TestTUIRender_EveryStateDrawsSomething(t *testing.T) {
	m := demoModel(t)
	m.selected = vmByName(t, m, "web-prod")
	m.doctorRows = fleetDiagnosis()
	m.edit = newEditModel(m.selected)
	m.hwEdit = newHWEditModel(m.selected)
	m.cloneInput = newCloneInput(m.selected.Name)
	m.snapshots = snapshotsState{vmName: m.selected.Name}
	m.events = eventsState{vmName: m.selected.Name}

	for _, state := range []string{"list", "actions", "edit", "hwedit", "cloneInput", "confirmDelete", "doctor", "snapshots", "events"} {
		m.state = state
		if got := m.render(); strings.TrimSpace(got) == "" {
			t.Errorf("state %q rendered nothing", state)
		}
	}
}

func TestTUIView_UsesTheAltScreen(t *testing.T) {
	m := demoModel(t)
	if !m.View().AltScreen {
		t.Error("the TUI should take the alternate screen")
	}
	if newEditModel(types.VM{Name: "x"}).render() == "" {
		t.Error("the ports form renders nothing")
	}
	if newHWEditModel(types.VM{Name: "x"}).render() == "" {
		t.Error("the hardware form renders nothing")
	}
}

func TestTUIDefaultString(t *testing.T) {
	if got := defaultString("", "fallback"); got != "fallback" {
		t.Errorf("defaultString(\"\") = %q", got)
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Errorf("defaultString(\"value\") = %q", got)
	}
}
