package proxmoxbe

// The operations, one per row of the parity matrix.
//
// Every one of them resolves a name to a guest first, which is what lets a
// caller pass the name a human uses while PVE gets the vmid and node it needs.
// Each mutation returns its Task so a surface can wait, poll, or ignore it —
// the choice belongs to the caller, not to the backend.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a wait a caller did not size itself.
const DefaultTimeout = 5 * time.Minute

// ── power ─────────────────────────────────────────────────────────

func (c *Client) statusAction(name, action string, params map[string]string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	raw, err := c.post(guest.path()+"/status/"+action, form(params))
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// Start boots a guest.
func (c *Client) Start(name string) (Task, error) {
	return c.statusAction(name, "start", nil)
}

// Stop shuts a guest down cleanly. PVE's /stop is a power cut; /shutdown asks
// the guest, which is what every other Corral backend's Stop means. The forced
// path is Kill, so the destructive one has to be chosen by name.
func (c *Client) Stop(name string) (Task, error) {
	return c.statusAction(name, "shutdown", nil)
}

// Kill pulls the plug. Corral's Stop never escalates to this on its own: an
// unclean shutdown of somebody's database is not a fallback to apply quietly.
func (c *Client) Kill(name string) (Task, error) {
	return c.statusAction(name, "stop", nil)
}

// Restart reboots the guest through PVE rather than stop-then-start. The
// stop-then-start dance other backends do loses the guest's own shutdown
// ordering and races with anything that autostarts.
func (c *Client) Restart(name string) (Task, error) {
	return c.statusAction(name, "reboot", nil)
}

// Pause suspends a running guest to memory.
func (c *Client) Pause(name string) (Task, error) {
	return c.statusAction(name, "suspend", nil)
}

// Resume unpauses.
func (c *Client) Resume(name string) (Task, error) {
	return c.statusAction(name, "resume", nil)
}

// Delete removes a guest. purge=1 so its backup jobs and HA entries go with it —
// leaving orphaned jobs pointing at a vmid that will be reused later is how a
// backup silently starts capturing somebody else's VM.
func (c *Client) Delete(name string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	raw, err := c.delete(guest.path()+"?purge=1&destroy-unreferenced-disks=1", nil)
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// ── status and metrics ────────────────────────────────────────────

// Status is a guest's live state, the /status/current shape.
type Status struct {
	Status    string  `json:"status"`
	CPU       float64 `json:"cpu"`     // fraction of assigned cores, 0..1
	CPUs      int     `json:"cpus"`    // assigned cores
	Mem       int64   `json:"mem"`     // bytes in use
	MaxMem    int64   `json:"maxmem"`  // bytes assigned
	MaxDisk   int64   `json:"maxdisk"` // bytes
	Uptime    int64   `json:"uptime"`
	QMPStatus string  `json:"qmpstatus"` // "running" | "paused" | …
	Agent     int     `json:"agent"`     // guest agent configured
	Lock      string  `json:"lock"`
	Template  int     `json:"template"`
	HA        struct {
		Managed int `json:"managed"`
	} `json:"ha"`
}

// Paused reports a guest suspended to memory. PVE reports this in qmpstatus
// rather than status, which is why Corral cannot read it off the resource list.
func (s Status) Paused() bool { return s.QMPStatus == "paused" }

// AgentConnected reports whether the guest agent is configured and answering.
func (s Status) AgentConnected() bool { return s.Agent == 1 }

// Status fetches a guest's live state.
func (c *Client) Status(name string) (Status, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Status{}, err
	}
	var status Status
	err = c.get(guest.path()+"/status/current", &status)
	return status, err
}

// Metrics returns the instantaneous CPU and memory in the shape the web UI's
// live-usage row expects ("240m", "1.5Gi").
func (c *Client) Metrics(name string) (map[string]string, error) {
	status, err := c.Status(name)
	if err != nil {
		return nil, err
	}
	cores := status.CPUs
	if cores == 0 {
		cores = 1
	}
	return map[string]string{
		"cpu": strconv.Itoa(int(status.CPU*float64(cores)*1000)) + "m",
		"mem": humanBytes(status.Mem),
	}, nil
}

// CPUSample is one point of the CPU history the sparkline draws.
type CPUSample struct {
	Time int64   `json:"time"`
	CPU  float64 `json:"cpu"`
}

// CPUHistory returns the last hour of CPU usage as percentages. PVE keeps RRD
// data per guest, so the sparkline has real history the moment a guest is added
// — where the KubeVirt path has to sample and accumulate its own.
func (c *Client) CPUHistory(name string) ([]CPUSample, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Time int64   `json:"time"`
		CPU  float64 `json:"cpu"`
	}
	if err := c.get(guest.path()+"/rrddata?timeframe=hour&cf=AVERAGE", &raw); err != nil {
		return nil, err
	}
	out := make([]CPUSample, 0, len(raw))
	for _, r := range raw {
		out = append(out, CPUSample{Time: r.Time, CPU: r.CPU * 100})
	}
	return out, nil
}

