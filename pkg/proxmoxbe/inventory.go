package proxmoxbe

// Inventory and identity.
//
// PVE addresses everything by a cluster-wide integer (vmid); Corral addresses
// everything by name. The whole cluster comes back from one /cluster/resources
// call — VMs and containers together, with node, status, shape, and tags — so
// the name→vmid map and the fleet listing are the same request, and no operation
// needs a second round trip to find out where an instance lives.

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// Kind distinguishes PVE's two workload types. It matters because Corral models
// them as different things: a qemu guest is a VM, an lxc guest is a CT — the
// same split pkg/incus draws, and the one Incus support originally got wrong.
type Kind string

const (
	KindQemu Kind = "qemu"
	KindLXC  Kind = "lxc"
)

// Resource is one instance as /cluster/resources reports it.
type Resource struct {
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Node     string  `json:"node"`
	Type     string  `json:"type"`   // "qemu" | "lxc" | "storage" | "node" | …
	Status   string  `json:"status"` // "running" | "stopped"
	MaxCPU   float64 `json:"maxcpu"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	CPU      float64 `json:"cpu"` // fraction of maxcpu, 0..1
	Mem      int64   `json:"mem"`
	Uptime   int64   `json:"uptime"`
	Template int     `json:"template"`
	Tags     string  `json:"tags"`
	Lock     string  `json:"lock"`
	Pool     string  `json:"pool"`
}

// Kind reports which workload type this resource is.
func (r Resource) Kind() Kind {
	if r.Type == string(KindLXC) {
		return KindLXC
	}
	return KindQemu
}

// IsTemplate reports PVE's own template flag — a real mark, not an emulated
// label like the one Corral keeps for KubeVirt.
func (r Resource) IsTemplate() bool { return r.Template == 1 }

// TagList splits PVE's comma-separated tags. PVE has tags natively, so Corral's
// tags map straight across instead of being emulated with labels.
func (r Resource) TagList() []string {
	if strings.TrimSpace(r.Tags) == "" {
		return nil
	}
	parts := strings.FieldsFunc(r.Tags, func(c rune) bool { return c == ',' || c == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Resources returns every guest in the cluster, VMs and containers alike.
func (c *Client) Resources() ([]Resource, error) {
	var all []Resource
	if err := c.get("/cluster/resources?type=vm", &all); err != nil {
		return nil, err
	}
	guests := make([]Resource, 0, len(all))
	for _, r := range all {
		// type=vm is a filter PVE applies loosely; only qemu and lxc are guests.
		if r.Type == string(KindQemu) || r.Type == string(KindLXC) {
			guests = append(guests, r)
		}
	}
	sort.Slice(guests, func(i, j int) bool { return guests[i].VMID < guests[j].VMID })
	return guests, nil
}

// List converts the cluster's *qemu* guests into Corral VMs. LXC containers are
// CTs and come back from Containers instead — one instance is never both.
func (c *Client) List() ([]types.VM, error) {
	guests, err := c.Resources()
	if err != nil {
		return nil, err
	}
	vms := make([]types.VM, 0, len(guests))
	for _, r := range guests {
		if r.Kind() != KindQemu {
			continue
		}
		vms = append(vms, c.toVM(r))
	}
	return vms, nil
}

// Containers returns the cluster's LXC guests.
func (c *Client) Containers() ([]Resource, error) {
	guests, err := c.Resources()
	if err != nil {
		return nil, err
	}
	cts := make([]Resource, 0, len(guests))
	for _, r := range guests {
		if r.Kind() == KindLXC {
			cts = append(cts, r)
		}
	}
	return cts, nil
}

func (c *Client) toVM(r Resource) types.VM {
	running := r.Status == "running"
	status := statusWord(r)
	vm := types.VM{
		Name:    displayName(r),
		Backend: "proxmox",
		// Namespace stays empty: PVE has no namespace, and repurposing the
		// field (for the node, or a pool) would make identity mean two things.
		Context:        c.cfg.Host,
		Status:         status,
		Ready:          running && r.Lock == "",
		Running:        running,
		CPU:            int(r.MaxCPU),
		Mem:            humanBytes(r.MaxMem),
		Disk:           humanBytes(r.MaxDisk),
		Node:           r.Node,
		IsTemplate:     r.IsTemplate(),
		Tags:           r.TagList(),
		Capabilities:   types.CapabilitiesForBackend("proxmox"),
		LiveMigratable: running,
	}
	vm.SetIdentity()
	return vm
}

// statusWord reports the lock as the status when one is held. A PVE guest with
// lock=migrate is not simply "running", and the TUI's busy classification keys
// off exactly these words.
func statusWord(r Resource) string {
	if r.Lock != "" {
		switch r.Lock {
		case "migrate":
			return "Migrating"
		case "backup":
			return "Backing up"
		case "snapshot", "snapshot-delete":
			return "Snapshotting"
		case "clone":
			return "Cloning"
		default:
			return strings.ToUpper(r.Lock[:1]) + r.Lock[1:]
		}
	}
	if r.Status == "" {
		return "Unknown"
	}
	return strings.ToUpper(r.Status[:1]) + r.Status[1:]
}

// displayName falls back to the vmid when a guest has no name, which PVE allows.
func displayName(r Resource) string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return strconv.Itoa(r.VMID)
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1<<30:
		return trimFloat(float64(n)/(1<<30)) + "Gi"
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + "Mi"
	default:
		return strconv.FormatInt(n, 10)
	}
}

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// ── identity ──────────────────────────────────────────────────────

// Guest is a resolved instance: enough to address it without another lookup.
type Guest struct {
	VMID int
	Name string
	Node string
	Kind Kind
}

// path builds the API prefix for this guest: /nodes/{node}/{qemu|lxc}/{vmid}.
func (g Guest) path() string {
	return fmt.Sprintf("/nodes/%s/%s/%d", g.Node, g.Kind, g.VMID)
}

// Resolve finds a guest by name or by vmid. Both are accepted because PVE users
// think in vmids and Corral users think in names, and refusing one of them would
// make the backend feel foreign from whichever side you arrive.
func (c *Client) Resolve(nameOrVMID string) (Guest, error) {
	guests, err := c.Resources()
	if err != nil {
		return Guest{}, err
	}
	if id, convErr := strconv.Atoi(nameOrVMID); convErr == nil {
		for _, r := range guests {
			if r.VMID == id {
				return Guest{VMID: r.VMID, Name: displayName(r), Node: r.Node, Kind: r.Kind()}, nil
			}
		}
		return Guest{}, fmt.Errorf("proxmox: no guest with vmid %d", id)
	}

	var matches []Resource
	for _, r := range guests {
		if displayName(r) == nameOrVMID {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return Guest{}, fmt.Errorf("proxmox: no guest named %q", nameOrVMID)
	case 1:
		r := matches[0]
		return Guest{VMID: r.VMID, Name: displayName(r), Node: r.Node, Kind: r.Kind()}, nil
	default:
		// PVE allows duplicate names across nodes even though the UI discourages
		// it. Refusing with the vmids is more useful than acting on a guess.
		var ids []string
		for _, r := range matches {
			ids = append(ids, fmt.Sprintf("%d on %s", r.VMID, r.Node))
		}
		return Guest{}, fmt.Errorf("proxmox: %q is ambiguous (%s); select it by vmid",
			nameOrVMID, strings.Join(ids, ", "))
	}
}

// VMIDMap maps display name to vmid, for callers annotating a whole inventory.
func (c *Client) VMIDMap() (map[string]int, error) {
	guests, err := c.Resources()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(guests))
	for _, r := range guests {
		out[displayName(r)] = r.VMID
	}
	return out, nil
}

// Exists reports whether a guest is present. A cluster that cannot be reached
// reports false, matching every other backend's Exists.
func (c *Client) Exists(name string) bool {
	_, err := c.Resolve(name)
	return err == nil
}

// Nodes returns the cluster's nodes.
func (c *Client) Nodes() ([]NodeInfo, error) {
	var nodes []NodeInfo
	if err := c.get("/nodes", &nodes); err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })
	return nodes, nil
}

// NodeInfo is one PVE node.
type NodeInfo struct {
	Node   string  `json:"node"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	MaxCPU int     `json:"maxcpu"`
	Mem    int64   `json:"mem"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
}

// Ready reports whether the node is online.
func (n NodeInfo) Ready() bool { return n.Status == "online" }

// defaultNode picks the node for an operation that has no guest to derive one
// from — a create, mostly. The configured node wins; otherwise the first online
// node, so a single-node cluster (which is most of them) needs no configuration.
func (c *Client) defaultNode() (string, error) {
	if c.cfg.Node != "" {
		return c.cfg.Node, nil
	}
	nodes, err := c.Nodes()
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Ready() {
			return n.Node, nil
		}
	}
	return "", fmt.Errorf("proxmox: no online node to place this on")
}

// nextVMID asks the cluster for a free id rather than inventing one: PVE's own
// allocator knows about ids reserved by in-flight creates.
func (c *Client) nextVMID() (int, error) {
	var raw any
	if err := c.get("/cluster/nextid", &raw); err != nil {
		return 0, err
	}
	switch v := raw.(type) {
	case string:
		return strconv.Atoi(v)
	case float64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("proxmox: unexpected /cluster/nextid response %s", dump(raw))
	}
}

// form is a small helper for the parameter maps every mutation sends.
func form(pairs map[string]string) url.Values {
	values := url.Values{}
	for k, v := range pairs {
		if v != "" {
			values.Set(k, v)
		}
	}
	return values
}
