package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/types"
)

// The TUI create form mirrors the web "Create VM" dialog: the same source
// types, the same per-backend field visibility, and the same underlying create
// path (executeCreate, shared with `corral create`). Fields that the web VM
// form also omits (storageClass) or that default cleanly (the SSH key, loaded
// from disk) are not shown here; everything else the web offers is.

// crAction is what the create form asks the parent tuiModel to do after a key.
type crAction int

const (
	crNone crAction = iota
	crSubmit
	crCancel
)

// crRow identifies one navigable line in the form. The visible set is computed
// each frame from the chosen backend and source type (see visibleRows).
type createModel struct {
	targets   []config.ContextConfig
	targetIdx int

	stIdx     int // index into sourceTypes() for the current backend
	catImages []catalog.Image
	catIdx    int
	bootcOK   bool // KubeVirt bootc plugin compiled in → offer the bootc source

	name      textinput.Model
	source    textinput.Model // containerDisk/import/iso/bootc/windows/pvc value
	cpu       textinput.Model
	mem       textinput.Model
	disk      textinput.Model
	namespace textinput.Model
	node      textinput.Model
	itype     textinput.Model
	pref      textinput.Model
	cloudinit textinput.Model

	cursor int // index into visibleRows()
	err    string
}

func newCreateModel() createModel {
	mk := func(placeholder, val string, limit int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		if val != "" {
			ti.SetValue(val)
		}
		ti.CharLimit = limit
		return ti
	}
	c := createModel{
		targets:   config.Contexts(),
		catImages: catalog.Images,
		bootcOK:   kubevirt.BootcAvailable(),
		name:      mk("vm name (a-z0-9-)", "", 63),
		source:    mk("", "", 512),
		cpu:       mk("2", "2", 3),
		mem:       mk("4G", "4G", 8),
		disk:      mk("20G", "20G", 8),
		namespace: mk(kubevirt.DefaultNamespace, "", 63),
		node:      mk("any node", "", 63),
		itype:     mk("cluster instancetype (optional)", "", 63),
		pref:      mk("cluster preference (optional)", "", 63),
		cloudinit: mk("extra cloud-init user-data (single line)", "", 512),
	}
	// Start on the default context so the common case is one keystroke away.
	def := config.DefaultContext().Name
	for i, t := range c.targets {
		if t.Name == def {
			c.targetIdx = i
			break
		}
	}
	return c
}

// backend returns the backend of the currently selected target.
func (c createModel) backend() string {
	if len(c.targets) == 0 {
		return "qemu"
	}
	return c.targets[c.targetIdx].Backend
}

// sourceTypes lists the source types the current backend accepts, in the same
// order and with the same gating the web form uses (updateSourceFields).
func (c createModel) sourceTypes() []string {
	switch c.backend() {
	case "qemu", "libvirt":
		return []string{"iso", "import"}
	case "incus":
		return []string{"containerDisk", "import"}
	default: // kubevirt
		sts := []string{"catalog", "containerDisk", "import", "iso"}
		if c.bootcOK {
			sts = append(sts, "bootc")
		}
		return append(sts, "windows", "pvc")
	}
}

func (c createModel) sourceType() string {
	sts := c.sourceTypes()
	if c.stIdx >= len(sts) {
		return sts[0]
	}
	return sts[c.stIdx]
}

// visibleRows is the ordered list of navigable keys for the current state.
func (c createModel) visibleRows() []string {
	kube := c.backend() == "kubevirt"
	st := c.sourceType()
	rows := []string{"name", "target", "source_type"}
	if kube && st == "catalog" {
		rows = append(rows, "catalog")
	}
	switch st {
	case "containerDisk", "import", "iso", "bootc", "windows", "pvc":
		rows = append(rows, "source")
	}
	rows = append(rows, "cpu", "mem", "disk")
	if kube {
		rows = append(rows, "namespace", "node", "instancetype", "preference", "cloudinit")
	}
	rows = append(rows, "submit")
	return rows
}

// tiFor returns the text input backing a row key, or nil for non-text rows.
func (c *createModel) tiFor(key string) *textinput.Model {
	switch key {
	case "name":
		return &c.name
	case "source":
		return &c.source
	case "cpu":
		return &c.cpu
	case "mem":
		return &c.mem
	case "disk":
		return &c.disk
	case "namespace":
		return &c.namespace
	case "node":
		return &c.node
	case "instancetype":
		return &c.itype
	case "preference":
		return &c.pref
	case "cloudinit":
		return &c.cloudinit
	}
	return nil
}

// syncFocus focuses the text input under the cursor (blurring the rest) so the
// blinking caret marks the active field. Returns the Blink cmd when a text row
// is focused.
func (c *createModel) syncFocus() tea.Cmd {
	rows := c.visibleRows()
	if c.cursor >= len(rows) {
		c.cursor = len(rows) - 1
	}
	active := rows[c.cursor]
	for _, k := range []string{"name", "source", "cpu", "mem", "disk", "namespace", "node", "instancetype", "preference", "cloudinit"} {
		ti := c.tiFor(k)
		if k == active {
			ti.Focus()
		} else {
			ti.Blur()
		}
	}
	if c.tiFor(active) != nil {
		return textinput.Blink
	}
	return nil
}

