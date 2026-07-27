package cmd

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

// ── Styles ────────────────────────────────────────────────────────
var (
	tuiTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Padding(0, 1)
	tuiRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	tuiStopped  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	tuiProxyOn  = "●"
	tuiProxyOff = "○"
	tuiHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	// postQuitAction is set by the TUI when an action needs to run
	// after the Bubble Tea program quits (e.g. SSH, Viewer).
	postQuitAction func()
)

// ── VM item for the list ──────────────────────────────────────────

type vmItem struct {
	vm      types.VM
	display string
}

func (i vmItem) Title() string       { return i.vm.Name }
func (i vmItem) Description() string { return i.display }
func (i vmItem) FilterValue() string { return i.vm.Name }

func vmToItem(vm types.VM) vmItem {
	proxy := tuiProxyOff
	if vm.VNC == "on" {
		proxy = tuiProxyOn
	} else if vm.VNC == "pending" {
		proxy = "◐"
	}
	display := fmt.Sprintf("%s  %s  ports:%s  %d CPU / %s",
		vm.Status, vm.Backend, proxy, vm.CPU, vm.Mem)
	if vm.Node != "" && vm.Node != "—" {
		display += "  " + vm.Node
	}
	if vm.IP != "" {
		display += "  " + vm.IP
	}
	return vmItem{vm: vm, display: display}
}

// ── CT item for the list ────────────────────────────────────────────
//
// A CT is not a types.VM (see pkg/ct's package doc — deliberately not a
// types.Backend peer), so it gets its own list.Item rather than being
// coerced into vmItem's shape. list.Model just wants anything satisfying
// list.Item, so vmItem and ctItem values mix freely in one []list.Item —
// same "merged, distinguished by icon" list the web UI's tree now uses,
// matching real Proxmox's own single resource tree per node/pool.

type ctItem struct {
	ct      ct.CT
	display string
}

func (i ctItem) Title() string       { return "[CT] " + i.ct.Name }
func (i ctItem) Description() string { return i.display }
func (i ctItem) FilterValue() string { return i.ct.Name }

func ctToItem(c ct.CT) ctItem {
	priv := ""
	if c.Privileged {
		priv = "  privileged"
	}
	return ctItem{
		ct: c,
		display: fmt.Sprintf("CT  %s  %d CPU / %s%s",
			c.Phase, c.CPU, c.Mem, priv),
	}
}

// ── Action item for the actions menu ──────────────────────────────

type actionItem struct {
	id    string
	label string
}

func (a actionItem) Title() string       { return a.label }
func (a actionItem) Description() string { return "" }
func (a actionItem) FilterValue() string { return a.label }

var actionsListItems = []actionItem{
	{id: "start", label: "Start"},
	{id: "stop", label: "Stop"},
	{id: "restart", label: "Restart"},
	{id: "pause", label: "Pause"},
	{id: "unpause", label: "Resume"},
	{id: "migrate", label: "Migrate"},
	{id: "clone", label: "Clone"},
	{id: "hardware", label: "Edit CPU / RAM"},
	{id: "snapshot", label: "Snapshot"},
	{id: "export", label: "Export (backup disk)"},
	{id: "ssh", label: "SSH"},
	{id: "viewer", label: "Viewer (VNC)"},
	{id: "ports", label: "Edit ports"},
	{id: "delete", label: "Delete"},
}

// actionApplies reports whether an action is a valid transition from the
// VM's current power state — the menu only offers what can actually happen
// (Proxmox-style), instead of a fixed list where "Start" leads on a VM
// that's already running.
func actionApplies(id string, vm types.VM) bool {
	paused := strings.Contains(vm.Status, "Paused")
	switch id {
	case "start":
		return !vm.Running
	case "stop", "restart":
		return vm.Running
	case "pause":
		return vm.Running && !paused
	case "unpause":
		return paused
	case "migrate":
		return vm.Backend == "kubevirt" && vm.Running && !paused
	case "ssh", "viewer":
		return vm.Running && !paused
	default:
		return true
	}
}

