package cmd

// Tests for mouse support. The geometry is the risky part, so rowAt is tested
// as a pure function against stated geometry, and the wiring is then driven with
// real click and wheel messages through Update.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/demo"
	"github.com/tuna-os/corral/pkg/types"
)

func click(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func wheel(up bool) tea.MouseWheelMsg {
	b := tea.MouseWheelDown
	if up {
		b = tea.MouseWheelUp
	}
	return tea.MouseWheelMsg{Button: b}
}

func mouse(m tuiModel, msg tea.Msg) tuiModel {
	next, _ := m.Update(msg)
	return next.(tuiModel)
}

// ── geometry ──────────────────────────────────────────────────────

func TestMouseRowAt(t *testing.T) {
	// A fleet list: one header row above it, a two-row title bar, two-row items,
	// ten rows per page, 25 items, 80 columns wide.
	fleet := listGeom{titleHeight: 2, stride: 2, perPage: 10, count: 25, width: 80}

	cases := []struct {
		name      string
		g         listGeom
		x, y, top int
		want      int
		ok        bool
	}{
		{"first row", fleet, 5, 3, 1, 0, true},
		{"first row, second line", fleet, 5, 4, 1, 0, true},
		{"second row", fleet, 5, 5, 1, 1, true},
		{"tenth row on page one", fleet, 5, 21, 1, 9, true},
		{"header", fleet, 5, 0, 1, 0, false},
		{"title bar", fleet, 5, 2, 1, 0, false},
		{"detail pane to the right", fleet, 95, 3, 1, 0, false},
		{"negative column", fleet, -1, 3, 1, 0, false},
		{"past the page", fleet, 5, 23, 1, 0, false},
		{"no title bar", listGeom{stride: 2, perPage: 10, count: 4, width: 40}, 1, 1, 1, 0, true},
		{"single-row stride", listGeom{titleHeight: 2, stride: 1, perPage: 5, count: 5, width: 40}, 1, 4, 0, 2, true},
		{"zero stride is treated as one", listGeom{titleHeight: 0, stride: 0, perPage: 4, count: 4, width: 40}, 1, 2, 0, 2, true},
		{"unset width accepts any column", listGeom{stride: 1, perPage: 4, count: 4}, 999, 0, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := rowAt(c.g, c.x, c.y, c.top)
			if ok != c.ok {
				t.Fatalf("rowAt ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("rowAt = %d, want %d", got, c.want)
			}
		})
	}
}

// A click on the second page is an offset into that page, not into the list.
func TestMouseRowAt_PageOffset(t *testing.T) {
	g := listGeom{titleHeight: 2, stride: 2, page: 2, perPage: 10, count: 25, width: 80}
	got, ok := rowAt(g, 5, 3, 1)
	if !ok || got != 20 {
		t.Fatalf("rowAt on page 2 = %d (ok %v), want 20", got, ok)
	}
	// Page 2 of 25 items only has 5 rows; the sixth is padding.
	if _, ok := rowAt(g, 5, 13, 1); ok {
		t.Error("a row past the last item on the final page should miss")
	}
}

// The measured geometry has to match the list actually on screen; if bubbles
// changes its title padding, this is what catches it.
func TestMouseGeomOfMatchesTheRenderedList(t *testing.T) {
	m := demoModel(t)
	g := geomOf(m.list, vmItemDelegate{}.Height()+vmItemDelegate{}.Spacing())

	if g.stride != 2 {
		t.Errorf("stride = %d, want 2 (two-row items, no spacing)", g.stride)
	}
	if g.width != m.list.Width() {
		t.Errorf("width = %d, want the list's %d", g.width, m.list.Width())
	}
	if g.count != len(m.list.VisibleItems()) {
		t.Errorf("count = %d, want %d", g.count, len(m.list.VisibleItems()))
	}
	// The title occupies whatever the styles say, and the rendered view must
	// have at least that many rows before its first item.
	title := strings.SplitN(m.list.View(), "\n", g.titleHeight+1)
	if len(title) < g.titleHeight+1 {
		t.Fatalf("list view has fewer than %d rows", g.titleHeight+1)
	}
	if !strings.Contains(title[0], "VMs") {
		t.Errorf("row 0 of the list view is not the title: %q", title[0])
	}
	for _, row := range title[1:g.titleHeight] {
		if strings.TrimSpace(row) != "" {
			t.Errorf("row inside the title bar is not blank: %q", row)
		}
	}
}

// rowOf finds the screen row a piece of text is drawn on in a rendered frame.
// Tests that compute a click position from the same constants the code uses
// prove only that the arithmetic is self-consistent; locating the row in the
// actual frame is what pins the click to what the operator sees.
func rowOf(t *testing.T, frame, text string) int {
	t.Helper()
	for i, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, text) {
			return i
		}
	}
	t.Fatalf("%q is not on screen:\n%s", text, frame)
	return -1
}