// ── shape ─────────────────────────────────────────────────────────

// Scale sets cores and memory. Where the guest has hotplug enabled PVE applies
// them live; where it does not, the change lands on next boot — the same honest
// "applies live / will restart to apply" note the hardware form already shows,
// and PVE tells us which by whether the config's hotplug field covers it.
func (c *Client) Scale(name string, cores int, mem string) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	params := map[string]string{}
	if cores > 0 {
		params["cores"] = strconv.Itoa(cores)
	}
	if mem != "" {
		mib, err := memoryMiB(mem)
		if err != nil {
			return err
		}
		params["memory"] = strconv.Itoa(mib)
	}
	if len(params) == 0 {
		return nil
	}
	return c.put(guest.path()+"/config", form(params))
}

// memoryMiB converts Corral's memory strings to the MiB integer PVE wants.
func memoryMiB(mem string) (int, error) {
	lower := strings.ToLower(strings.TrimSpace(mem))
	multiplier := 1
	switch {
	case strings.HasSuffix(lower, "gi"), strings.HasSuffix(lower, "g"), strings.HasSuffix(lower, "gb"):
		multiplier = 1024
	case strings.HasSuffix(lower, "mi"), strings.HasSuffix(lower, "m"), strings.HasSuffix(lower, "mb"):
		multiplier = 1
	case isAllDigits(lower):
		// A bare number is MiB, which is what PVE itself means by memory.
		multiplier = 1
	default:
		return 0, fmt.Errorf("proxmox: cannot read memory %q; use e.g. 4Gi, 2048Mi, or 2048", mem)
	}
	digits := strings.TrimRight(lower, "gimb")
	value, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return 0, fmt.Errorf("proxmox: cannot read memory %q: %w", mem, err)
	}
	mib := int(value * float64(multiplier))
	if mib <= 0 {
		return 0, fmt.Errorf("proxmox: memory %q is not a positive size", mem)
	}
	return mib, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// GuestConfig is the subset of a guest's configuration Corral reads. It is not
// called Config: that name belongs to this package's own cluster configuration,
// and a backend where "config" means two things is a backend nobody can read.
type GuestConfig struct {
	Name    string `json:"name"`
	Cores   int    `json:"cores"`
	Memory  int    `json:"memory"`
	Tags    string `json:"tags"`
	Hotplug string `json:"hotplug"`
	Agent   string `json:"agent"`
	OSType  string `json:"ostype"`
	Boot    string `json:"boot"`
	// Disks and NICs arrive as scsi0/virtio0/net0/hostpci0 keys, which do not
	// fit a struct; Raw keeps them for the callers that walk them.
	Raw map[string]any `json:"-"`
}

// HotplugsMemory reports whether a memory change applies without a reboot.
func (cfg GuestConfig) HotplugsMemory() bool {
	return strings.Contains(cfg.Hotplug, "memory")
}

// HotplugsCPU reports whether a core change applies without a reboot.
func (cfg GuestConfig) HotplugsCPU() bool {
	return strings.Contains(cfg.Hotplug, "cpu")
}