func ctActionApplies(id string, c ct.CT) bool {
	switch id {
	case "start":
		return c.Phase != "Running"
	case "stop", "console":
		return c.Phase == "Running"
	default:
		return true
	}
}

// CT actions are a small, distinct set — no migrate/snapshot/hardware-edit/
// ports/clone, since those are VM (hypervisor) concepts a plain pod doesn't
// have. "Console" replaces "SSH": a CT is reached by kubectl exec, not a
// virtctl SSH tunnel.
var actionsListItemsCT = []actionItem{
	{id: "start", label: "Start"},
	{id: "stop", label: "Stop"},
	{id: "console", label: "Console"},
	{id: "delete", label: "Delete"},
}

// ── Main model ────────────────────────────────────────────────────

type tuiModel struct {
	list        list.Model
	actionsList list.Model
	quitting    bool
	state       string // "list", "actions", "edit", "hwedit", "confirmDelete", "doctor", "cloneInput"
	selected    types.VM
	isCT        bool // true when the actions menu / performAction target is selectedCT, not selected
	selectedCT  ct.CT
	edit        editModel
	hwEdit      hwEditModel
	doctorRows  []doctor.Check
	cloneInput  textinput.Model
	cloneErr    string
	width       int
	height      int
}

func newTUIModel() tuiModel {
	items := []list.Item{}
	vms, _ := kubevirt.NewClient("").ListVMs()
	for _, vm := range vms {
		items = append(items, vmToItem(vm))
	}
	qVMs, _ := qemu.List()
	for _, vm := range qVMs {
		items = append(items, vmToItem(vm))
	}
	if plugin.IsInstalled("incus") {
		iVMs, _ := incus.List()
		for _, vm := range iVMs {
			items = append(items, vmToItem(vm))
		}
	}
	cts, _ := ct.ListCTs()
	for _, c := range cts {
		items = append(items, ctToItem(c))
	}

	if len(items) == 0 {
		fmt.Println("No VMs or CTs found. Create one: corral create <name>")
		os.Exit(0)
	}

	l := list.New(items, vmItemDelegate{}, 0, 0)
	l.Title = tuiListTitle(items)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.Styles.Title = tuiTitle

	m := tuiModel{list: l, state: "list"}
	m.actionsList = m.newActionsList()
	return m
}

func (m *tuiModel) newActionsList() list.Model {
	title := "Actions"
	source := actionsListItems
	if m.isCT {
		if m.selectedCT.Name != "" {
			title = fmt.Sprintf("%s (ct)", m.selectedCT.Name)
		}
		source = actionsListItemsCT
	} else if m.selected.Name != "" {
		b := m.selected.Backend
		if b == "" {
			b = "qemu"
		}
		title = fmt.Sprintf("%s (%s)", m.selected.Name, b)
	}

	listItems := make([]list.Item, 0, len(source))
	for _, a := range source {
		if m.isCT && m.selectedCT.Name != "" && !ctActionApplies(a.id, m.selectedCT) {
			continue
		}
		if !m.isCT && m.selected.Name != "" && !actionApplies(a.id, m.selected) {
			continue
		}
		listItems = append(listItems, a)
	}

	// Height: 2 rows per item + title/padding, capped to the window so every
	// action (Delete included) is visible without paginating.
	h := len(listItems)*2 + 4
	if m.height > 2 && h > m.height-2 {
		h = m.height - 2
	}
	l := list.New(listItems, actionItemDelegate{}, 30, h)
	l.Title = title
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	l.SetShowTitle(true)
	l.Styles.Title = tuiTitle
	l.KeyMap.Quit.Unbind()
	l.KeyMap.ForceQuit.Unbind()
	return l
}