// clampSourceType keeps stIdx valid after the backend changes (each backend
// offers a different source-type list).
func (c *createModel) clampSourceType() {
	if c.stIdx >= len(c.sourceTypes()) {
		c.stIdx = 0
	}
}

func (c createModel) Update(msg tea.KeyPressMsg) (createModel, tea.Cmd, crAction) {
	rows := c.visibleRows()
	if c.cursor >= len(rows) {
		c.cursor = len(rows) - 1
	}
	key := rows[c.cursor]
	s := msg.String()

	switch s {
	case "ctrl+c", "esc":
		return c, nil, crCancel
	case "up", "shift+tab":
		c.cursor = (c.cursor - 1 + len(rows)) % len(rows)
		return c, c.syncFocus(), crNone
	case "down", "tab":
		c.cursor = (c.cursor + 1) % len(rows)
		return c, c.syncFocus(), crNone
	}

	switch key {
	case "target":
		switch s {
		case "left", "h":
			c.targetIdx = (c.targetIdx - 1 + len(c.targets)) % len(c.targets)
			c.clampSourceType()
		case "right", "l":
			c.targetIdx = (c.targetIdx + 1) % len(c.targets)
			c.clampSourceType()
		}
		return c, nil, crNone
	case "source_type":
		sts := c.sourceTypes()
		switch s {
		case "left", "h":
			c.stIdx = (c.stIdx - 1 + len(sts)) % len(sts)
		case "right", "l":
			c.stIdx = (c.stIdx + 1) % len(sts)
		}
		return c, nil, crNone
	case "catalog":
		switch s {
		case "left", "h":
			c.catIdx = (c.catIdx - 1 + len(c.catImages)) % len(c.catImages)
		case "right", "l":
			c.catIdx = (c.catIdx + 1) % len(c.catImages)
		}
		return c, nil, crNone
	case "submit":
		if s == "enter" {
			return c, nil, crSubmit
		}
		return c, nil, crNone
	default:
		// A text row. Enter advances to the next field (keyboard-friendly);
		// everything else edits the focused input.
		if s == "enter" {
			c.cursor = (c.cursor + 1) % len(rows)
			return c, c.syncFocus(), crNone
		}
		if ti := c.tiFor(key); ti != nil {
			var cmd tea.Cmd
			*ti, cmd = ti.Update(msg)
			return c, cmd, crNone
		}
		return c, nil, crNone
	}
}

func (c createModel) render(th theme, width int) string {
	var b strings.Builder
	b.WriteString(th.title.Render(" Create VM "))
	b.WriteString("\n\n")

	rows := c.visibleRows()
	st := c.sourceType()
	for i, key := range rows {
		cursor := "  "
		if i == c.cursor {
			cursor = th.key.Render("> ")
		}
		var line string
		switch key {
		case "name":
			line = "Name          " + c.name.View()
		case "target":
			t := c.targets[c.targetIdx]
			val := fmt.Sprintf("%s · %s", t.Name, t.Backend)
			if t.Context != "" {
				val += " · " + t.Context
			}
			line = "Target        " + selectView(th, val)
		case "source_type":
			line = "Source type   " + selectView(th, sourceTypeLabel(st))
		case "catalog":
			img := c.catImages[c.catIdx]
			line = "Image         " + selectView(th, img.Name) + "  " + th.muted.Render(img.Description)
		case "source":
			line = sourceLabel(st) + "  " + c.source.View()
			if hint := sourceHint(st); hint != "" {
				line += "\n                " + th.muted.Render(hint)
			}
		case "cpu":
			line = "CPU cores     " + c.cpu.View()
		case "mem":
			line = "Memory        " + c.mem.View()
		case "disk":
			line = "Disk          " + c.disk.View()
			if c.backend() == "incus" {
				line += "  " + th.muted.Render("(ignored by incus)")
			}
		case "namespace":
			line = "Namespace     " + c.namespace.View()
		case "node":
			line = "Node          " + c.node.View()
		case "instancetype":
			line = "InstanceType  " + c.itype.View()
		case "preference":
			line = "Preference    " + c.pref.View()
		case "cloudinit":
			line = "Cloud-init    " + c.cloudinit.View()
		case "submit":
			label := " Create VM "
			if i == c.cursor {
				line = th.ok.Render("[" + label + "]")
			} else {
				line = th.muted.Render("[" + label + "]")
			}
		}
		b.WriteString(cursor + line + "\n")
	}

	if c.err != "" {
		b.WriteString("\n  " + th.danger.Render(c.err) + "\n")
	}
	b.WriteString("\n" + th.help.Render("  ↑/↓ move · ←/→ change · enter next / create · esc cancel"))
	return b.String()
}

func selectView(th theme, val string) string {
	return th.key.Render("‹ ") + val + th.key.Render(" ›")
}