// GuestConfig fetches a guest's configuration.
func (c *Client) GuestConfig(name string) (GuestConfig, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return GuestConfig{}, err
	}
	var raw map[string]any
	if err := c.get(guest.path()+"/config", &raw); err != nil {
		return GuestConfig{}, err
	}
	cfg := GuestConfig{Raw: raw}
	cfg.Name, _ = raw["name"].(string)
	cfg.Tags, _ = raw["tags"].(string)
	cfg.Hotplug, _ = raw["hotplug"].(string)
	cfg.Agent, _ = raw["agent"].(string)
	cfg.OSType, _ = raw["ostype"].(string)
	cfg.Boot, _ = raw["boot"].(string)
	if v, ok := raw["cores"].(float64); ok {
		cfg.Cores = int(v)
	}
	if v, ok := raw["memory"].(float64); ok {
		cfg.Memory = int(v)
	}
	return cfg, nil
}

// ExpandDisk grows a disk. PVE takes a signed size ("+10G") and refuses to
// shrink, which is the behaviour Corral wants anyway.
func (c *Client) ExpandDisk(name, disk, size string) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(size, "+") {
		// A bare size would be read as an absolute target and rejected for
		// anything smaller than the current disk. Growth is the only operation
		// Corral offers, so the sign is added rather than surfaced.
		size = "+" + size
	}
	return c.put(guest.path()+"/resize", form(map[string]string{"disk": disk, "size": size}))
}

// AddDisk attaches a new disk on a storage, sized like "10G".
func (c *Client) AddDisk(name, storage, slot, size string) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	if slot == "" {
		slot = "scsi1"
	}
	return c.put(guest.path()+"/config",
		form(map[string]string{slot: fmt.Sprintf("%s:%s", storage, strings.TrimSuffix(size, "G"))}))
}

// RemoveDisk detaches and deletes a disk.
func (c *Client) RemoveDisk(name, slot string) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	return c.put(guest.path()+"/config", form(map[string]string{"delete": slot}))
}

// AttachGPU passes a host PCI device through.
func (c *Client) AttachGPU(name, pciID, slot string) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	if slot == "" {
		slot = "hostpci0"
	}
	return c.put(guest.path()+"/config", form(map[string]string{slot: pciID + ",pcie=1"}))
}

// DetachGPU removes a passthrough device.
func (c *Client) DetachGPU(name, slot string) error {
	if slot == "" {
		slot = "hostpci0"
	}
	return c.RemoveDisk(name, slot) // same config delete
}

// ── tags and templates ────────────────────────────────────────────

// SetTag adds or removes a tag. PVE stores tags as one comma-separated string,
// so this reads, edits, and writes it back — the read is what keeps a second
// tag from erasing the first.
func (c *Client) SetTag(name, tag string, on bool) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	cfg, err := c.GuestConfig(name)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, t := range strings.FieldsFunc(cfg.Tags, func(r rune) bool { return r == ',' || r == ';' }) {
		if t = strings.TrimSpace(t); t != "" {
			existing[t] = true
		}
	}
	if on {
		existing[tag] = true
	} else {
		delete(existing, tag)
	}
	tags := make([]string, 0, len(existing))
	for t := range existing {
		tags = append(tags, t)
	}
	sortStrings(tags)
	// An empty value clears the field, which is what removing the last tag has
	// to do; form() drops empty values, so this one is set directly.
	values := form(nil)
	values.Set("tags", strings.Join(tags, ","))
	return c.put(guest.path()+"/config", values)
}

// MarkTemplate converts a guest into a template. PVE templates are one-way: a
// template cannot be converted back, so the "unmark" half of the web UI's
// button has no counterpart here and the refusal says why rather than failing
// with a PVE parameter error.
func (c *Client) MarkTemplate(name string, on bool) error {
	guest, err := c.Resolve(name)
	if err != nil {
		return err
	}
	if !on {
		return fmt.Errorf("proxmox: a template cannot be converted back into a guest; " +
			"clone it and delete the template instead")
	}
	_, err = c.post(guest.path()+"/template", nil)
	return err
}

// ── movement ──────────────────────────────────────────────────────