func (m tuiModel) Init() tea.Cmd { return nil }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(msg.Width, msg.Height-2)
		m.actionsList.SetSize(msg.Width, msg.Height-2)
		return m, nil

	case tea.KeyMsg:
		if m.state == "edit" {
			em, cmd := m.edit.Update(msg)
			m.edit = em.(editModel)
			if m.edit.done {
				m.state = "list"
				m.refreshList()
				return m, nil
			}
			return m, cmd
		}

		if m.state == "hwedit" {
			hm, cmd := m.hwEdit.Update(msg)
			m.hwEdit = hm.(hwEditModel)
			if m.hwEdit.done {
				m.state = "list"
				m.refreshList()
				return m, nil
			}
			return m, cmd
		}

		if m.state == "cloneInput" {
			switch msg.String() {
			case "esc":
				m.state = "actions"
				return m, nil
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "enter":
				dst := strings.TrimSpace(m.cloneInput.Value())
				if dst == "" {
					m.cloneErr = "name can't be empty"
					return m, nil
				}
				if err := runClone(m.selected, dst); err != nil {
					m.cloneErr = err.Error()
					return m, nil
				}
				m.refreshList()
				m.state = "list"
				return m, nil
			}
			var cmd tea.Cmd
			m.cloneInput, cmd = m.cloneInput.Update(msg)
			return m, cmd
		}

		if m.state == "confirmDelete" {
			switch msg.String() {
			case "y", "Y":
				m.performAction("delete")
				m.refreshList()
				m.state = "list"
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			default:
				m.state = "actions"
			}
			return m, nil
		}

		if m.state == "actions" {
			switch msg.String() {
			case "esc":
				m.state = "list"
				return m, nil
			case "q", "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "enter":
				if item, ok := m.actionsList.SelectedItem().(actionItem); ok {
					switch item.id {
					case "ports":
						m.state = "edit"
						m.edit = newEditModel(m.selected)
						return m, m.edit.Init()
					case "hardware":
						m.state = "hwedit"
						m.hwEdit = newHWEditModel(m.selected)
						return m, m.hwEdit.Init()
					case "clone":
						m.state = "cloneInput"
						m.cloneErr = ""
						m.cloneInput = newCloneInput(m.selected.Name)
						return m, m.cloneInput.Focus()
					case "start", "stop", "restart", "pause", "unpause", "migrate", "snapshot":
						m.performAction(item.id)
						m.refreshList()
						m.state = "list"
						return m, nil
					case "ssh", "viewer", "export":
						actionID := item.id
						postQuitAction = func() { m.performAction(actionID) }
						m.quitting = true
						return m, tea.Quit
					case "console":
						name, ns := m.selectedCT.Name, m.selectedCT.Namespace
						postQuitAction = func() { ct.Console(name, ns) }
						m.quitting = true
						return m, tea.Quit
					case "delete":
						m.state = "confirmDelete"
						return m, nil
					}
				}
			}
			var cmd tea.Cmd
			m.actionsList, cmd = m.actionsList.Update(msg)
			return m, cmd
		}

		if m.state == "doctor" {
			switch msg.String() {
			case "f":
				doctor.Fix()
				m.doctorRows = doctor.Run()
			case "esc", "q", "enter":
				m.state = "list"
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}

		// VM list state
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		case "d":
			m.doctorRows = doctor.Run()
			m.state = "doctor"
			return m, nil
		case "enter":
			switch item := m.list.SelectedItem().(type) {
			case vmItem:
				m.selected = item.vm
				m.isCT = false
				m.actionsList = m.newActionsList()
				m.state = "actions"
				return m, nil
			case ctItem:
				m.selectedCT = item.ct
				m.isCT = true
				m.actionsList = m.newActionsList()
				m.state = "actions"
				return m, nil
			}
		}
	}

	if m.state == "list" {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *tuiModel) performAction(action string) {
	if m.isCT {
		m.performCTAction(action)
		return
	}
	name := m.selected.Name
	backend := m.selected.Backend
	ns := m.selected.Namespace
	if ns == "" {
		ns = "default"
	}

	switch action {
	case "start":
		if backend == "incus" {
			incus.Start(name)
		} else if backend == "kubevirt" {
			kubevirt.NewClient(ns).StartVM(name)
		} else {
			qemu.Start(name)
		}
	case "stop":
		if backend == "incus" {
			incus.Stop(name)
		} else if backend == "kubevirt" {
			kubevirt.NewClient(ns).StopVM(name)
		} else {
			qemu.Stop(name)
		}
	case "restart":
		if backend == "incus" {
			incus.Stop(name)
			incus.Start(name)
		} else if backend == "kubevirt" {
			kubevirt.NewClient(ns).RestartVM(name)
		} else {
			qemu.Stop(name)
			qemu.Start(name)
		}
	case "pause":
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).PauseVM(name)
		}
	case "unpause":
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).UnpauseVM(name)
		}
	case "migrate":
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).Migrate(name, "")
		}
	case "snapshot":
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).Snapshot(name, "")
		}
	case "export":
		if backend == "kubevirt" {
			out, err := kubevirt.NewClient(ns).Export(name, "", "")
			if err != nil {
				fmt.Fprintln(os.Stderr, "export failed:", err)
			} else {
				fmt.Println("Exported to", out)
			}
		}
	case "delete":
		if backend == "incus" {
			incus.Delete(name)
		} else if backend == "kubevirt" {
			kubevirt.NewClient(ns).DeleteVM(name)
		} else {
			qemu.Delete(name)
		}
		if registryStore != nil {
			registryStore.Remove(name)
		}
	case "viewer":
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).Viewer(name)
		} else {
			qemu.Viewer(name)
		}
	case "ssh":
		user, password := "", ""
		if registryStore != nil {
			if entry, ok := registryStore.Get(name); ok {
				user, password = entry.Username, entry.Password
			}
		}
		if user == "" {
			user = os.Getenv("USER")
		}
		if user == "" {
			user = "root"
		}
		if backend == "kubevirt" {
			kubevirt.NewClient(ns).SSH(name, user, "", "", 22, password, nil)
		} else {
			qemu.SSH(name, user, "", "", 22, password, nil)
		}
	}
}

