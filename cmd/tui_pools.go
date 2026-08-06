package cmd

// Pools and cross-backend move in the TUI (#167).
//
// The web UI does this by dragging a card onto a tree node. A terminal has
// mouse drag — Bubble Tea reports motion while a button is held, and
// cmd/tui_mouse.go already resolves a pixel to a row — but a single-column
// list is a poor drag surface: there is nowhere to drag *to* that is visible
// at the same time as the thing being dragged, and no drag ghost to show what
// is happening. Copying the gesture would make a worse version of a good
// interaction.
//
// So the TUI gets the same two operations through a picker instead: choose the
// instance, then choose the destination from a list. That is faster than
// dragging for anyone already using the keyboard, works over ssh in tmux, and
// reaches the identical server-side paths — the folder tree for grouping, the
// move preflight for relocation.
//
// The one thing that carries over unchanged is the *asymmetry*: assigning to a
// pool touches nothing and commits immediately, where moving to a backend
// stops the guest and therefore shows the preflight first and asks. A refused
// plan has no confirm key, exactly as the web dialog has no confirm button.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/backend"
	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/move"
	"github.com/tuna-os/corral/pkg/types"
)

// folderStore is the TUI's handle on the pool tree — the same on-disk store
// the web UI and the CLI use, so a pool made in one shows up in the others.
var folderStore = func() *folder.Store { return folder.NewStore(folder.ConfigBackend{}) }

// poolsState drives three related screens, distinguished by mode: picking a
// pool for an instance, picking a backend to move it to, and reading the plan
// that move would follow.
type poolsState struct {
	mode string // "assign" | "destination" | "plan"

	vm     types.VM
	ref    types.InstanceRef
	cursor int
	err    string
	notice string

	// choices are the rows of whichever picker is showing.
	choices []poolChoice
	// plan is the preflight, in "plan" mode.
	plan move.Plan
	// naming is the inline "new pool" prompt; path accumulates its keystrokes.
	naming bool
	path   string
}

// poolChoice is one row: a label, the value it selects, and why it cannot be
// selected. A disabled row is shown rather than hidden — an operator who
// cannot see that Incus exists as a backend will wonder whether Corral knows
// about it, where a greyed row with a reason answers the question.
type poolChoice struct {
	label  string
	value  string
	detail string
	reason string // non-empty disables the row
}

func (c poolChoice) enabled() bool { return c.reason == "" }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// newPoolAssignState lists the pools an instance can be put in.
func newPoolAssignState(vm types.VM) poolsState {
	s := poolsState{mode: "assign", vm: vm, ref: vm.Ref()}
	s.choices = poolChoices(s.ref)
	return s
}

func poolChoices(ref types.InstanceRef) []poolChoice {
	choices := []poolChoice{{label: "(unassigned)", value: "", detail: "remove from its pool"}}
	tree, err := folderStore().Tree()
	if err != nil {
		return choices
	}
	current := tree.PathOf(ref)
	paths := tree.Paths()
	sort.Strings(paths)
	for _, path := range paths {
		members := len(tree.Members(path, false))
		detail := fmt.Sprintf("%d instance%s", members, plural(members))
		if path == current {
			detail += " · current"
		}
		choices = append(choices, poolChoice{label: path, value: path, detail: detail})
	}
	return append(choices, poolChoice{label: "+ new pool…", value: "\x00new", detail: "nest with /"})
}

// newMoveDestinationState lists the backends an instance could move to.
func newMoveDestinationState(vm types.VM) poolsState {
	s := poolsState{mode: "destination", vm: vm, ref: vm.Ref()}
	for _, name := range backend.Backends {
		if name == vm.Backend {
			continue // same backend is `corral migrate`, a different operation
		}
		choice := poolChoice{label: name, value: name}
		if reason := backend.IngestRefusal(name); reason != "" {
			choice.reason = reason
		} else {
			choice.detail = "accepts moves"
		}
		s.choices = append(s.choices, choice)
	}
	return s
}

// planMove runs the preflight and switches to the plan screen. It changes
// nothing — the same guarantee the web UI relies on to make a drop safe.
func (s *poolsState) planMove(destination string) {
	s.plan = move.Preflight(move.Inspect(s.vm, false), move.Target{Backend: destination})
	s.mode = "plan"
	s.cursor = 0
}

// executeMove is a seam. A test needs to assert that choosing a destination
// runs *nothing* — that the plan screen is reached without the guest being
// stopped — and asserting on the rendered view cannot tell the difference
// between "not executed" and "executed and failed".
var executeMove = func(plan move.Plan) (move.Result, error) {
	return move.Execute(context.Background(), plan, nil)
}

// commitMove runs an approved plan. Preflight already refused everything it
// could; Execute re-checks rather than trusting this caller.
func (s *poolsState) commitMove() {
	result, err := executeMove(s.plan)
	if err != nil {
		s.err = err.Error()
		return
	}
	s.notice = fmt.Sprintf("moved to %s/%s — created stopped",
		result.Destination.Backend, result.Destination.Name)
}

// assign puts the instance in a pool, or takes it out when path is empty.
func (s *poolsState) assign(path string) {
	err := folderStore().Update(func(tree *folder.Tree) error {
		if path == "" {
			tree.Unassign(s.ref)
			return nil
		}
		if _, err := tree.Ensure(path); err != nil {
			return err
		}
		return tree.Assign(s.ref, path)
	})
	if err != nil {
		s.err = err.Error()
		return
	}
	if path == "" {
		s.notice = "removed from its pool"
		return
	}
	s.notice = "added to " + path
}

