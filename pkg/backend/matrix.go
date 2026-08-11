// Package backend is the parity matrix: for every operation Corral ships, what
// each backend does about it — and where it does nothing, whether that is
// because the backend cannot, or only because Corral has not.
//
// It exists because the honest state of the codebase was "KubeVirt is
// first-class and the rest are best effort", and that state was invisible.
// Capabilities were a hardcoded table in types.CapabilitiesForBackend, the
// richer operations were reached by `if backend == "kubevirt"` branches in
// cmd/ and pkg/web, and nothing anywhere said which absences were deliberate.
// A gap you cannot enumerate is a gap nobody closes.
//
// So the matrix is data, in one place, and it is enforced: conformance tests
// check it against the declared capabilities, against pkg/snapshot's adapter
// registry, and against docs/backend-parity.md, so the table in the docs cannot
// drift from the code and a capability cannot be advertised that nothing
// implements.
//
// The rule this encodes, per the parity decision: if a backend can do a thing
// we ship, we want to support it. Not every backend feature — parity across
// backends for the features Corral has.
package backend

import "sort"

// Support is what Corral does about one operation on one backend.
type Support string

const (
	// Shipped — implemented and reachable from at least one surface.
	Shipped Support = "shipped"
	// Possible — the backend can do this and Corral does not yet. Note names
	// the native mechanism, so the gap is a work item rather than a shrug.
	Possible Support = "possible"
	// Unsupported — the backend genuinely cannot, or the operation is
	// meaningless there. Note says why, so nobody re-litigates it quarterly.
	Unsupported Support = "unsupported"
)

// Backends are the compute backends, in the order tables should show them.
var Backends = []string{"kubevirt", "qemu", "incus", "libvirt", "proxmox"}

// Operation is one capability-bearing thing Corral does to an instance.
type Operation struct {
	// ID is stable and used by tests and docs.
	ID string
	// Title is how the docs name it.
	Title string
	// Capability is the types.InstanceCapabilities field this operation gates
	// on, or "" for operations that have no capability flag yet.
	Capability string
}

// Operations are every instance operation Corral ships, in reading order.
var Operations = []Operation{
	{ID: "list", Title: "List / inventory"},
	{ID: "create", Title: "Create"},
	{ID: "start", Title: "Start", Capability: "Start"},
	{ID: "stop", Title: "Stop", Capability: "Stop"},
	{ID: "restart", Title: "Restart"},
	{ID: "pause", Title: "Pause / resume"},
	{ID: "delete", Title: "Delete", Capability: "Delete"},
	{ID: "ssh", Title: "SSH", Capability: "SSH"},
	{ID: "tty", Title: "Serial / shell console", Capability: "TTY"},
	{ID: "vnc", Title: "Graphical console (VNC)", Capability: "VNC"},
	{ID: "rdp", Title: "RDP", Capability: "RDP"},
	{ID: "metrics", Title: "Live CPU / memory", Capability: "Metrics"},
	{ID: "snapshots", Title: "Snapshot / restore", Capability: "Snapshots"},
	{ID: "migrate", Title: "Migrate", Capability: "Migrate"},
	{ID: "clone", Title: "Clone"},
	{ID: "template", Title: "Template mark"},
	{ID: "scale", Title: "CPU / memory edit"},
	{ID: "volumes", Title: "Add / remove disks", Capability: "Volumes"},
	{ID: "expand", Title: "Expand disk"},
	{ID: "gpu", Title: "GPU passthrough", Capability: "GPU"},
	{ID: "export", Title: "Export / backup disk"},
	{ID: "events", Title: "Events"},
	{ID: "tags", Title: "Tags"},
	{ID: "ports", Title: "Published ports"},
	{ID: "containers", Title: "Containers (CT)"},
}

// Entry is one cell: what a backend does about an operation, and why.
type Entry struct {
	Support Support
	// Note is the native mechanism for Possible, the reason for Unsupported,
	// and the implementing path for Shipped. Required in every case — a cell
	// without a note is a cell nobody can act on.
	Note string
}