// ── clicking the fleet list ───────────────────────────────────────

// The anchor test for every offset in tui_mouse.go: click the row a VM is
// actually drawn on in a real frame, and that VM must become the selection. If
// the header grows a line or the list's title padding changes, this fails.
func TestMouseClick_LandsOnTheRowTheFrameDrew(t *testing.T) {
	m := demoModel(t)
	m.list.Select(0)

	// A VM that is not the current selection, so its name appears in the list
	// and not also in the detail pane.
	const target = "dev-fedora"
	row := rowOf(t, m.render(), target)

	m = mouse(m, click(3, row))
	item, ok := m.list.SelectedItem().(vmItem)
	if !ok {
		t.Fatalf("selection is not a VM after clicking row %d", row)
	}
	if item.vm.Name != target {
		t.Errorf("clicking row %d selected %q, want %q", row, item.vm.Name, target)
	}

	// The row below a two-row item is its second line and selects the same VM.
	m.list.Select(0)
	m = mouse(m, click(3, row+1))
	if got := m.list.SelectedItem().(vmItem).vm.Name; got != target {
		t.Errorf("clicking the item's second line selected %q, want %q", got, target)
	}
}

// Same anchor for the actions menu, whose title bar and single-row items have
// their own geometry.
func TestMouseClick_ActionsMenuLandsOnTheRowTheFrameDrew(t *testing.T) {
	demo.Enable()
	m := testModel(kubeVM("web-prod"))
	m.state = "actions"

	row := rowOf(t, m.render(), "Export")
	m = mouse(m, click(2, row))
	if got := m.actionsList.SelectedItem().(actionItem).id; got != "export" {
		t.Errorf("clicking row %d selected %q, want the export action", row, got)
	}
}

// And for the ports form, which draws its own lines.
func TestMouseClick_PortsFormLandsOnTheRowTheFrameDrew(t *testing.T) {
	demo.Enable()
	vm := types.VM{Name: "web-prod", Namespace: "corral-vms", Backend: "kubevirt"}
	m := testModel(vm)
	m.state = "edit"
	m.edit = newEditModel(vm)

	// Pick a port that has a protocol name, so the row is findable in the frame.
	want := m.edit.ports[2]
	label := "port " + strconv.Itoa(want)
	for proto, p := range types.PortMap {
		if p == want {
			label = proto
			break
		}
	}
	row := rowOf(t, m.render(), label)
	m = mouse(m, click(4, row))
	if m.edit.cursor != 2 {
		t.Errorf("clicking row %d (%s) set the cursor to %d, want 2", row, label, m.edit.cursor)
	}
	if !m.edit.toggled[want] {
		t.Errorf("clicking the %s row did not enable port %d", label, want)
	}
}

