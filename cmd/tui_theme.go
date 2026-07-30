package cmd

// The TUI's visual language.
//
// Everything the terminal draws goes through this file, so a colour is chosen
// once and named for what it means rather than what it looks like. `danger`
// survives a palette change; `lipgloss.Color("204")` scattered through a View
// does not.
//
// Colours adapt to the terminal's background. A light terminal gets darker
// variants, because the previous palette — grey 240 on whatever the user had —
// was close to unreadable on a light theme, and a fleet tool that cannot be
// read on a projector is a fleet tool nobody demos.
//
// lipgloss v2 removed AdaptiveColor, and deliberately: resolving a colour used
// to hide a terminal query behind what looked like a constant. Now the
// background is asked for once, over the Bubble Tea event loop, and a theme is
// built from the answer. That is why every style lives on a struct rather than
// in a package-level var — the values do not exist until the terminal has
// replied.

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/tuna-os/corral/pkg/types"
)

// ── palette ───────────────────────────────────────────────────────

// palette holds every colour the TUI uses, already resolved for one
// background. lipgloss.LightDark picks between the pair.
type palette struct {
	primary, accent, border, focus color.Color
	text, muted, subtle            color.Color
	ok, warn, danger, pending      color.Color
	headerFG                       color.Color
	kubevirt, qemu, incus, libvirt color.Color
}

func newPalette(isDark bool) palette {
	pick := lipgloss.LightDark(isDark)
	return palette{
		primary: pick(lipgloss.Color("#7D2E68"), lipgloss.Color("#F778BA")),
		accent:  pick(lipgloss.Color("#0550AE"), lipgloss.Color("#79C0FF")),
		border:  pick(lipgloss.Color("#D0D7DE"), lipgloss.Color("#30363D")),
		focus:   pick(lipgloss.Color("#7D2E68"), lipgloss.Color("#F778BA")),

		text:   pick(lipgloss.Color("#1F2328"), lipgloss.Color("#E6EDF3")),
		muted:  pick(lipgloss.Color("#656D76"), lipgloss.Color("#8B949E")),
		subtle: pick(lipgloss.Color("#8C959F"), lipgloss.Color("#6E7681")),

		// State colours carry meaning, so they are never reused for decoration.
		ok:      pick(lipgloss.Color("#1A7F37"), lipgloss.Color("#3FB950")),
		warn:    pick(lipgloss.Color("#9A6700"), lipgloss.Color("#D29922")),
		danger:  pick(lipgloss.Color("#CF222E"), lipgloss.Color("#F85149")),
		pending: pick(lipgloss.Color("#0969DA"), lipgloss.Color("#58A6FF")),

		headerFG: pick(lipgloss.Color("#FFFFFF"), lipgloss.Color("#0D1117")),

		// One hue per backend, the way k9s colours resource kinds: a mixed
		// fleet is easier to read when "which system is this on" is a colour
		// rather than a word you have to find.
		kubevirt: pick(lipgloss.Color("#0550AE"), lipgloss.Color("#79C0FF")),
		qemu:     pick(lipgloss.Color("#1A7F37"), lipgloss.Color("#3FB950")),
		incus:    pick(lipgloss.Color("#BC4C00"), lipgloss.Color("#FFA657")),
		libvirt:  pick(lipgloss.Color("#6639BA"), lipgloss.Color("#D2A8FF")),
	}
}

func (p palette) backend(name string) color.Color {
	switch name {
	case "kubevirt":
		return p.kubevirt
	case "qemu":
		return p.qemu
	case "incus":
		return p.incus
	case "libvirt":
		return p.libvirt
	default:
		return p.muted
	}
}

// ── styles ────────────────────────────────────────────────────────

// theme is every style the TUI draws with, resolved for one background.
type theme struct {
	p palette

	title, muted, help, key   lipgloss.Style
	ok, warn, danger          lipgloss.Style
	panel, panelFocus         lipgloss.Style
	header, headerDim, chip   lipgloss.Style
	label, name, nameSelected lipgloss.Style
}