// Matrix is operation ID → backend → entry. It is the single source of truth
// the docs table and the conformance tests both read.
//
// Every cell was checked against the code, not inferred from intent; where the
// audit found something surprising the note says so.
var Matrix = map[string]map[string]Entry{
	"list": {
		"kubevirt": {Shipped, "kubectl get vms/vmis, pkg/kubevirt"},
		"qemu":     {Shipped, "systemd user units + metadata, pkg/qemu"},
		"incus":    {Shipped, "incus list --format=json, pkg/incus"},
		"libvirt":  {Shipped, "virsh list --all, pkg/libvirt"},
		"proxmox":  {Shipped, "GET /cluster/resources, pkg/proxmoxbe.List — one call for the whole cluster"},
	},
	"create": {
		"kubevirt": {Shipped, "pkg/web/create_*.go, cmd/create.go"},
		"qemu":     {Shipped, "qemu-img + generated systemd unit, pkg/qemu.Create"},
		"incus":    {Shipped, "incus launch, pkg/incus.Create"},
		"libvirt":  {Shipped, "virt-install, pkg/libvirt.Create"},
		"proxmox":  {Shipped, "POST /nodes/{node}/qemu or /lxc, pkg/proxmoxbe.Create"},
	},
	"start": {
		"kubevirt": {Shipped, "virtctl start"},
		"qemu":     {Shipped, "systemctl --user start"},
		"incus":    {Shipped, "incus start"},
		"libvirt":  {Shipped, "virsh start"},
		"proxmox":  {Shipped, "POST …/status/start"},
	},
	"stop": {
		"kubevirt": {Shipped, "virtctl stop"},
		"qemu":     {Shipped, "systemctl --user stop"},
		"incus":    {Shipped, "incus stop"},
		"libvirt":  {Shipped, "virsh shutdown"},
		"proxmox":  {Shipped, "POST …/status/shutdown; the forced power cut is Kill, chosen by name"},
	},
	"restart": {
		"kubevirt": {Shipped, "virtctl restart"},
		"qemu":     {Shipped, "stop then start, cmd/ops.go"},
		"incus":    {Shipped, "incus restart — the backend's own, so the guest's shutdown ordering survives"},
		"libvirt":  {Shipped, "virsh reboot — an ACPI request; a guest that ignores it stays up, which is honest"},
		"proxmox":  {Shipped, "POST …/status/reboot — a real reboot, not stop-then-start"},
	},
	"pause": {
		"kubevirt": {Shipped, "virtctl pause / unpause"},
		"qemu":     {Shipped, "QMP stop/cont on the unit's QMP socket, pkg/qemu.Pause"},
		"incus":    {Shipped, "incus pause / incus start, pkg/incus.Pause"},
		"libvirt":  {Shipped, "virsh suspend / virsh resume, pkg/libvirt.Pause"},
		"proxmox":  {Shipped, "POST …/status/suspend and /resume"},
	},
	"delete": {
		"kubevirt": {Shipped, "kubectl delete vm"},
		"qemu":     {Shipped, "unit removal + disk delete"},
		"incus":    {Shipped, "incus delete --force"},
		"libvirt":  {Shipped, "virsh undefine --remove-all-storage"},
		"proxmox":  {Shipped, "DELETE …?purge=1, so backup jobs do not outlive the guest"},
	},
	"ssh": {
		"kubevirt": {Shipped, "virtctl ssh"},
		"qemu":     {Shipped, "ssh via hostfwd port, pkg/qemu.SSH"},
		"incus":    {Shipped, "incus exec … ssh, pkg/incus.SSH"},
		"libvirt":  {Possible, "the domain's address via the guest agent or DHCP leases, then plain ssh — pkg/libvirt has SSH but the TUI does not offer it because the capability table omits it"},
		"proxmox":  {Shipped, "the guest agent (VM) or interface list (CT) for the address, then plain ssh"},
	},
	"tty": {
		"kubevirt": {Shipped, "virtctl console, web ttyBridge"},
		"qemu":     {Possible, "the serial socket the generated unit already defines"},
		"incus":    {Shipped, "incus exec through the web ttyBridge (pkg/web/server.go); the TUI has no TTY view for any backend yet"},
		"libvirt":  {Possible, "virsh console"},
		"proxmox":  {Possible, "termproxy tickets are implemented (pkg/proxmoxbe.TermTicket); the web websocket bridge is not wired yet"},
	},
	"vnc": {
		"kubevirt": {Shipped, "virtctl vnc --proxy-only, noVNC in the browser"},
		"qemu":     {Shipped, "VNC display on the host, pkg/qemu.VNCAddr"},
		"incus":    {Possible, "incus console --type=vga for Incus VMs; the web vncBridge handles local, libvirt, and cluster namespaces only"},
		"libvirt":  {Shipped, "pkg/libvirt.DialVNC, including over SSH"},
		"proxmox":  {Possible, "vncproxy tickets are implemented (pkg/proxmoxbe.VNCTicket); the web websocket bridge is not wired yet"},
	},
	"rdp": {
		"kubevirt": {Shipped, "port-forward + IronRDP, ADR-0002"},
		"qemu":     {Possible, "the same probe and bridge over the hostfwd port"},
		"incus":    {Possible, "same, via the instance address"},
		"libvirt":  {Possible, "same, via the domain address"},
		"proxmox":  {Possible, "same, via the guest address"},
	},
	"metrics": {
		"kubevirt": {Shipped, "kubectl top pod, metrics-server"},
		"qemu":     {Shipped, "the unit's cgroup via systemctl show, pkg/qemu.Metrics — host-side cost, sampled twice for a CPU rate"},
		"incus":    {Shipped, "incus query /1.0/instances/{name}/state, pkg/incus.Metrics"},
		"libvirt":  {Shipped, "virsh domstats --cpu-total --balloon, pkg/libvirt.Metrics"},
		"proxmox":  {Shipped, "GET …/status/current for live usage, /rrddata for the sparkline's real history"},
	},
	"snapshots": {
		"kubevirt": {Shipped, "VirtualMachineSnapshot, pkg/snapshot.KubeVirt"},
		"qemu":     {Shipped, "qemu-img internal snapshots, offline only, pkg/snapshot.QEMU"},
		"incus":    {Shipped, "incus snapshot, pkg/snapshot.Incus"},
		"libvirt":  {Shipped, "virsh snapshot-create-as --atomic, pkg/snapshot.Libvirt"},
		"proxmox":  {Shipped, "pkg/snapshot.Proxmox; vmstate=1 captures RAM, so a running guest is honestly consistent"},
	},
	"migrate": {
		"kubevirt": {Shipped, "VirtualMachineInstanceMigration"},
		"qemu":     {Unsupported, "one host; there is nowhere to migrate to"},
		"incus":    {Possible, "incus move, including between remotes"},
		"libvirt":  {Possible, "virsh migrate --live to another URI"},
		"proxmox":  {Shipped, "POST …/migrate with online=1 (restart=1 for LXC), and GET …/migrate as a real precondition check"},
	},
	"clone": {
		"kubevirt": {Shipped, "VirtualMachineClone"},
		"qemu":     {Possible, "qemu-img convert plus a new unit"},
		"incus":    {Possible, "incus copy"},
		"libvirt":  {Possible, "virt-clone"},
		"proxmox":  {Shipped, "POST …/clone, full by default so a clone does not depend on its parent forever"},
	},
	"template": {
		"kubevirt": {Shipped, "corral.dev/template label"},
		"qemu":     {Possible, "the same mark in the local registry"},
		"incus":    {Possible, "incus publish, or the registry mark"},
		"libvirt":  {Possible, "the registry mark"},
		"proxmox":  {Shipped, "POST …/template — native, and one-way: unmarking is refused with the reason"},
	},
	"scale": {
		"kubevirt": {Shipped, "patch domain cpu/memory, hotplug where migratable"},
		"qemu":     {Possible, "rewrite the unit and restart"},
		"incus":    {Possible, "incus config set limits.cpu / limits.memory, live"},
		"libvirt":  {Possible, "virsh setvcpus / setmem"},
		"proxmox":  {Shipped, "PUT …/config cores/memory; the config's hotplug field says whether it applies live"},
	},
	"volumes": {
		"kubevirt": {Shipped, "addvolume / removevolume, DataVolumes"},
		"qemu":     {Possible, "qemu-img create plus a unit edit"},
		"incus":    {Possible, "incus storage volume attach"},
		"libvirt":  {Possible, "virsh attach-disk / detach-disk"},
		"proxmox":  {Shipped, "PUT …/config scsiN to attach, delete= to remove"},
	},
	"expand": {
		"kubevirt": {Shipped, "PVC resize where the storage class allows"},
		"qemu":     {Possible, "qemu-img resize while stopped"},
		"incus":    {Possible, "incus config device set … size"},
		"libvirt":  {Possible, "virsh blockresize"},
		"proxmox":  {Shipped, "PUT …/resize; PVE refuses to shrink, which is what Corral wants"},
	},
	"gpu": {
		"kubevirt": {Shipped, "GPU device plugin, pkg/web/gpu.go"},
		"qemu":     {Possible, "vfio-pci in the generated unit"},
		"incus":    {Possible, "incus config device add … gpu"},
		"libvirt":  {Possible, "hostdev in the domain XML"},
		"proxmox":  {Shipped, "PUT …/config hostpciN"},
	},
	"export": {
		"kubevirt": {Shipped, "virtctl vmexport, qcow2 or raw.gz — pkg/export.KubeVirt"},
		"qemu":     {Shipped, "qemu-img convert while stopped — pkg/export.QEMU"},
		"incus":    {Shipped, "incus export; qcow2 pulls the boot disk out of the archive — pkg/export.Incus"},
		"libvirt":  {Shipped, "qemu-img convert of the backing volume — pkg/export.Libvirt"},
		"proxmox":  {Possible, "vzdump in snapshot mode; the export adapter downloads the backup over the PVE API, extracts the disk via vma, and converts to qcow2"},
	},
	"events": {
		"kubevirt": {Shipped, "kubectl get events for the VM and its launcher pod"},
		"qemu":     {Shipped, "journalctl --user for the unit, pkg/qemu.Events"},
		"incus":    {Unsupported, "incus monitor is a subscription to what happens next; Incus keeps no queryable event history, so there is nothing to read at the moment a view asks"},
		"libvirt":  {Unsupported, "virsh event likewise only streams; libvirt keeps no per-domain event log. Both would need Corral to run a collector and store the history itself, which is a feature rather than an adapter"},
		"proxmox":  {Shipped, "GET /nodes/{node}/tasks filtered by vmid — PVE's task history is its event record"},
	},
	"tags": {
		"kubevirt": {Shipped, "corral.dev/tag.<name> labels"},
		"qemu":     {Possible, "the local registry, which already persists per-VM state"},
		"incus":    {Possible, "instance config user.corral.tag.<name>"},
		"libvirt":  {Possible, "domain metadata"},
		"proxmox":  {Shipped, "the config's own tags field, read-modify-write so a second tag does not erase the first"},
	},
	"ports": {
		"kubevirt": {Shipped, "port-proxy Service on the tailnet, ApplyProxy"},
		"qemu":     {Shipped, "hostfwd in the generated unit"},
		"incus":    {Possible, "incus config device add … proxy"},
		"libvirt":  {Unsupported, "no port mapping of Corral's own; the domain is on the host network"},
		"proxmox":  {Unsupported, "the guest is bridged; PVE has no per-VM port mapping to drive"},
	},
	"containers": {
		"kubevirt": {Shipped, "pet-pod CTs, ADR-0005, pkg/ct"},
		"qemu":     {Unsupported, "a hypervisor, not a container runtime"},
		"incus":    {Shipped, "LXC containers listed and controlled as CTs through pkg/incus.Containers; Incus virtual machines are VMs, not CTs"},
		"libvirt":  {Unsupported, "libvirt's LXC driver is not something Corral drives"},
		"proxmox":  {Possible, "pkg/proxmoxbe.Containers lists them and Create makes them; pkg/ct does not yet surface a non-Kubernetes CT"},
	},
}

// Get returns one cell.
func Get(operation, backend string) (Entry, bool) {
	row, ok := Matrix[operation]
	if !ok {
		return Entry{}, false
	}
	entry, ok := row[backend]
	return entry, ok
}

// OperationIDs returns every operation ID in reading order.
func OperationIDs() []string {
	ids := make([]string, 0, len(Operations))
	for _, op := range Operations {
		ids = append(ids, op.ID)
	}
	return ids
}

// Gaps returns the operations a backend could support and does not, in reading
// order — the work list this package exists to make enumerable.
func Gaps(backend string) []string {
	var out []string
	for _, op := range Operations {
		if entry, ok := Get(op.ID, backend); ok && entry.Support == Possible {
			out = append(out, op.ID)
		}
	}
	return out
}

// ShippedBy returns the backends that ship an operation, sorted.
func ShippedBy(operation string) []string {
	var out []string
	for backend, entry := range Matrix[operation] {
		if entry.Support == Shipped {
			out = append(out, backend)
		}
	}
	sort.Strings(out)
	return out
}