// performCTAction is performAction's CT counterpart — a much smaller
// surface (no backend split, no migrate/snapshot/pause) since a CT is
// always a plain pod, never a hypervisor guest. "console" isn't handled
// here — it's a quit-then-exec action (see the "actions" state's enter
// handler), same as ssh/viewer/export, since it needs the real terminal
// back after Bubble Tea releases it.
func (m *tuiModel) performCTAction(action string) {
	name, ns := m.selectedCT.Name, m.selectedCT.Namespace

	switch action {
	case "start":
		ct.Start(name, ns)
	case "stop":
		ct.Stop(name, ns)
	case "delete":
		ct.Delete(name, ns)
	}
}

// newCloneInput sets up the target-name text input for the Clone action,
// pre-filled with a "-clone" suggestion off the source VM's name.
func newCloneInput(sourceName string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "target VM name"
	ti.SetValue(sourceName + "-clone")
	ti.CharLimit = 63
	ti.Width = 30
	return ti
}

// runClone mirrors cmd/clone.go's logic (kubevirt-only, registers the clone
// in the local registry) so the TUI's Clone action behaves identically to
// `corral clone` — same checks, same errors, just driven by a text input
// instead of positional args.
func runClone(src types.VM, dst string) error {
	if src.Backend != "kubevirt" {
		return fmt.Errorf("cloning is only supported on KubeVirt VMs (VM %q uses backend %q)", src.Name, src.Backend)
	}
	if existing := resolveBackend(dst); existing != "" {
		return fmt.Errorf("target VM %q already exists (backend: %s)", dst, existing)
	}
	ns := src.Namespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	if err := kubevirt.NewClient(ns).Clone(src.Name, dst); err != nil {
		return err
	}
	if registryStore != nil {
		if err := registryStore.Set(dst, types.RegistryEntry{Backend: "kubevirt", Namespace: ns}); err != nil {
			return fmt.Errorf("saving registry entry: %w", err)
		}
	}
	return nil
}