func newTheme(isDark bool) theme {
	p := newPalette(isDark)
	return theme{
		p:     p,
		title: lipgloss.NewStyle().Bold(true).Foreground(p.primary).Padding(0, 1),
		muted: lipgloss.NewStyle().Foreground(p.muted),
		help:  lipgloss.NewStyle().Foreground(p.subtle),
		key:   lipgloss.NewStyle().Bold(true).Foreground(p.accent),

		ok:     lipgloss.NewStyle().Foreground(p.ok),
		warn:   lipgloss.NewStyle().Foreground(p.warn),
		danger: lipgloss.NewStyle().Foreground(p.danger),

		// A panel without focus. The focused one swaps the border colour,
		// which is how lazygit shows where your keys will land.
		panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.border).
			Padding(0, 1),
		panelFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.focus).
			Padding(0, 1),

		// The strip across the top: brand, context, fleet counts.
		header: lipgloss.NewStyle().Bold(true).
			Foreground(p.headerFG).Background(p.primary).Padding(0, 1),
		headerDim: lipgloss.NewStyle().Foreground(p.muted).Padding(0, 1),

		// A chip: a small label for a capability or a backend.
		chip: lipgloss.NewStyle().Padding(0, 1),

		label:        lipgloss.NewStyle().Foreground(p.subtle).Bold(true),
		name:         lipgloss.NewStyle().Foreground(p.text),
		nameSelected: lipgloss.NewStyle().Bold(true).Foreground(p.text),
	}
}

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
func (t theme) statusDot(state runState) string {
	switch state {
	case stateRunning:
		return lipgloss.NewStyle().Foreground(t.p.ok).Render("●")
	case statePaused:
		return lipgloss.NewStyle().Foreground(t.p.warn).Render("⏸")
	case stateBusy:
		return lipgloss.NewStyle().Foreground(t.p.pending).Render("◐")
	case stateFailed:
		return lipgloss.NewStyle().Foreground(t.p.danger).Render("✖")
	default:
		return lipgloss.NewStyle().Foreground(t.p.subtle).Render("○")
	}
}

// ── chips ─────────────────────────────────────────────────────────

// capabilityChips renders what an instance can actually do, so the operator
// can see it without opening the actions menu and finding half of it absent.
func (t theme) capabilityChips(caps types.InstanceCapabilities) string {
	var chips []string
	add := func(on bool, label string) {
		if !on {
			return
		}
		chips = append(chips, t.chip.Foreground(t.p.accent).Render(label))
	}
	add(caps.SSH, "ssh")
	add(caps.VNC, "vnc")
	add(caps.RDP, "rdp")
	add(caps.Snapshots, "snap")
	add(caps.Migrate, "migrate")
	add(caps.GPU, "gpu")
	if len(chips) == 0 {
		return t.muted.Render("no remote access")
	}
	return strings.Join(chips, "")
}

// backendChip names the backend in its own colour.
func (t theme) backendChip(backend string) string {
	if backend == "" {
		backend = "qemu"
	}
	return t.chip.Foreground(t.p.backend(backend)).Bold(true).Render(backend)
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
func (t theme) headerBar(width int, context string, counts fleetCounts, refreshing bool, spinner string) string {
	brand := t.header.Render("🤠 CORRAL")

	ctx := context
	if ctx == "" {
		ctx = "all contexts"
	}
	scope := t.headerDim.Render("context " + t.key.Render(ctx))

	stats := strings.Join([]string{
		lipgloss.NewStyle().Foreground(t.p.ok).Render(fmt.Sprintf("● %d", counts.running)),
		lipgloss.NewStyle().Foreground(t.p.subtle).Render(fmt.Sprintf("○ %d", counts.stopped)),
		t.muted.Render(fmt.Sprintf("Σ %d", counts.total)),
	}, "  ")
	if counts.other > 0 {
		stats += "  " + lipgloss.NewStyle().Foreground(t.p.pending).Render(fmt.Sprintf("◐ %d", counts.other))
	}
	if refreshing {
		stats += "  " + spinner
	}
	right := t.headerDim.Render(stats)

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
func (t theme) statusBar(width int, hints []keyHint, notice string, noticeStyle lipgloss.Style) string {
	var parts []string
	for _, h := range hints {
		parts = append(parts, t.key.Render(h.key)+" "+t.help.Render(h.label))
	}
	bar := "  " + strings.Join(parts, hintSeparator)
	if notice != "" {
		bar = "  " + noticeStyle.Render(notice) + "\n" + bar
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(bar)
}

const hintSeparator = "  ·  "

// statusWords strips the glyph a backend puts in front of its own status
// string. pkg/qemu reports "● Running" and pkg/kubevirt "○ Stopped"; the list
// row draws its own coloured dot, so without this every row shows two.
func statusWords(status string) string {
	return strings.TrimSpace(strings.TrimLeft(status, "●○◐⏸✖↓ "))
}