func TestMouseClick_SelectsARow(t *testing.T) {
	m := demoModel(t)
	m.list.Select(0)
	g := geomOf(m.list, 2)
	thirdRow := headerHeight + g.titleHeight + 2*2

	m = mouse(m, click(3, thirdRow))
	if m.list.Index() != 2 {
		t.Errorf("index after clicking the third row = %d, want 2", m.list.Index())
	}
	if m.state != "list" {
		t.Errorf("a single click changed state to %q", m.state)
	}
}

func TestMouseClick_OutsideTheRowsIsIgnored(t *testing.T) {
	m := demoModel(t)
	m.list.Select(3)
	g := geomOf(m.list, 2)

	for _, c := range []struct {
		name string
		x, y int
	}{
		{"header", 3, 0},
		{"title bar", 3, headerHeight + g.titleHeight - 1},
		{"detail pane", m.list.Width() + 5, headerHeight + g.titleHeight},
	} {
		after := mouse(m, click(c.x, c.y))
		if after.list.Index() != 3 {
			t.Errorf("a click on the %s moved the selection to %d", c.name, after.list.Index())
		}
		if after.state != "list" {
			t.Errorf("a click on the %s changed state to %q", c.name, after.state)
		}
	}
}

func TestMouseClick_RightButtonIsIgnored(t *testing.T) {
	m := demoModel(t)
	m.list.Select(0)
	g := geomOf(m.list, 2)

	m = mouse(m, tea.MouseClickMsg{X: 3, Y: headerHeight + g.titleHeight + 2, Button: tea.MouseRight})
	if m.list.Index() != 0 {
		t.Error("a right click moved the selection")
	}
}

func TestMouseDoubleClick_OpensTheActionsMenu(t *testing.T) {
	m := demoModel(t)
	g := geomOf(m.list, 2)
	row := headerHeight + g.titleHeight // first row

	m = mouse(m, click(3, row))
	if m.state != "list" {
		t.Fatalf("state after one click = %q, want to stay in the list", m.state)
	}
	m = mouse(m, click(3, row))
	if m.state != "actions" {
		t.Fatalf("state after a double click = %q, want 'actions'", m.state)
	}
	// The menu opened on the row that was clicked.
	first := m.list.VisibleItems()[0]
	switch item := first.(type) {
	case vmItem:
		if m.isCT || m.selected.Name != item.vm.Name {
			t.Errorf("actions opened on %q, want %q", m.selected.Name, item.vm.Name)
		}
	case ctItem:
		if !m.isCT || m.selectedCT.Name != item.ct.Name {
			t.Errorf("actions opened on CT %q, want %q", m.selectedCT.Name, item.ct.Name)
		}
	}
}

func TestMouseDoubleClick_OnADifferentRowJustSelects(t *testing.T) {
	m := demoModel(t)
	g := geomOf(m.list, 2)
	first := headerHeight + g.titleHeight

	m = mouse(m, click(3, first))
	m = mouse(m, click(3, first+2)) // a different row
	if m.state != "actions" {
		if m.list.Index() != 1 {
			t.Errorf("index = %d, want the second row selected", m.list.Index())
		}
		return
	}
	t.Error("two clicks on different rows should not count as a double click")
}

func TestMouseDoubleClick_ExpiresAfterTheWindow(t *testing.T) {
	m := demoModel(t)
	g := geomOf(m.list, 2)
	row := headerHeight + g.titleHeight

	m = mouse(m, click(3, row))
	// Age the first click past the window.
	m.lastClickAt = time.Now().Add(-2 * doubleClickWindow)
	m = mouse(m, click(3, row))
	if m.state != "list" {
		t.Errorf("state = %q, want a slow second click to only select", m.state)
	}
}

// A third click must start a fresh pair rather than re-firing.
func TestMouseTripleClick_DoesNotRefire(t *testing.T) {
	m := demoModel(t)
	g := geomOf(m.list, 2)
	row := headerHeight + g.titleHeight

	m = mouse(m, click(3, row))
	m = mouse(m, click(3, row))
	if m.state != "actions" {
		t.Fatalf("state = %q, want 'actions' after the double click", m.state)
	}
	if !m.lastClickAt.IsZero() {
		t.Error("the double click should have consumed its timestamp")
	}
}

