package types

import (
	"fmt"
	"net/url"
	"strings"
)

// Backend is the seam between corral's two compute backends (qemu, kubevirt):
// the operations both genuinely implement today. It's deliberately smaller
// than either backend's full capability set — kubevirt.Client has migrate/
// snapshot/scale/GPU operations qemu has no counterpart for, and qemu has no
// restart/pause/unpause plumbing kubevirt does. Those stay reachable through
// the concrete types directly; this interface only covers what both sides
// can promise.
type Backend interface {
	ListVMs() ([]VM, error)
	VMExists(name string) bool
	StartVM(name string) error
	StopVM(name string) error
	DeleteVM(name string) error
	VMInfo(name string) ([]byte, error)
	SSH(name, username, identityFile, command string, port int, password string, localForwards []string) error
	Viewer(name string) error
	Logs(name string) error
}

// VM represents a virtual machine from either backend.
type VM struct {
	// ID is the stable, fully-scoped identity used by APIs and selectors. Name
	// remains the human display name and is only accepted as a selector when it
	// resolves to exactly one instance.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Backend string `json:"backend"` // "qemu", "kubevirt", or "incus"
	Status  string `json:"status"`  // "Running", "Stopped", "Starting", "↓ 42.5%"
	Ready   bool   `json:"ready"`
	Running bool   `json:"running"`
	// Context disambiguates instances with the same name across independent
	// universes (an Incus remote or Kubernetes context).
	Context      string               `json:"context,omitempty"`
	Peer         string               `json:"peer,omitempty"`
	Capabilities InstanceCapabilities `json:"capabilities"`
	Endpoints    map[string]string    `json:"endpoints,omitempty"`

	CPU  int    `json:"cpu"`
	Mem  string `json:"mem"`
	Disk string `json:"disk,omitempty"`
	Node string `json:"node,omitempty"`
	VNC  string `json:"vnc,omitempty"` // port or "on"/"off"/"pending"
	IP   string `json:"ip,omitempty"`
	ISO  string `json:"iso,omitempty"` // ISO download progress

	// KubeVirt-specific
	Namespace      string   `json:"namespace,omitempty"`
	LiveMigratable bool     `json:"liveMigratable"` // VMI LiveMigratable condition
	AgentConnected bool     `json:"agentConnected"` // qemu-guest-agent reachable
	IsTemplate     bool     `json:"isTemplate"`     // labeled corral.dev/template=true
	Bootc          bool     `json:"bootc"`          // kernel-boot VM (built by the bootc plugin)
	Tags           []string `json:"tags,omitempty"` // from corral.dev/tag.<name> labels

	// Ephemeral GC (see pkg/kubevirt/gc.go): labeled corral.dev/ephemeral=true
	// at create time with a TTL. `corral gc` stops (not deletes) the VM once
	// ExpiresAt passes — PVCs and disk state survive a stop — and only
	// deletes it outright after it's sat stopped past the grace period.
	Ephemeral bool   `json:"ephemeral,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"` // RFC3339; when running past this, gc stops it
	StoppedAt string `json:"stoppedAt,omitempty"` // RFC3339; when gc stopped it (unset if user-stopped)
}