func (m *tuiModel) refreshList() {
	items := []list.Item{}
	vms, _ := kubevirt.NewClient("").ListVMs()
	for _, vm := range vms {
		items = append(items, vmToItem(vm))
	}
	qVMs, _ := qemu.List()
	for _, vm := range qVMs {
		items = append(items, vmToItem(vm))
	}
	cts, _ := ct.ListCTs()
	for _, c := range cts {
		items = append(items, ctToItem(c))
	}
	m.list.SetItems(items)
	m.list.Title = tuiListTitle(items)
}

// tuiListTitle summarizes the fleet in the header: "Corral · 8 VMs · 2 CTs".
func tuiListTitle(items []list.Item) string {
	nVM, nCT := 0, 0
	for _, it := range items {
		if _, ok := it.(ctItem); ok {
			nCT++
		} else {
			nVM++
		}
	}
	t := fmt.Sprintf("Corral  ·  %d VMs", nVM)
	if nCT > 0 {
		t += fmt.Sprintf("  ·  %d CTs", nCT)
	}
	return t + "  ·  d: cluster health"
}

func (m tuiModel) View() string {
	if m.quitting {
		return ""
	}

	if m.state == "edit" {
		return m.edit.View()
	}

	if m.state == "hwedit" {
		return m.hwEdit.View()
	}

	if m.state == "cloneInput" {
		var sb strings.Builder
		sb.WriteString(tuiTitle.Render(fmt.Sprintf(" Clone %s ", m.selected.Name)))
		sb.WriteString("\n\n  Target name: ")
		sb.WriteString(m.cloneInput.View())
		sb.WriteString("\n")
		if m.cloneErr != "" {
			sb.WriteString("\n  " + tuiStopped.Render(m.cloneErr) + "\n")
		}
		sb.WriteString("\n" + tuiHelp.Render("  enter clone · esc cancel"))
		return sb.String()
	}

	if m.state == "doctor" {
		var sb strings.Builder
		sb.WriteString(tuiTitle.Render(" Cluster health "))
		sb.WriteString("\n\n")
		fixable := false
		for _, c := range m.doctorRows {
			mark := tuiRunning.Render("●")
			if !c.OK {
				mark = tuiStopped.Render("○")
			}
			tag := ""
			if !c.OK && c.Fixable {
				tag = tuiRunning.Render("  (fixable)")
				fixable = true
			}
			sb.WriteString(fmt.Sprintf("  %s %-30s %s%s\n", mark, c.Name, tuiHelp.Render(c.Detail), tag))
		}
		help := "  esc back"
		if fixable {
			help = "  f reconcile fixable   esc back"
		}
		sb.WriteString("\n" + tuiHelp.Render(help))
		return sb.String()
	}

	if m.state == "confirmDelete" {
		name, what := m.selected.Name, "and its disks"
		if m.isCT {
			name, what = m.selectedCT.Name, "and its data volume"
		}
		return fmt.Sprintf("\n  %s\n\n  %s\n",
			tuiTitle.Render(fmt.Sprintf(" Delete %s %s? ", name, what)),
			tuiHelp.Render("y confirm  any other key cancel"))
	}

	if m.state == "actions" {
		return m.actionsList.View()
	}

	return m.list.View()
}

// ── Actions list delegate ─────────────────────────────────────────

type actionItemDelegate struct{}

func (d actionItemDelegate) Height() int                               { return 1 }
func (d actionItemDelegate) Spacing() int                              { return 1 }
func (d actionItemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d actionItemDelegate) Render(w io.Writer, m list.Model, index int, li list.Item) {
	a, ok := li.(actionItem)
	if !ok {
		return
	}

	label := a.label
	if index == m.Index() {
		label = tuiRunning.Render("▶ " + label)
	} else {
		label = "  " + label
	}
	fmt.Fprint(w, label)
}

// ── Edit model (port toggles) ─────────────────────────────────────

type editModel struct {
	vm       types.VM
	ports    []int
	toggled  map[int]bool
	cursor   int
	done     bool
	addInput textinput.Model
	adding   bool
}

