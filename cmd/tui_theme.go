package cmd

// The TUI's visual language.
//
// Everything the terminal draws goes through this file, so a colour is chosen
// once and named for what it means rather than what it looks like. `danger`
// survives a palette change; `lipgloss.Color("204")` scattered through a View
// does not.
//
// Every colour is adaptive. A terminal with a light background gets a darker
// variant, because the previous palette — grey 240 on whatever the user had —
// was close to unreadable on a light theme, and a fleet tool that cannot be
// read on a projector is a fleet tool nobody demos.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/tuna-os/corral/pkg/types"
)

// ── palette ───────────────────────────────────────────────────────

var (
	// Brand and structure.
	colPrimary = lipgloss.AdaptiveColor{Light: "#7D2E68", Dark: "#F778BA"}
	colAccent  = lipgloss.AdaptiveColor{Light: "#0550AE", Dark: "#79C0FF"}
	colBorder  = lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#30363D"}
	colFocus   = lipgloss.AdaptiveColor{Light: "#7D2E68", Dark: "#F778BA"}

	// Text.
	colText   = lipgloss.AdaptiveColor{Light: "#1F2328", Dark: "#E6EDF3"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#656D76", Dark: "#8B949E"}
	colSubtle = lipgloss.AdaptiveColor{Light: "#8C959F", Dark: "#6E7681"}

	// State. These carry meaning, so they are never reused for decoration.
	colOK      = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	colWarn    = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"}
	colDanger  = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"}
	colPending = lipgloss.AdaptiveColor{Light: "#0969DA", Dark: "#58A6FF"}
)

// backendColour gives each backend its own hue, the way k9s colours resource
// kinds: a mixed fleet is easier to read when "which system is this on" is a
// colour rather than a word you have to find.
func backendColour(backend string) lipgloss.TerminalColor {
	switch backend {
	case "kubevirt":
		return lipgloss.AdaptiveColor{Light: "#0550AE", Dark: "#79C0FF"}
	case "qemu":
		return lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	case "incus":
		return lipgloss.AdaptiveColor{Light: "#BC4C00", Dark: "#FFA657"}
	case "libvirt":
		return lipgloss.AdaptiveColor{Light: "#6639BA", Dark: "#D2A8FF"}
	default:
		return colMuted
	}
}

// ── styles ────────────────────────────────────────────────────────

var (
	stTitle = lipgloss.NewStyle().Bold(true).Foreground(colPrimary).Padding(0, 1)
	stMuted = lipgloss.NewStyle().Foreground(colMuted)
	stHelp  = lipgloss.NewStyle().Foreground(colSubtle)
	stKey   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)

	stOK     = lipgloss.NewStyle().Foreground(colOK)
	stWarn   = lipgloss.NewStyle().Foreground(colWarn)
	stDanger = lipgloss.NewStyle().Foreground(colDanger)

	// A panel that does not have focus. The focused one swaps the border
	// colour, which is how lazygit shows you where your keys will land.
	stPanel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colBorder).
		Padding(0, 1)

	stPanelFocus = stPanel.Copy().BorderForeground(colFocus)

	// The header strip across the top: brand, context, fleet counts.
	stHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#0D1117"}).
			Background(colPrimary).
			Padding(0, 1)

	stHeaderDim = lipgloss.NewStyle().
			Foreground(colMuted).
			Padding(0, 1)

	// A chip: a small rounded label for a capability or a count.
	stChip = lipgloss.NewStyle().Padding(0, 1)
)

// ── status ────────────────────────────────────────────────────────

// runState is what a dot means, independent of the words each backend uses
// for it. Every backend spells its states differently; the operator only
// wants to know whether the thing is up.
type runState int

const (
	stateStopped runState = iota
	stateRunning
	statePaused
	stateBusy
	stateFailed
)

// classifyStatus maps a backend's own status string onto a run state.
func classifyStatus(status string, running bool) runState {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "fail"), strings.Contains(s, "error"),
		strings.Contains(s, "crashloop"), strings.Contains(s, "unknown"):
		return stateFailed
	case strings.Contains(s, "paus"):
		return statePaused
	case strings.Contains(s, "start"), strings.Contains(s, "stopping"),
		strings.Contains(s, "migrat"), strings.Contains(s, "pending"),
		strings.Contains(s, "provision"), strings.Contains(s, "creat"):
		return stateBusy
	case running, strings.Contains(s, "running"):
		return stateRunning
	default:
		return stateStopped
	}
}