// InstanceRef identifies one instance without relying on globally unique
// names. Context is a kubeconfig context, Incus remote, or libvirt URI.
type InstanceRef struct {
	Peer      string `json:"peer,omitempty"`
	Backend   string `json:"backend"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func (r InstanceRef) Validate() error {
	if strings.TrimSpace(r.Backend) == "" || strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("instance backend and name are required")
	}
	return nil
}

// String is a reversible, URL-safe selector suitable for CLI arguments and
// API payloads: backend/context/namespace/name, optionally prefixed by peer.
func (r InstanceRef) String() string {
	parts := []string{r.Backend, r.Context, r.Namespace, r.Name}
	if r.Peer != "" {
		parts = append([]string{"peer", r.Peer}, parts...)
	}
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func ParseInstanceRef(s string) (InstanceRef, error) {
	raw := strings.Split(s, "/")
	for i := range raw {
		part, err := url.PathUnescape(raw[i])
		if err != nil {
			return InstanceRef{}, fmt.Errorf("invalid instance selector %q: %w", s, err)
		}
		raw[i] = part
	}
	var r InstanceRef
	if len(raw) == 6 && raw[0] == "peer" {
		r.Peer, raw = raw[1], raw[2:]
	}
	if len(raw) != 4 {
		return InstanceRef{}, fmt.Errorf("invalid instance selector %q (want backend/context/namespace/name)", s)
	}
	r.Backend, r.Context, r.Namespace, r.Name = raw[0], raw[1], raw[2], raw[3]
	return r, r.Validate()
}

func (v VM) Ref() InstanceRef {
	return InstanceRef{Peer: v.Peer, Backend: v.Backend, Context: v.Context, Namespace: v.Namespace, Name: v.Name}
}

func (v *VM) SetIdentity() {
	if v.ID == "" {
		v.ID = v.Ref().String()
	}
	if v.Capabilities == (InstanceCapabilities{}) {
		v.Capabilities = CapabilitiesForBackend(v.Backend)
	}
}

// InstanceCapabilities prevents clients from guessing supported operations
// from namespace strings. Remote peers may further restrict this set.
type InstanceCapabilities struct {
	Start     bool `json:"start"`
	Stop      bool `json:"stop"`
	Delete    bool `json:"delete"`
	SSH       bool `json:"ssh"`
	TTY       bool `json:"tty"`
	VNC       bool `json:"vnc"`
	RDP       bool `json:"rdp"`
	Metrics   bool `json:"metrics"`
	Snapshots bool `json:"snapshots"`
	Migrate   bool `json:"migrate"`
	Volumes   bool `json:"volumes"`
	GPU       bool `json:"gpu"`
}

func CapabilitiesForBackend(backend string) InstanceCapabilities {
	switch backend {
	case "kubevirt":
		return InstanceCapabilities{Start: true, Stop: true, Delete: true, SSH: true, TTY: true, VNC: true, RDP: true, Metrics: true, Snapshots: true, Migrate: true, Volumes: true, GPU: true}
	case "qemu":
		// Snapshots: qcow2 internal snapshots via qemu-img. Only while the VM
		// is stopped and only on a qcow2 disk — the adapter refuses the rest
		// with a reason, which is a better experience than a hidden tab.
		return InstanceCapabilities{Start: true, Stop: true, Delete: true, SSH: true, VNC: true, Snapshots: true}
	case "incus":
		return InstanceCapabilities{Start: true, Stop: true, Delete: true, SSH: true, TTY: true, Snapshots: true}
	case "libvirt":
		return InstanceCapabilities{Start: true, Stop: true, Delete: true, VNC: true, Snapshots: true}
	case "proxmox":
		// Consoles are declared off deliberately: PVE serves them over its own
		// websocket and pkg/proxmoxbe can mint the tickets, but no bridge is
		// wired yet — advertising a console nothing can open is the exact
		// failure the parity conformance tests exist to catch.
		return InstanceCapabilities{Start: true, Stop: true, Delete: true, SSH: true,
			Metrics: true, Snapshots: true, Migrate: true, Volumes: true, GPU: true}
	default:
		return InstanceCapabilities{}
	}
}

// Capabilities reports what optional operations the cluster supports, so the
// UI can enable/disable controls instead of failing on click.
type Capabilities struct {
	StorageClass string `json:"storageClass"` // preferred SC for new disks ("" = cluster default)
	CanExpand    bool   `json:"canExpand"`    // default SC has allowVolumeExpansion
	CanSnapshot  bool   `json:"canSnapshot"`  // a VolumeSnapshotClass exists
}

// RegistryEntry persists backend choice per VM.
type RegistryEntry struct {
	Backend   string `json:"backend"`
	Context   string `json:"context,omitempty"`
	Peer      string `json:"peer,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Password  string `json:"password,omitempty"`
	Username  string `json:"username,omitempty"` // remembered from the last explicit `corral ssh -u`
}

// CreateOpts holds all VM creation options.
type CreateOpts struct {
	Name              string
	Backend           string // "qemu" or "kubevirt"
	Mem               string
	CPU               int
	Disk              string
	ISO               string
	QCOW              string
	Force             bool
	ContainerDisk     string
	ImportURL         string // qcow2/raw disk image URL → CDI import as the boot disk
	PVC               string
	Namespace         string
	Node              string
	CloudInitPassword string
	CloudInitExtra    string
	SSHPublicKey      string
	InstanceType      string // KubeVirt cluster instancetype (sets CPU/mem); overrides CPU/Mem
	Preference        string // KubeVirt cluster preference (devices/firmware defaults)
	StorageClass      string // overrides PreferredStorageClass() for this VM's disks; "" = default
	Ephemeral         bool   // labels the VM for `corral gc` (see pkg/kubevirt/gc.go)
	TTL               string // duration string (e.g. "4h"); with Ephemeral, sets corral.dev/expires-at
	// ExistingDisk means the VM dir already holds a prepared disk.qcow2 (e.g.
	// a bootc-built disk) — Create must boot it as-is, never recreate it.
	ExistingDisk bool
}

// PortMap maps protocol names to port numbers.
var PortMap = map[string]int{
	"ssh":   22,
	"rdp":   3389,
	"vnc":   5900,
	"http":  80,
	"https": 443,
}

// DefaultPorts are the ports offered in the edit menu.
var DefaultPorts = []int{22, 3389, 5900, 80, 443}

// PeerProtocol is the version of the Corral-to-Corral federation protocol this
// build speaks. `corral web` advertises it on /api/v1/meta and `corral doctor`
// compares a peer's against it, so it lives here rather than being written out
// in both places — a peer diagnosed as compatible has to be one the aggregator
// will actually federate with.
const PeerProtocol = 1
