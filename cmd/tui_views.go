package cmd

// TUI views that close the gap with the web UI.
//
// The web dashboard grew a tab per concern — Snapshots, Events, the template
// mark, CT hardware — while the TUI kept a single fire-and-forget action list.
// The result was a terminal that could *take* a snapshot but never show you the
// ones you had, and a "Snapshot" action wired straight to pkg/kubevirt even
// though snapshots became backend-neutral in #134.
//
// Parity here means the same operations against the same packages, not the same
// layout: the web UI's tabs become states of the one model, and anything that
// only reads well in a browser (live CPU graphs, the noVNC canvas, image
// uploads) is deliberately left out.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

// ── Snapshots ─────────────────────────────────────────────────────

// snapshotsState is the Snapshots tab: the VM's captures, plus create,
// restore, and delete. It goes through pkg/snapshot rather than pkg/kubevirt,
// so a libvirt or Incus instance gets its own adapter's semantics — including
// the honest refusals, which are shown verbatim instead of being swallowed.
type snapshotsState struct {
	ref     types.InstanceRef
	vmName  string
	running bool
	snaps   []snapshot.Snapshot
	cursor  int
	err     string
	notice  string

	// naming is the "new snapshot" prompt. An empty value asks the adapter to
	// generate a name, matching the web UI's optional name field.
	naming bool
	name   textinput.Model

	// confirm is "" | "delete" | "restore" — a destructive step never runs
	// off a single keypress, same as the web UI's confirm dialogs.
	confirm string
}

func newSnapshotsState(vm types.VM) snapshotsState {
	ti := textinput.New()
	ti.Placeholder = "name (blank = auto)"
	ti.CharLimit = 63
	ti.SetWidth(32)

	s := snapshotsState{ref: vm.Ref(), vmName: vm.Name, running: vm.Running, name: ti}
	s.reload()
	return s
}

func (s *snapshotsState) reload() {
	adapter, err := snapshot.For(s.ref)
	if err != nil {
		s.snaps, s.err = nil, err.Error()
		return
	}
	snaps, err := adapter.List(s.ref)
	if err != nil {
		s.snaps, s.err = nil, err.Error()
		return
	}
	s.snaps, s.err = snaps, ""
	if s.cursor >= len(s.snaps) {
		s.cursor = max(0, len(s.snaps)-1)
	}
}

func (s *snapshotsState) create(name string) {
	adapter, err := snapshot.For(s.ref)
	if err != nil {
		s.err = err.Error()
		return
	}
	snap, err := adapter.Create(s.ref, strings.TrimSpace(name))
	if err != nil {
		s.err = err.Error()
		return
	}
	// The consistency the adapter actually achieved, not the one that was
	// hoped for — a crash-consistent capture of a database is worth saying
	// out loud, and the web UI says it too.
	s.notice = fmt.Sprintf("created %s (%s-consistent)", snap.Name, snap.Consistency)
	s.reload()
}

func (s *snapshotsState) selected() (snapshot.Snapshot, bool) {
	if s.cursor < 0 || s.cursor >= len(s.snaps) {
		return snapshot.Snapshot{}, false
	}
	return s.snaps[s.cursor], true
}

func (s *snapshotsState) restore() {
	snap, ok := s.selected()
	if !ok {
		return
	}
	adapter, err := snapshot.For(s.ref)
	if err != nil {
		s.err = err.Error()
		return
	}
	if err := adapter.Restore(s.ref, snap.Name); err != nil {
		s.err = err.Error()
		return
	}
	s.err, s.notice = "", "restoring from "+snap.Name
	s.reload()
}

func (s *snapshotsState) delete() {
	snap, ok := s.selected()
	if !ok {
		return
	}
	adapter, err := snapshot.For(s.ref)
	if err != nil {
		s.err = err.Error()
		return
	}
	if err := adapter.Delete(s.ref, snap.Name); err != nil {
		s.err = err.Error()
		return
	}
	s.err, s.notice = "", "deleted "+snap.Name
	s.reload()
}