// MigratePrecondition is what PVE says about a migration before it is attempted.
// Corral currently guesses live-migratability from a KubeVirt condition; PVE will
// simply tell us, including the local disks that would block it.
type MigratePrecondition struct {
	Running          int      `json:"running"`
	AllowedNodes     []string `json:"allowed_nodes"`
	NotAllowedNodes  any      `json:"not_allowed_nodes"`
	LocalDisks       []any    `json:"local_disks"`
	LocalResources   []string `json:"local_resources"`
	MappedResources  []string `json:"mapped_resources"`
	MappedResourceOK any      `json:"mapped_resource_info"`
}

// CanLiveMigrate reports whether a live migration is possible right now, and why
// not when it is not.
func (p MigratePrecondition) CanLiveMigrate() (bool, string) {
	if len(p.LocalResources) > 0 {
		return false, "the guest uses local resources: " + strings.Join(p.LocalResources, ", ")
	}
	if len(p.AllowedNodes) == 0 {
		return false, "no other node can accept this guest"
	}
	return true, ""
}

// MigratePreconditions asks PVE what a migration would do.
func (c *Client) MigratePreconditions(name string) (MigratePrecondition, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return MigratePrecondition{}, err
	}
	if guest.Kind == KindLXC {
		// PVE has no precondition endpoint for containers.
		return MigratePrecondition{}, fmt.Errorf("proxmox: migration preconditions are a VM-only query")
	}
	var pre MigratePrecondition
	err = c.get(guest.path()+"/migrate", &pre)
	return pre, err
}