// A double click on an empty list has nothing to open, and must not panic.
func TestMouseClick_EmptyListIsSafe(t *testing.T) {
	m := testModel(types.VM{})
	m.state = "list"
	m = mouse(m, click(1, headerHeight))
	m = mouse(m, click(1, headerHeight))
	if m.state != "list" {
		t.Errorf("state = %q, want to stay in an empty list", m.state)
	}
}

// ── clicking the actions menu ─────────────────────────────────────

func TestMouseClick_ActionsMenuSelectsAndRuns(t *testing.T) {
	demo.Enable()
	m := testModel(kubeVM("web-prod"))
	m.state = "actions"

	// Find the row that holds Delete — a destructive action, so this also
	// proves a double click still lands on the confirmation.
	target := -1
	for i, item := range m.actionsList.Items() {
		if item.(actionItem).id == "delete" {
			target = i
		}
	}
	if target < 0 {
		t.Fatal("no delete action in the menu")
	}
	g := geomOf(m.actionsList, actionItemDelegate{}.Height()+actionItemDelegate{}.Spacing())
	y := g.titleHeight + target*g.stride

	m = mouse(m, click(2, y))
	if m.actionsList.Index() != target {
		t.Fatalf("index after the click = %d, want %d", m.actionsList.Index(), target)
	}
	if m.state != "actions" {
		t.Errorf("one click ran the action (state %q)", m.state)
	}

	m = mouse(m, click(2, y))
	if m.state != "confirmDelete" {
		t.Errorf("state after double-clicking Delete = %q, want 'confirmDelete'", m.state)
	}
}

func TestMouseClick_ActionsMenuIgnoresTheTitle(t *testing.T) {
	demo.Enable()
	m := testModel(kubeVM("web-prod"))
	m.state = "actions"
	m.actionsList.Select(1)

	m = mouse(m, click(2, 0))
	if m.actionsList.Index() != 1 {
		t.Error("a click on the menu title moved the selection")
	}
}

// ── clicking the ports form ───────────────────────────────────────

func TestMouseClick_PortsFormTogglesAddsAndClears(t *testing.T) {
	demo.Enable()
	vm := types.VM{Name: "web-prod", Namespace: "corral-vms", Backend: "kubevirt"}
	m := testModel(vm)
	m.state = "edit"
	m.edit = newEditModel(vm)

	const firstRow = 2 // title, blank, then the ports
	port := m.edit.ports[1]
	before := m.edit.toggled[port]
	m = mouse(m, click(4, firstRow+1))
	if m.edit.cursor != 1 {
		t.Errorf("cursor = %d, want the clicked row", m.edit.cursor)
	}
	if m.edit.toggled[port] == before {
		t.Error("clicking a port row did not toggle it")
	}

	// The row after the ports is "+ Add port…", which opens the prompt.
	m = mouse(m, click(4, firstRow+len(m.edit.ports)))
	if !m.edit.adding {
		t.Error("clicking the add row did not open the prompt")
	}

	// And the one after that clears every port.
	m.edit = newEditModel(vm)
	m.edit.toggled[types.DefaultPorts[0]] = true
	m = mouse(m, click(4, firstRow+len(m.edit.ports)+1))
	if len(m.edit.toggled) != 0 {
		t.Errorf("clicking the clear row left %v", m.edit.toggled)
	}

	// A click above the first row or below the last is nothing.
	m.edit = newEditModel(vm)
	m.edit.cursor = 3
	for _, y := range []int{0, 1, firstRow + len(m.edit.ports) + 2} {
		after := mouse(m, click(4, y))
		if after.edit.cursor != 3 {
			t.Errorf("a click at row %d moved the cursor to %d", y, after.edit.cursor)
		}
	}
}