// updateSnapshots handles the Snapshots view's keys. It returns false when the
// view is done and the caller should go back to the actions menu.
func (m *tuiModel) updateSnapshots(msg tea.KeyPressMsg) bool {
	s := &m.snapshots

	if s.naming {
		switch msg.String() {
		case "enter":
			s.naming = false
			value := s.name.Value()
			s.name.Reset()
			s.create(value)
		case "esc":
			s.naming = false
			s.name.Reset()
		default:
			s.name, _ = s.name.Update(msg)
		}
		return true
	}

	if s.confirm != "" {
		switch msg.String() {
		case "y", "Y":
			if s.confirm == "delete" {
				s.delete()
			} else {
				s.restore()
			}
		}
		s.confirm = ""
		return true
	}

	switch msg.String() {
	case "esc", "q":
		return false
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
	case "down", "j":
		if s.cursor < len(s.snaps)-1 {
			s.cursor++
		}
	case "n":
		s.naming = true
		s.err, s.notice = "", ""
		s.name.Focus()
	case "r":
		s.err, s.notice = "", ""
		s.reload()
	case "enter":
		if _, ok := s.selected(); ok {
			s.confirm = "restore"
		}
	case "x", "d":
		if _, ok := s.selected(); ok {
			s.confirm = "delete"
		}
	}
	return true
}

func (m tuiModel) snapshotsView() string {
	s := m.snapshots
	th := m.th

	var body string
	switch {
	case s.err != "":
		// An Unsupported refusal carries its own remedy; print it whole rather
		// than reducing it to "failed".
		body = th.danger.Render("✖ " + s.err)
	case len(s.snaps) == 0:
		body = th.muted.Render("No snapshots yet. Press n to take one.")
	default:
		t := table.New().
			Width(tableWidth(m.width)).
			Border(lipgloss.RoundedBorder()).
			BorderStyle(lipgloss.NewStyle().Foreground(th.p.border)).
			Headers("", "NAME", "READY", "CONSISTENCY", "CREATED").
			StyleFunc(func(row, col int) lipgloss.Style {
				base := lipgloss.NewStyle().Padding(0, 1)
				if row == table.HeaderRow {
					return base.Bold(true).Foreground(th.p.muted)
				}
				switch col {
				case 1:
					if row == s.cursor {
						return base.Bold(true).Foreground(th.p.text)
					}
					return base.Foreground(th.p.text)
				case 4:
					return base.Foreground(th.p.muted)
				default:
					return base
				}
			})
		for i, snap := range s.snaps {
			marker := " "
			if i == s.cursor {
				marker = lipgloss.NewStyle().Foreground(th.p.primary).Bold(true).Render("▌")
			}
			ready := th.warn.Render("pending")
			if snap.Ready {
				ready = th.ok.Render("ready")
			}
			t.Row(marker, snap.Name, ready, consistencyLabel(snap.Consistency, th), humanAge(snap.Created))
		}
		body = t.String()
	}

	if s.naming {
		body += "\n\n  " + th.label.Render("NAME") + "  " + s.name.View()
	}

	notice, noticeStyle := s.notice, th.ok
	if s.confirm == "delete" {
		if snap, ok := s.selected(); ok {
			notice, noticeStyle = "Delete "+snap.Name+"? y to confirm, any other key cancels", th.danger
		}
	} else if s.confirm == "restore" {
		if snap, ok := s.selected(); ok {
			notice, noticeStyle = "Restore "+m.snapshots.vmName+" from "+snap.Name+"? y to confirm", th.warn
		}
	} else if notice == "" && s.running {
		// Every adapter that needs the instance stopped says so on refusal,
		// but saying it before the keypress saves the round trip.
		notice, noticeStyle = "VM is running — a restore may be refused until it is stopped", th.muted
	}

	hints := []keyHint{{"n", "new"}, {"enter", "restore"}, {"x", "delete"}, {"r", "reload"}, {"esc", "back"}}
	if s.naming {
		hints = []keyHint{{"enter", "create"}, {"esc", "cancel"}}
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		th.title.Render(fmt.Sprintf(" Snapshots · %s ", s.vmName)),
		"",
		body,
		th.statusBar(m.width, hints, notice, noticeStyle),
	)
}

// consistencyLabel colours what a capture actually caught. Crash-consistent is
// a warning, not a detail: it is the one that can restore into a broken guest.
func consistencyLabel(c snapshot.Consistency, th theme) string {
	switch c {
	case snapshot.Offline:
		return th.ok.Render("offline")
	case snapshot.Filesystem:
		return th.ok.Render("filesystem")
	case snapshot.Crash:
		return th.warn.Render("crash")
	case "":
		return th.muted.Render("—")
	default:
		return th.muted.Render(string(c))
	}
}