// statusDot renders the coloured indicator for a run state. The glyph differs
// as well as the colour, so the meaning survives a monochrome terminal and a
// reader who cannot separate red from green.
func statusDot(state runState) string {
	switch state {
	case stateRunning:
		return lipgloss.NewStyle().Foreground(colOK).Render("●")
	case statePaused:
		return lipgloss.NewStyle().Foreground(colWarn).Render("⏸")
	case stateBusy:
		return lipgloss.NewStyle().Foreground(colPending).Render("◐")
	case stateFailed:
		return lipgloss.NewStyle().Foreground(colDanger).Render("✖")
	default:
		return lipgloss.NewStyle().Foreground(colSubtle).Render("○")
	}
}

// ── chips ─────────────────────────────────────────────────────────

// capabilityChips renders what an instance can actually do, so the operator
// can see it without opening the actions menu and finding half of it absent.
func capabilityChips(caps types.InstanceCapabilities) string {
	var chips []string
	add := func(on bool, label string) {
		if !on {
			return
		}
		chips = append(chips, stChip.Foreground(colAccent).Render(label))
	}
	add(caps.SSH, "ssh")
	add(caps.VNC, "vnc")
	add(caps.RDP, "rdp")
	add(caps.Snapshots, "snap")
	add(caps.Migrate, "migrate")
	add(caps.GPU, "gpu")
	if len(chips) == 0 {
		return stMuted.Render("no remote access")
	}
	return strings.Join(chips, "")
}

// backendChip names the backend in its own colour.
func backendChip(backend string) string {
	if backend == "" {
		backend = "qemu"
	}
	return stChip.Foreground(backendColour(backend)).Bold(true).Render(backend)
}

// ── header ────────────────────────────────────────────────────────

// fleetCounts is the running/stopped/total summary in the header.
type fleetCounts struct {
	running, stopped, other, total int
}

// countFleet summarises what is on screen. Counting the rendered list rather
// than the whole fleet is deliberate: the header describes what the operator
// is looking at, which is filtered by the selected context.
func countFleet(items []listRow) fleetCounts {
	var c fleetCounts
	for _, row := range items {
		c.total++
		switch row.state {
		case stateRunning:
			c.running++
		case stateStopped:
			c.stopped++
		default:
			c.other++
		}
	}
	return c
}

// listRow is the minimum a header needs to know about a list entry.
type listRow struct{ state runState }

// header renders the top strip: brand, selected context, and fleet counts.
// Modelled on k9s, where the top of the screen always answers "where am I and
// how much is here" without the operator navigating anywhere.
func header(width int, context string, counts fleetCounts, refreshing bool, spinner string) string {
	brand := stHeader.Render("🤠 CORRAL")

	ctx := context
	if ctx == "" {
		ctx = "all contexts"
	}
	scope := stHeaderDim.Render("context " + stKey.Render(ctx))

	stats := strings.Join([]string{
		lipgloss.NewStyle().Foreground(colOK).Render(fmt.Sprintf("● %d", counts.running)),
		lipgloss.NewStyle().Foreground(colSubtle).Render(fmt.Sprintf("○ %d", counts.stopped)),
		stMuted.Render(fmt.Sprintf("Σ %d", counts.total)),
	}, "  ")
	if counts.other > 0 {
		stats += "  " + lipgloss.NewStyle().Foreground(colPending).Render(fmt.Sprintf("◐ %d", counts.other))
	}
	if refreshing {
		stats += "  " + spinner
	}
	right := stHeaderDim.Render(stats)

	gap := width - lipgloss.Width(brand) - lipgloss.Width(scope) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Center,
		brand, scope, strings.Repeat(" ", gap), right)
}

// ── status bar ────────────────────────────────────────────────────

// keyHint is one binding shown in the status bar.
type keyHint struct{ key, label string }

// statusBar renders the bindings that apply right now. lazygit and k9s both
// do this, and it is the single thing that makes a dense TUI learnable: the
// keys on screen are the keys that work in the state you are in.
func statusBar(width int, hints []keyHint, notice string, noticeStyle lipgloss.Style) string {
	var parts []string
	for _, h := range hints {
		parts = append(parts, stKey.Render(h.key)+" "+stHelp.Render(h.label))
	}
	bar := "  " + strings.Join(parts, stSeparator)
	if notice != "" {
		bar = "  " + noticeStyle.Render(notice) + "\n" + bar
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(bar)
}

const stSeparator = "  ·  "

// statusWords strips the glyph a backend puts in front of its own status
// string. pkg/qemu reports "● Running" and pkg/kubevirt "○ Stopped"; the list
// row draws its own coloured dot, so without this every row shows two.
func statusWords(status string) string {
	return strings.TrimSpace(strings.TrimLeft(status, "●○◐⏸✖↓ "))
}
