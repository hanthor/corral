package cmd

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/plugin"
)

// The plugins view is the TUI counterpart to the web Extensions tab: browse the
// marketplace, see what's installed, and install/remove without leaving the
// dashboard. Marketplace fetches and installs are network-bound, so they run as
// tea.Cmds and report back through the messages below.

type pluginsLoadedMsg struct {
	entries []plugin.Entry
	err     error
}

type pluginActionMsg struct {
	verb string // "Installed" / "Removed"
	name string
	err  error
}

type pluginsModel struct {
	entries   []plugin.Entry
	installed map[string]string // name → installed version
	cursor    int
	loading   bool
	err       string
	notice    string
	confirm   *plugin.Entry // non-nil → showing the permission prompt for this entry
	busy      bool
}

func newPluginsModel() pluginsModel {
	inst := map[string]string{}
	for _, p := range plugin.Installed() {
		inst[p.Name] = p.Version
	}
	return pluginsModel{installed: inst, loading: true}
}

// fetchPluginsCmd loads the marketplace index off the UI goroutine.
func fetchPluginsCmd() tea.Cmd {
	return func() tea.Msg {
		idx, errs := plugin.FetchAll()
		var err error
		if (idx == nil || len(idx.Plugins) == 0) && len(errs) > 0 {
			err = errs[0]
		}
		var entries []plugin.Entry
		if idx != nil {
			entries = idx.Plugins
		}
		return pluginsLoadedMsg{entries: entries, err: err}
	}
}

func installPluginCmd(e plugin.Entry) tea.Cmd {
	return func() tea.Msg {
		if err := e.InstallPinned(false); err != nil {
			return pluginActionMsg{verb: "Install", name: e.Name, err: err}
		}
		return pluginActionMsg{verb: "Installed", name: e.Name}
	}
}

func removePluginCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := plugin.Remove(name); err != nil {
			return pluginActionMsg{verb: "Remove", name: name, err: err}
		}
		return pluginActionMsg{verb: "Removed", name: name}
	}
}

func (m pluginsModel) selected() *plugin.Entry {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return nil
	}
	return &m.entries[m.cursor]
}

// Update handles a key for the plugins view. It returns any cmd to run and
// whether the view wants to close (back to the list).
func (m pluginsModel) Update(msg tea.KeyPressMsg) (pluginsModel, tea.Cmd, bool) {
	s := msg.String()

	// Permission confirmation takes over the keyboard while shown.
	if m.confirm != nil {
		switch s {
		case "y", "enter":
			e := *m.confirm
			m.confirm = nil
			m.busy = true
			m.notice = "Installing " + e.Name + "…"
			return m, installPluginCmd(e), false
		default:
			m.confirm = nil
			return m, nil, false
		}
	}

	switch s {
	case "esc", "q":
		return m, nil, true
	case "ctrl+c":
		return m, nil, true
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil, false
	case "down", "j":
		if m.cursor < len(m.entries)-1 {
			m.cursor++
		}
		return m, nil, false
	case "r":
		m.loading, m.err, m.notice = true, "", ""
		return m, fetchPluginsCmd(), false
	case "enter", "i":
		e := m.selected()
		if e == nil || m.busy {
			return m, nil, false
		}
		if _, ok := m.installed[e.Name]; ok {
			m.notice = e.Name + " is already installed"
			return m, nil, false
		}
		if err := e.Compatible(); err != nil {
			m.err = err.Error()
			return m, nil, false
		}
		// Permissions require explicit consent, exactly like the CLI's
		// --accept-permissions gate.
		if len(e.Permissions) > 0 {
			ec := *e
			m.confirm = &ec
			return m, nil, false
		}
		m.busy = true
		m.notice = "Installing " + e.Name + "…"
		return m, installPluginCmd(*e), false
	case "x", "d":
		e := m.selected()
		if e == nil || m.busy {
			return m, nil, false
		}
		if _, ok := m.installed[e.Name]; !ok {
			return m, nil, false
		}
		m.busy = true
		m.notice = "Removing " + e.Name + "…"
		return m, removePluginCmd(e.Name), false
	}
	return m, nil, false
}

func (m pluginsModel) render(th theme, width int) string {
	var b strings.Builder
	b.WriteString(th.title.Render(" Extensions "))
	b.WriteString("\n\n")

	if m.loading {
		b.WriteString("  " + th.muted.Render("Loading marketplace…"))
		b.WriteString("\n\n" + th.help.Render("  esc back"))
		return b.String()
	}
	if m.err != "" {
		b.WriteString("  " + th.danger.Render(m.err) + "\n")
	}
	if m.confirm != nil {
		b.WriteString("  " + th.warn.Render(fmt.Sprintf("Install %s?", m.confirm.Name)) + "\n\n")
		b.WriteString("  It requests these permissions:\n")
		for _, p := range m.confirm.Permissions {
			b.WriteString("    • " + p + "\n")
		}
		b.WriteString("\n" + th.help.Render("  y accept & install · any other key cancel"))
		return b.String()
	}

	if len(m.entries) == 0 {
		b.WriteString("  " + th.muted.Render("No plugins in the marketplace.") + "\n")
	}
	for i, e := range m.entries {
		cursor := "  "
		if i == m.cursor {
			cursor = th.key.Render("> ")
		}
		var status string
		if v, ok := m.installed[e.Name]; ok {
			tag := "installed"
			if v != "" && v != e.Version {
				tag = "installed " + v + " → " + e.Version
			}
			status = th.ok.Render("[" + tag + "]")
		} else if err := e.Compatible(); err != nil {
			status = th.warn.Render("[needs newer corral]")
		} else {
			status = th.muted.Render("[available]")
		}
		name := th.name.Render(fmt.Sprintf("%-14s", e.Name))
		b.WriteString(fmt.Sprintf("%s%s %-8s %s\n", cursor, name, e.Version, status))
		if i == m.cursor && e.Description != "" {
			b.WriteString("    " + th.muted.Render(truncatePlugin(e.Description, width-6)) + "\n")
		}
	}

	if m.notice != "" {
		b.WriteString("\n  " + th.muted.Render(m.notice) + "\n")
	}
	b.WriteString("\n" + th.help.Render("  ↑/↓ move · i install · x remove · r refresh · esc back"))
	return b.String()
}

func truncatePlugin(s string, n int) string {
	if n < 4 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