// ── the wheel ─────────────────────────────────────────────────────

func TestMouseWheel_MovesTheFleetCursor(t *testing.T) {
	m := demoModel(t)
	m.list.Select(2)

	m = mouse(m, wheel(true))
	if m.list.Index() != 1 {
		t.Errorf("index after a wheel-up = %d, want 1", m.list.Index())
	}
	m = mouse(m, wheel(false))
	m = mouse(m, wheel(false))
	if m.list.Index() != 3 {
		t.Errorf("index after two wheel-downs = %d, want 3", m.list.Index())
	}
}

func TestMouseWheel_MovesTheActionsCursor(t *testing.T) {
	demo.Enable()
	m := testModel(kubeVM("web-prod"))
	m.state = "actions"
	m.actionsList.Select(0)

	m = mouse(m, wheel(false))
	if m.actionsList.Index() != 1 {
		t.Errorf("index after a wheel-down = %d, want 1", m.actionsList.Index())
	}
}

func TestMouseWheel_MovesTheSnapshotCursorAndClamps(t *testing.T) {
	f := fakeCluster(t)
	f.AddPrefixResponse("kubectl get vmsnapshot", snapshotListJSON(
		snapshotItem("a", "web-prod", "2026-06-03T00:00:00Z", true),
		snapshotItem("b", "web-prod", "2026-06-02T00:00:00Z", true),
	), nil)

	m := modelWithSnapshots(kubeVM("web-prod"))
	m = mouse(m, wheel(true))
	if m.snapshots.cursor != 0 {
		t.Errorf("cursor = %d, want it clamped at the top", m.snapshots.cursor)
	}
	m = mouse(m, wheel(false))
	if m.snapshots.cursor != 1 {
		t.Errorf("cursor = %d, want 1", m.snapshots.cursor)
	}
	m = mouse(m, wheel(false))
	if m.snapshots.cursor != 1 {
		t.Errorf("cursor = %d, want it clamped at the last row", m.snapshots.cursor)
	}
}

func TestMouseWheel_MovesThePortsCursorAndClamps(t *testing.T) {
	demo.Enable()
	vm := types.VM{Name: "web-prod", Namespace: "corral-vms"}
	m := testModel(vm)
	m.state = "edit"
	m.edit = newEditModel(vm)

	m = mouse(m, wheel(true))
	if m.edit.cursor != 0 {
		t.Errorf("cursor = %d, want it clamped at the top", m.edit.cursor)
	}
	last := len(m.edit.ports) + 1
	for range last + 3 {
		m = mouse(m, wheel(false))
	}
	if m.edit.cursor != last {
		t.Errorf("cursor = %d, want it clamped at %d", m.edit.cursor, last)
	}
}

// States with nothing scrollable ignore the wheel rather than absorbing it into
// some other cursor.
func TestMouseWheel_IgnoredWhereNothingScrolls(t *testing.T) {
	m := demoModel(t)
	for _, state := range []string{"doctor", "events", "confirmDelete", "hwedit", "cloneInput"} {
		m.state = state
		if handled := (&m).handleMouse(wheel(false)); handled {
			t.Errorf("state %q claimed to handle a wheel tick", state)
		}
	}
}

func TestMouseWheel_SidewaysButtonsAreIgnored(t *testing.T) {
	m := demoModel(t)
	m.list.Select(2)
	m = mouse(m, tea.MouseWheelMsg{Button: tea.MouseWheelLeft})
	if m.list.Index() != 2 {
		t.Error("a sideways wheel tick moved the cursor")
	}
}

// ── declaration ───────────────────────────────────────────────────

func TestMouseModeIsDeclaredOnTheView(t *testing.T) {
	m := demoModel(t)
	v := m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %v, want cell motion", v.MouseMode)
	}
	if !v.AltScreen {
		t.Error("declaring the mouse should not have dropped the alt screen")
	}
}
