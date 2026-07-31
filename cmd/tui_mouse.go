package cmd

// Mouse support.
//
// Bubble Tea v2 declares the mouse on the View alongside the alt screen (see
// tuiModel.View), and delivers clicks and wheel ticks as messages carrying cell
// coordinates. What it does not provide is hit-testing: bubbles/list has no
// notion of "which row is at row Y", so the geometry is computed here from the
// pieces that own it — the header's height, the list's own title bar, its
// delegate's row stride, and which page the list is showing.
//
// That geometry is the whole risk in this file, which is why it is one small
// value type and one pure function rather than arithmetic scattered through the
// Update switch: if a header grows a line, one constant moves, and the tests
// state the geometry outright instead of inferring it.

import (
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// headerHeight is how many rows the header bar occupies above the fleet list.
// theme.headerBar joins horizontally, so it is one line.
const headerHeight = 1

// doubleClickWindow is how close two clicks on the same row have to be to read
// as "open this", so a click selects and a double click drills in — the same
// feel as the web UI's tree, instead of making the operator reach for Enter.
const doubleClickWindow = 400 * time.Millisecond

// listGeom is everything hit-testing needs to know about a rendered list.
type listGeom struct {
	titleHeight int // rows the title bar spends before the first item
	stride      int // rows per item, including the delegate's spacing
	page        int // page currently on screen
	perPage     int // items drawn per page
	count       int // items in the list
	width       int // columns the list occupies; clicks to the right miss it
}

// geomOf measures a list. The title height comes from the real styles rather
// than a guess: bubbles pads a blank line under the title by default, and a
// theme is free to change that.
func geomOf(l list.Model, stride int) listGeom {
	titleHeight := 0
	if l.ShowTitle() {
		titleHeight = lipgloss.Height(l.Styles.TitleBar.Render(l.Styles.Title.Render(l.Title)))
	}
	return listGeom{
		titleHeight: titleHeight,
		stride:      stride,
		page:        l.Paginator.Page,
		perPage:     l.Paginator.PerPage,
		count:       len(l.VisibleItems()),
		width:       l.Width(),
	}
}

// rowAt maps a mouse position to an item index, with top the row the list
// itself starts on. It reports false when the click landed somewhere that is
// not an item: the title, the padding under the last row, or off to the side in
// the detail pane.
func rowAt(g listGeom, x, y, top int) (int, bool) {
	if x < 0 || (g.width > 0 && x >= g.width) {
		return 0, false
	}
	stride := g.stride
	if stride < 1 {
		stride = 1
	}
	row := y - top - g.titleHeight
	if row < 0 {
		return 0, false
	}
	row /= stride
	if g.perPage > 0 && row >= g.perPage {
		return 0, false
	}
	index := g.page*max(g.perPage, 0) + row
	if index >= g.count {
		return 0, false
	}
	return index, true
}

// handleMouse routes a click or a wheel tick to whatever is on screen. It
// returns false when the event means nothing in this state, so Update can leave
// the model alone instead of redrawing for a stray movement.
func (m *tuiModel) handleMouse(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		return m.handleWheel(msg)
	case tea.MouseClickMsg:
		return m.handleClick(msg)
	}
	return false
}

func (m *tuiModel) handleWheel(msg tea.MouseWheelMsg) bool {
	up := msg.Button == tea.MouseWheelUp
	down := msg.Button == tea.MouseWheelDown
	if !up && !down {
		return false
	}

	switch m.state {
	case "list":
		if up {
			m.list.CursorUp()
		} else {
			m.list.CursorDown()
		}
	case "actions":
		if up {
			m.actionsList.CursorUp()
		} else {
			m.actionsList.CursorDown()
		}
	case "snapshots":
		// The snapshots table is drawn by lipgloss/table, whose row offsets are
		// border-dependent; the wheel moves the cursor without needing to know
		// where a row sits on screen.
		if up && m.snapshots.cursor > 0 {
			m.snapshots.cursor--
		} else if down && m.snapshots.cursor < len(m.snapshots.snaps)-1 {
			m.snapshots.cursor++
		}
	case "edit":
		if up && m.edit.cursor > 0 {
			m.edit.cursor--
		} else if down && m.edit.cursor < len(m.edit.ports)+1 {
			m.edit.cursor++
		}
	default:
		return false
	}
	return true
}

func (m *tuiModel) handleClick(msg tea.MouseClickMsg) bool {
	if msg.Button != tea.MouseLeft {
		return false
	}

	switch m.state {
	case "list":
		index, ok := rowAt(geomOf(m.list, vmItemDelegate{}.Height()+vmItemDelegate{}.Spacing()), msg.X, msg.Y, headerHeight)
		if !ok {
			return false
		}
		repeat := m.registerClick(index)
		m.list.Select(index)
		if repeat {
			m.openActionsForSelection()
		}
		return true

	case "actions":
		index, ok := rowAt(geomOf(m.actionsList, actionItemDelegate{}.Height()+actionItemDelegate{}.Spacing()), msg.X, msg.Y, 0)
		if !ok {
			return false
		}
		repeat := m.registerClick(index)
		m.actionsList.Select(index)
		if repeat {
			// A double click runs the action, and a destructive one still has
			// to pass its confirmation — runSelectedAction is the same path
			// Enter takes.
			m.runSelectedAction()
		}
		return true

	case "edit":
		// The ports form draws its own lines: the title, a blank, then one row
		// per port followed by the add and clear rows.
		const portsFirstRow = 2
		row := msg.Y - portsFirstRow
		if row < 0 || row > len(m.edit.ports)+1 {
			return false
		}
		m.edit.cursor = row
		m.edit.activateCursor()
		return true
	}
	return false
}

// registerClick reports whether this click completes a double click on the same
// row, and records it either way.
func (m *tuiModel) registerClick(index int) bool {
	repeat := index == m.lastClickRow && !m.lastClickAt.IsZero() &&
		time.Since(m.lastClickAt) < doubleClickWindow
	m.lastClickRow, m.lastClickAt = index, time.Now()
	if repeat {
		// Consume the pair, so a third click starts a new one rather than
		// firing the action again.
		m.lastClickAt = time.Time{}
	}
	return repeat
}

// openActionsForSelection is the shared path between Enter and a double click on
// a row. It reports false when the highlighted row is not an instance, so the
// caller can let the list handle the key itself.
func (m *tuiModel) openActionsForSelection() bool {
	switch item := m.list.SelectedItem().(type) {
	case vmItem:
		m.selected, m.isCT = item.vm, false
	case ctItem:
		m.selectedCT, m.isCT = item.ct, true
	default:
		return false
	}
	m.actionsList = m.newActionsList()
	m.state = "actions"
	return true
}