// ── input ─────────────────────────────────────────────────────────

// updatePools handles a key. It returns false when the screen is done, which
// the caller reads as "go back", matching how the snapshots and events views
// hand control back.
func (m *tuiModel) updatePools(msg tea.KeyPressMsg) bool {
	s := &m.pools

	if s.naming {
		switch msg.String() {
		case "esc":
			s.naming, s.path = false, ""
		case "enter":
			path := strings.TrimSpace(s.path)
			s.naming, s.path = false, ""
			if path != "" {
				s.assign(path)
				s.choices = poolChoices(s.ref)
			}
		case "backspace":
			if s.path != "" {
				s.path = s.path[:len(s.path)-1]
			}
		default:
			if r := msg.String(); len(r) == 1 {
				s.path += r
			}
		}
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
		if s.mode != "plan" && s.cursor < len(s.choices)-1 {
			s.cursor++
		}
	case "enter", " ":
		return s.activate()
	case "y":
		// Only the plan screen has a confirm, and only when it can run.
		if s.mode == "plan" && s.plan.OK() {
			s.commitMove()
			return false
		}
	}
	return true
}

func (s *poolsState) activate() bool {
	if s.mode == "plan" {
		return true // the plan screen confirms with y, not enter
	}
	if s.cursor >= len(s.choices) {
		return true
	}
	choice := s.choices[s.cursor]
	if !choice.enabled() {
		s.err = choice.reason
		return true
	}
	switch s.mode {
	case "assign":
		if choice.value == "\x00new" {
			s.naming, s.err = true, ""
			return true
		}
		s.assign(choice.value)
		return false
	case "destination":
		s.planMove(choice.value)
	}
	return true
}

// ── rendering ─────────────────────────────────────────────────────

func (m tuiModel) poolsView() string {
	s := m.pools
	th := m.th
	var b strings.Builder

	switch s.mode {
	case "assign":
		b.WriteString(th.title.Render(" Pool for "+s.vm.Name+" ") + "\n\n")
	case "destination":
		b.WriteString(th.title.Render(" Move "+s.vm.Name+" to… ") + "\n\n")
		b.WriteString(th.muted.Render("  A move stops the guest. It is not a live migration.") + "\n\n")
	case "plan":
		return m.movePlanView()
	}

	if s.naming {
		b.WriteString("  New pool: " + s.path + "▌\n")
		b.WriteString(th.muted.Render("  nest with / — enter to create, esc to cancel") + "\n")
		return b.String()
	}

	for i, choice := range s.choices {
		cursor := "  "
		if i == s.cursor {
			cursor = th.key.Render("▸ ")
		}
		line := choice.label
		if choice.detail != "" {
			line += "  " + th.muted.Render(choice.detail)
		}
		if !choice.enabled() {
			line = th.muted.Render(choice.label + "  (unavailable)")
		}
		b.WriteString(cursor + line + "\n")
	}

	b.WriteString("\n")
	if s.err != "" {
		b.WriteString(th.danger.Render("  "+s.err) + "\n")
	}
	b.WriteString(th.muted.Render("  ↑/↓ choose · enter select · esc back") + "\n")
	return b.String()
}

// movePlanView is the terminal's version of the web confirmation dialog, and
// shows the same things for the same reason: every refusal, every warning, and
// what will not survive the move.
func (m tuiModel) movePlanView() string {
	s := m.pools
	th := m.th
	var b strings.Builder

	b.WriteString(th.title.Render(fmt.Sprintf(" Move %s → %s ",
		s.plan.Source.Name, s.plan.Destination.Backend)) + "\n\n")

	for i, step := range s.plan.Steps {
		b.WriteString(fmt.Sprintf("  %d. %-14s %s\n", i+1, step.Name, th.muted.Render(step.Detail)))
	}

	if len(s.plan.Refusals) > 0 {
		b.WriteString("\n" + th.danger.Render("  This move cannot run:") + "\n")
		for _, refusal := range s.plan.Refusals {
			b.WriteString(th.danger.Render("   ✗ "+refusal.Reason) + "\n")
			if refusal.Remedy != "" {
				b.WriteString(th.muted.Render("     "+refusal.Remedy) + "\n")
			}
		}
	}
	// Warnings print even on a refused plan: fixing the refusal does not make
	// the address change go away.
	if len(s.plan.Warnings) > 0 {
		b.WriteString("\n  Warnings:\n")
		for _, warning := range s.plan.Warnings {
			b.WriteString(th.warn.Render("   ! "+warning) + "\n")
		}
	}
	if len(s.plan.Dropped) > 0 {
		b.WriteString("\n  Not carried over:\n")
		for _, dropped := range s.plan.Dropped {
			b.WriteString(th.muted.Render("   - "+dropped) + "\n")
		}
	}

	b.WriteString("\n")
	if s.err != "" {
		b.WriteString(th.danger.Render("  "+s.err) + "\n")
	}
	if s.plan.OK() {
		b.WriteString(th.muted.Render("  y move · esc cancel") + "\n")
	} else {
		b.WriteString(th.muted.Render("  esc back") + "\n")
	}
	return b.String()
}

// poolLabel is the pool an instance belongs to, for the list subtitle. Empty
// when it is in none, so the row stays unchanged rather than gaining a "(none)"
// that is noise on a fleet nobody has organised yet.
func poolLabel(ref types.InstanceRef) string {
	tree, err := folderStore().Tree()
	if err != nil {
		return ""
	}
	return tree.PathOf(ref)
}