// ── Events ────────────────────────────────────────────────────────

// eventsState is the Events tab: recent Kubernetes events for the VM and its
// virt-launcher pod. KubeVirt-only, because no other backend has an event
// stream to show — the actions menu gates it accordingly.
type eventsState struct {
	vmName string
	events []kubevirt.EventInfo
	err    string
}

func newEventsState(vm types.VM) eventsState {
	s := eventsState{vmName: vm.Name}
	ns := vm.Namespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	evs, err := kubevirt.NewClientForContext(ns, vm.Context).Events(vm.Name)
	if err != nil {
		s.err = err.Error()
		return s
	}
	s.events = evs
	return s
}

func (m tuiModel) eventsView() string {
	th := m.th
	s := m.events

	var body string
	switch {
	case s.err != "":
		body = th.danger.Render("✖ " + s.err)
	case len(s.events) == 0:
		body = th.muted.Render("No recent events for this VM.")
	default:
		t := table.New().
			Width(tableWidth(m.width)).
			Border(lipgloss.RoundedBorder()).
			BorderStyle(lipgloss.NewStyle().Foreground(th.p.border)).
			Headers("", "AGE", "REASON", "OBJECT", "MESSAGE").
			StyleFunc(func(row, col int) lipgloss.Style {
				base := lipgloss.NewStyle().Padding(0, 1)
				if row == table.HeaderRow {
					return base.Bold(true).Foreground(th.p.muted)
				}
				switch col {
				case 3:
					return base.Foreground(th.p.accent)
				case 4:
					return base.Foreground(th.p.muted).MaxWidth(maxDetail(m.width))
				default:
					return base
				}
			})
		for _, e := range s.events {
			mark := th.muted.Render("·")
			if e.Type == "Warning" {
				mark = th.warn.Render("!")
			}
			t.Row(mark, humanAge(e.Time), e.Reason, e.Object, oneLine(e.Message))
		}
		body = t.String()
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		th.title.Render(fmt.Sprintf(" Events · %s ", s.vmName)),
		"",
		body,
		th.statusBar(m.width, []keyHint{{"r", "reload"}, {"esc", "back"}}, "", th.muted),
	)
}

// oneLine flattens a multi-line event message. Kubernetes puts whole command
// lines in a Message, and a newline inside a table cell breaks the borders.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// humanAge turns a backend timestamp into "4m" / "3h" / "2d", the way kubectl
// and the web UI both show it. An unparseable stamp is passed through: a raw
// value the operator can still read beats an empty cell.
func humanAge(stamp string) string {
	if stamp == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// ── Template mark ─────────────────────────────────────────────────

// toggleTemplate marks or unmarks the golden template label — the same
// MarkTemplate call behind the web UI's "Make template" button. Templates are
// what `corral clone` copies from, so a fleet driven from the terminal needs to
// be able to set the mark without opening a browser.
func (m *tuiModel) toggleTemplate() {
	vm := m.selected
	ns := vm.Namespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	on := !vm.IsTemplate
	if err := kubevirt.NewClientForContext(ns, vm.Context).MarkTemplate(vm.Name, on); err != nil {
		m.notice, m.noticeKind = "template: "+err.Error(), "error"
		return
	}
	if on {
		m.notice, m.noticeKind = vm.Name+" marked as a template", "ok"
	} else {
		m.notice, m.noticeKind = vm.Name+" is no longer a template", "ok"
	}
}

// ── CT hardware ───────────────────────────────────────────────────

// newCTHWEditModel is the CT side of the hardware editor. The web UI scales a
// CT from the same form it scales a VM from; ct.Scale resizes the pod in place
// where the cluster allows it and asks for a stop/start where it does not,
// which is why the note differs from a VM's hotplug promise.
func newCTHWEditModel(c ct.CT) hwEditModel {
	m := newHWEditModel(types.VM{Name: c.Name, Namespace: c.Namespace, CPU: c.CPU, Mem: c.Mem})
	m.isCT, m.ct = true, c
	return m
}