func newEditModel(vm types.VM) editModel {
	current := exposedPorts(vm.Name, vm.Namespace)
	toggled := make(map[int]bool)
	for _, p := range current {
		toggled[p] = true
	}

	allPorts := append([]int{}, types.DefaultPorts...)
	for _, p := range current {
		found := false
		for _, dp := range types.DefaultPorts {
			if p == dp {
				found = true
				break
			}
		}
		if !found {
			allPorts = append(allPorts, p)
		}
	}

	ti := textinput.New()
	ti.Placeholder = "port number"
	ti.CharLimit = 5

	return editModel{
		vm:       vm,
		ports:    allPorts,
		toggled:  toggled,
		addInput: ti,
	}
}

func (m editModel) Init() tea.Cmd { return nil }

func (m editModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.adding {
		return m.updateAdding(msg)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.done = true
		case "q", "ctrl+c":
			m.done = true
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.ports) {
				m.cursor++
			}
		case " ", "enter":
			if m.cursor < len(m.ports) {
				p := m.ports[m.cursor]
				m.toggled[p] = !m.toggled[p]
				m.applyPorts()
			} else if m.cursor == len(m.ports) {
				m.adding = true
				m.addInput.Focus()
				return m, textinput.Blink
			} else if m.cursor == len(m.ports)+1 {
				m.toggled = make(map[int]bool)
				m.applyPorts()
			}
		case "backspace":
			if m.cursor < len(m.ports) {
				p := m.ports[m.cursor]
				isDefault := false
				for _, dp := range types.DefaultPorts {
					if p == dp {
						isDefault = true
						break
					}
				}
				if !isDefault && m.toggled[p] {
					delete(m.toggled, p)
					m.applyPorts()
				}
			}
		}
	}
	return m, nil
}

func (m editModel) updateAdding(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if port, err := strconv.Atoi(m.addInput.Value()); err == nil && port > 0 && port < 65536 {
				m.ports = append(m.ports, port)
				m.toggled[port] = true
				m.applyPorts()
			}
			m.adding = false
			m.addInput.Reset()
		case "esc":
			m.adding = false
			m.addInput.Reset()
		}
	}
	var cmd tea.Cmd
	m.addInput, cmd = m.addInput.Update(msg)
	return m, cmd
}

func (m *editModel) applyPorts() {
	var enabled []int
	for p, on := range m.toggled {
		if on {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 0 {
		kubevirt.DeleteProxy(m.vm.Name, m.vm.Namespace)
	} else {
		kubevirt.ApplyProxy(m.vm.Name, m.vm.Namespace, enabled)
	}
}

func (m editModel) View() string {
	var sb strings.Builder
	host := m.vm.Name + "-vm.manatee-basking.ts.net"
	sb.WriteString(tuiTitle.Render(fmt.Sprintf(" Ports: %s ", host)))
	sb.WriteString("\n\n")

	for i, p := range m.ports {
		cursor := "  "
		if i == m.cursor {
			cursor = tuiRunning.Render("▶ ")
		}
		mark := "[OFF]"
		if m.toggled[p] {
			mark = tuiRunning.Render("[ON]")
		}
		label := fmt.Sprintf("port %d", p)
		for proto, port := range types.PortMap {
			if port == p {
				label = fmt.Sprintf("%s (%d)", proto, p)
				break
			}
		}
		sb.WriteString(fmt.Sprintf("%s%-20s  %s\n", cursor, label, mark))
	}

	cursor := "  "
	if m.cursor == len(m.ports) {
		cursor = tuiRunning.Render("▶ ")
	}
	sb.WriteString(fmt.Sprintf("%s+ Add port...\n", cursor))

	cursor = "  "
	if m.cursor == len(m.ports)+1 {
		cursor = tuiRunning.Render("▶ ")
	}
	if len(m.toggled) > 0 {
		sb.WriteString(fmt.Sprintf("%s✕ Remove all ports\n", cursor))
	}

	if m.adding {
		sb.WriteString(fmt.Sprintf("\n  Port: %s", m.addInput.View()))
	}

	sb.WriteString("\n" + tuiHelp.Render("  space toggle  ↑↓ nav  backspace remove  esc back"))
	return sb.String()
}

// ── Hardware edit (CPU / RAM) ─────────────────────────────────────

type hwEditModel struct {
	vm     types.VM
	cpu    textinput.Model
	mem    textinput.Model
	focus  int // 0 = cpu, 1 = mem
	status string
	done   bool
}

func newHWEditModel(vm types.VM) hwEditModel {
	cpu := textinput.New()
	cpu.SetValue(strconv.Itoa(vm.CPU))
	cpu.CharLimit = 3
	cpu.Width = 6
	cpu.Focus()

	mem := textinput.New()
	mem.SetValue(vm.Mem)
	mem.CharLimit = 8
	mem.Width = 8

	return hwEditModel{vm: vm, cpu: cpu, mem: mem}
}

func (m hwEditModel) Init() tea.Cmd { return textinput.Blink }

func (m hwEditModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.done = true
			return m, nil
		case "tab", "up", "down", "shift+tab":
			m.focus = (m.focus + 1) % 2
			if m.focus == 0 {
				m.cpu.Focus()
				m.mem.Blur()
			} else {
				m.mem.Focus()
				m.cpu.Blur()
			}
			return m, textinput.Blink
		case "enter":
			m.apply()
			m.done = true
			return m, nil
		}
	}
	var cmd tea.Cmd
	if m.focus == 0 {
		m.cpu, cmd = m.cpu.Update(msg)
	} else {
		m.mem, cmd = m.mem.Update(msg)
	}
	return m, cmd
}