// Migrate moves a guest to another node, live when it is running. An empty
// target asks PVE for the first node it says would accept the guest, so
// "migrate" with no argument behaves the way it does on other backends.
func (c *Client) Migrate(name, target string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	status, err := c.Status(name)
	if err != nil {
		return Task{}, err
	}
	if target == "" {
		if guest.Kind == KindQemu {
			pre, err := c.MigratePreconditions(name)
			if err != nil {
				return Task{}, err
			}
			if ok, why := pre.CanLiveMigrate(); !ok && status.Status == "running" {
				return Task{}, fmt.Errorf("proxmox: %s cannot be migrated: %s", name, why)
			}
			if len(pre.AllowedNodes) > 0 {
				target = pre.AllowedNodes[0]
			}
		}
		if target == "" {
			nodes, err := c.Nodes()
			if err != nil {
				return Task{}, err
			}
			for _, n := range nodes {
				if n.Ready() && n.Node != guest.Node {
					target = n.Node
					break
				}
			}
		}
		if target == "" {
			return Task{}, fmt.Errorf("proxmox: no other online node to migrate %s to", name)
		}
	}

	params := map[string]string{"target": target}
	if status.Status == "running" {
		// online for a VM, restart for a container: LXC cannot live-migrate, and
		// PVE's own answer is a brief restart on the target rather than a
		// refusal. Saying which happened is the caller's job.
		if guest.Kind == KindQemu {
			params["online"] = "1"
			params["with-local-disks"] = "1"
		} else {
			params["restart"] = "1"
		}
	}
	raw, err := c.post(guest.path()+"/migrate", form(params))
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// Clone copies a guest. A full clone is the default because a linked clone
// depends on its parent forever, and deleting the parent of a linked clone is a
// footgun Corral should not hand over by default.
func (c *Client) Clone(name, target string, full bool) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	if c.Exists(target) {
		return Task{}, fmt.Errorf("proxmox: %q already exists", target)
	}
	newID, err := c.nextVMID()
	if err != nil {
		return Task{}, err
	}
	params := map[string]string{
		"newid": strconv.Itoa(newID),
		"name":  target,
	}
	if full {
		params["full"] = "1"
	}
	raw, err := c.post(guest.path()+"/clone", form(params))
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// ── guest access ──────────────────────────────────────────────────

// Address returns the guest's first global IPv4, via the guest agent for a VM or
// the container interface list for an LXC. Without an agent a VM has no address
// PVE can report, and the empty string says so rather than guessing.
func (c *Client) Address(name string) (string, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return "", err
	}
	if guest.Kind == KindLXC {
		var interfaces []struct {
			Name  string `json:"name"`
			Inet  string `json:"inet"`
			HWAdr string `json:"hwaddr"`
		}
		if err := c.get(guest.path()+"/interfaces", &interfaces); err != nil {
			return "", err
		}
		for _, iface := range interfaces {
			if iface.Name == "lo" || iface.Inet == "" {
				continue
			}
			return strings.SplitN(iface.Inet, "/", 2)[0], nil
		}
		return "", nil
	}

	var result struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				Type    string `json:"ip-address-type"`
				Address string `json:"ip-address"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.get(guest.path()+"/agent/network-get-interfaces", &result); err != nil {
		// No agent is a normal state, not a failure of the listing.
		return "", nil
	}
	for _, iface := range result.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.Type == "ipv4" && !strings.HasPrefix(addr.Address, "127.") &&
				!strings.HasPrefix(addr.Address, "169.254.") {
				return addr.Address, nil
			}
		}
	}
	return "", nil
}

// ConsoleTicket is what a browser needs to open a console: PVE hands back a
// one-shot ticket and a port, and the websocket carries the rest.
type ConsoleTicket struct {
	Ticket string `json:"ticket"`
	Port   string `json:"port"`
	User   string `json:"user"`
	UPID   string `json:"upid"`
	Cert   string `json:"cert"`
	// Node and Guest complete the websocket URL the bridge has to open.
	Node   string `json:"-"`
	VMID   int    `json:"-"`
	Kind   Kind   `json:"-"`
	Serial bool   `json:"-"`
}

// WebsocketPath is the URL the console bridge connects to with this ticket.
func (t ConsoleTicket) WebsocketPath() string {
	return fmt.Sprintf("/nodes/%s/%s/%d/vncwebsocket?port=%s&vncticket=%s",
		t.Node, t.Kind, t.VMID, t.Port, urlQueryEscape(t.Ticket))
}

// VNCTicket opens a graphical console session.
func (c *Client) VNCTicket(name string) (ConsoleTicket, error) {
	return c.consoleTicket(name, "vncproxy", map[string]string{"websocket": "1"})
}

// TermTicket opens a shell/serial console session — PVE's termproxy, which is
// what the xterm.js front end already knows how to speak to.
func (c *Client) TermTicket(name string) (ConsoleTicket, error) {
	ticket, err := c.consoleTicket(name, "termproxy", nil)
	ticket.Serial = true
	return ticket, err
}

func (c *Client) consoleTicket(name, endpoint string, params map[string]string) (ConsoleTicket, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return ConsoleTicket{}, err
	}
	var ticket ConsoleTicket
	if err := c.do("POST", guest.path()+"/"+endpoint, form(params), &ticket); err != nil {
		return ConsoleTicket{}, err
	}
	ticket.Node, ticket.VMID, ticket.Kind = guest.Node, guest.VMID, guest.Kind
	return ticket, nil
}

// ── backup ────────────────────────────────────────────────────────

// Backup runs vzdump for one guest, which is PVE's export. The archive lands on
// the named storage; downloading it is a separate step, and deliberately so — a
// multi-gigabyte stream through Corral is not something to start implicitly.
func (c *Client) Backup(name, storage, mode string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	if mode == "" {
		// snapshot mode keeps the guest running; it is what the PVE UI defaults
		// to and the only mode that does not cost downtime.
		mode = "snapshot"
	}
	params := map[string]string{
		"vmid":           strconv.Itoa(guest.VMID),
		"mode":           mode,
		"compress":       "zstd",
		"remove":         "0",
		"notes-template": "corral backup of {{guestname}}",
	}
	if storage != "" {
		params["storage"] = storage
	}
	raw, err := c.post("/nodes/"+guest.Node+"/vzdump", form(params))
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// Events returns the guest's recent task history, which is PVE's equivalent of
// an event stream.
func (c *Client) Events(name string) ([]TaskEntry, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return nil, err
	}
	return c.RecentTasks(guest.Node, guest.VMID, 50)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