func sourceTypeLabel(st string) string {
	switch st {
	case "catalog":
		return "OS image (catalog)"
	case "containerDisk":
		return "container disk image"
	case "import":
		return "import qcow2/raw (URL)"
	case "iso":
		return "installer ISO (URL/path)"
	case "bootc":
		return "bootc image (built on-cluster)"
	case "windows":
		return "Windows (UEFI/TPM, installer ISO)"
	case "pvc":
		return "existing PVC"
	}
	return st
}

func sourceLabel(st string) string {
	switch st {
	case "containerDisk":
		return "Container    "
	case "import":
		return "Import URL   "
	case "iso":
		return "ISO          "
	case "bootc":
		return "Bootc image  "
	case "windows":
		return "Windows ISO  "
	case "pvc":
		return "PVC          "
	}
	return "Source       "
}

func sourceHint(st string) string {
	switch st {
	case "containerDisk":
		return "e.g. quay.io/containerdisks/fedora:42"
	case "import":
		return "qcow2/raw image URL"
	case "iso":
		return "installer ISO — local path (qemu) or URL"
	case "bootc":
		return "e.g. ghcr.io/tuna-os/yellowfin:gnome"
	case "windows":
		return "Windows installer ISO URL"
	case "pvc":
		return "existing PVC / DataVolume name"
	}
	return ""
}

// applyCreate turns the form into a VM. It populates the create* package vars
// (the same ones `corral create`'s flags set) and dispatches through the shared
// executeCreate, so the TUI and CLI create paths never diverge. Windows is the
// one source type executeCreate does not cover, so it is handled directly.
func applyCreate(c createModel) error {
	name := strings.TrimSpace(c.name.Value())
	if name == "" {
		return fmt.Errorf("name can't be empty")
	}
	if len(c.targets) == 0 {
		return fmt.Errorf("no target context configured")
	}

	// The TUI reuses these globals across creates and — unlike the CLI — cobra
	// never populated their flag defaults, so start from a clean slate.
	resetCreateVars()

	cpu, err := strconv.Atoi(strings.TrimSpace(c.cpu.Value()))
	if err != nil || cpu < 1 {
		cpu = 2
	}
	createCPU = cpu
	createMem = createValueOr(c.mem.Value(), "4G")
	createDisk = createValueOr(c.disk.Value(), "20G")

	target := c.targets[c.targetIdx]
	rootBackend = target.Backend
	rootContext = target.Name
	switch target.Backend {
	case "kubevirt":
		createKubevirt = true
		createNamespace = strings.TrimSpace(c.namespace.Value())
		createNode = strings.TrimSpace(c.node.Value())
		createInstanceType = strings.TrimSpace(c.itype.Value())
		createPreference = strings.TrimSpace(c.pref.Value())
		createCloudInit = strings.TrimSpace(c.cloudinit.Value())
	case "incus":
		createIncus = true
	}

	src := strings.TrimSpace(c.source.Value())
	switch c.sourceType() {
	case "catalog":
		createImage = c.catImages[c.catIdx].Name
	case "containerDisk":
		createContainerDisk = src
	case "import":
		createImport = src
	case "iso":
		createISO = src
	case "bootc":
		createBootc = src
	case "pvc":
		createPVC = src
	case "windows":
		return createWindowsFromTUI(name, target, src)
	}

	// kubevirtExplicit=true: the target picker is an explicit backend choice,
	// so bootc respects it instead of auto-detecting a cluster.
	return executeCreate(name, true)
}

// createWindowsFromTUI mirrors the web's guided Windows install (UEFI+TPM),
// which executeCreate does not handle.
func createWindowsFromTUI(name string, target config.ContextConfig, iso string) error {
	if iso == "" {
		return fmt.Errorf("a Windows installer ISO URL is required")
	}
	ns := createNamespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	password, err := kubevirt.CreateWindowsVMInContext(name, ns, iso, createDisk, createMem, createCPU, true, target.Context)
	if err != nil {
		return err
	}
	if registryStore != nil {
		return registryStore.Set(name, types.RegistryEntry{
			Backend:   "kubevirt",
			Namespace: ns,
			Username:  "Administrator",
			Password:  password,
		})
	}
	return nil
}

func createValueOr(v, def string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return def
}

// resetCreateVars returns every create* dispatch var to its zero value so a
// TUI create never inherits a previous one's leftovers.
func resetCreateVars() {
	createKubevirt, createIncus = false, false
	createFirmware = ""
	createMem, createCPU, createDisk = "", 0, ""
	createISO, createQCOW = "", ""
	createForce = false
	createContainerDisk, createImage, createImport, createPVC = "", "", "", ""
	createNamespace, createNode = "", ""
	createCloudInitPassword, createCloudInit = "", ""
	createInstanceType, createPreference = "", ""
	createFile, createStorageClass, createBootc = "", "", ""
	createStart, createWaitSSH = false, false
	createEphemeral, createTTL = false, ""
	createLAN, createNetworkNAD, createBridgeIface, createLANService = false, "", "", false
	rootBackend, rootContext = "", ""
}