func (m *hwEditModel) apply() {
	ns := m.vm.Namespace
	if ns == "" {
		ns = "default"
	}
	c := kubevirt.NewClient(ns)
	if v, err := strconv.Atoi(strings.TrimSpace(m.cpu.Value())); err == nil && v > 0 && v != m.vm.CPU {
		c.ScaleCPU(m.vm.Name, v)
	}
	if mem := strings.TrimSpace(m.mem.Value()); mem != "" && mem != m.vm.Mem {
		c.ScaleMemory(m.vm.Name, mem)
	}
}

func (m hwEditModel) View() string {
	var sb strings.Builder
	sb.WriteString(tuiTitle.Render(fmt.Sprintf(" Edit hardware: %s ", m.vm.Name)))
	sb.WriteString("\n\n")
	cpuMark, memMark := "  ", "  "
	if m.focus == 0 {
		cpuMark = tuiRunning.Render("▶ ")
	} else {
		memMark = tuiRunning.Render("▶ ")
	}
	sb.WriteString(fmt.Sprintf("%svCPUs   %s\n", cpuMark, m.cpu.View()))
	sb.WriteString(fmt.Sprintf("%sMemory  %s\n", memMark, m.mem.View()))
	note := "applies live (hotplug)"
	if !m.vm.LiveMigratable {
		note = "VM will restart to apply"
	}
	sb.WriteString("\n" + tuiHelp.Render("  "+note))
	sb.WriteString("\n" + tuiHelp.Render("  tab switch  enter apply  esc cancel"))
	return sb.String()
}

// ── Proxy helpers ─────────────────────────────────────────────────

func exposedPorts(name, ns string) []int {
	return kubevirt.ExposedPorts(name, ns)
}

// ── List delegates ────────────────────────────────────────────────

type vmItemDelegate struct{}

func (d vmItemDelegate) Height() int                               { return 2 }
func (d vmItemDelegate) Spacing() int                              { return 0 }
func (d vmItemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d vmItemDelegate) Render(w io.Writer, m list.Model, index int, li list.Item) {
	// The list mixes vmItem and ctItem (both satisfy list.Item) — render via
	// the shared shape, or CTs silently occupy invisible rows.
	i, ok := li.(interface {
		Title() string
		Description() string
	})
	if !ok {
		return
	}

	name := i.Title()
	desc := i.Description()

	if index == m.Index() {
		name = tuiRunning.Render("▶ " + name)
	} else {
		name = "  " + name
	}

	fmt.Fprintf(w, "%s\n%s", name, tuiHelp.Render("  "+desc))
}
