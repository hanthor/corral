package proxmoxbe

// Creation, and the types.Backend implementation the CLI dispatches through.

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// urlQueryEscape escapes a value for a query string. Console tickets contain
// characters that must not be pasted into a URL raw.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// CreateOpts is what this backend needs to make a guest. It is deliberately
// smaller than types.CreateOpts: the KubeVirt-only fields (instancetype,
// storage class, NAD) have no PVE meaning, and pretending otherwise would put
// dead options in front of an operator.
type CreateOpts struct {
	Name string
	// Container makes an LXC guest instead of a VM. PVE's two workload types map
	// onto Corral's VM and CT, so this is the same choice Corral already makes.
	Container bool
	Node      string
	Cores     int
	Mem       string // "4Gi", "2048Mi", or a bare MiB count
	Disk      string // "20G"
	Storage   string // PVE storage id; "" lets PVE choose its default
	Bridge    string // "vmbr0" when unset
	// ISO is a PVE volume id ("local:iso/debian.iso") to boot from.
	ISO string
	// Template is an LXC template volume id ("local:vztmpl/debian-12.tar.zst").
	Template string
	// Image is a cloud image volume id used as the boot disk for a VM.
	Image string
	// SSHKeys is an authorized_keys body handed to cloud-init or LXC.
	SSHKeys string
	// Password seeds cloud-init (VM) or the root account (CT).
	Password string
	User     string
	// Unprivileged applies to containers, and defaults to true — the same
	// default PVE uses and ADR-0005 models.
	Privileged bool
	Tags       []string
	// UEFI selects OVMF firmware plus an EFI vars disk. A guest installed under
	// UEFI boots to a blank screen on PVE's SeaBIOS default, so this has to
	// travel with an imported disk (ADR-0010).
	UEFI  bool
	Start bool
}

// Create makes a guest and returns its task. It does not wait: creation of a
// disk-importing VM takes minutes, and blocking a CLI on it without progress is
// worse than handing back the task.
func (c *Client) Create(opts CreateOpts) (Task, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return Task{}, fmt.Errorf("proxmox: a name is required")
	}
	if c.Exists(opts.Name) {
		return Task{}, fmt.Errorf("proxmox: %q already exists", opts.Name)
	}
	node := opts.Node
	if node == "" {
		resolved, err := c.defaultNode()
		if err != nil {
			return Task{}, err
		}
		node = resolved
	}
	vmid, err := c.nextVMID()
	if err != nil {
		return Task{}, err
	}

	params, kind, err := c.createParams(opts, vmid)
	if err != nil {
		return Task{}, err
	}
	raw, err := c.post(fmt.Sprintf("/nodes/%s/%s", node, kind), params)
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: node}, nil
}

func (c *Client) createParams(opts CreateOpts, vmid int) (url.Values, Kind, error) {
	cores := opts.Cores
	if cores == 0 {
		cores = 2
	}
	mem := opts.Mem
	if mem == "" {
		mem = "2Gi"
	}
	mib, err := memoryMiB(mem)
	if err != nil {
		return nil, "", err
	}
	disk := strings.TrimSuffix(strings.TrimSuffix(opts.Disk, "G"), "Gi")
	if disk == "" {
		disk = "20"
	}
	storage := opts.Storage
	bridge := opts.Bridge
	if bridge == "" {
		bridge = "vmbr0"
	}

	if opts.Container {
		if opts.Template == "" {
			return nil, "", fmt.Errorf("proxmox: a container needs an ostemplate " +
				"(e.g. local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst)")
		}
		if storage == "" {
			storage = "local-lvm"
		}
		params := map[string]string{
			"vmid":       strconv.Itoa(vmid),
			"hostname":   opts.Name,
			"ostemplate": opts.Template,
			"cores":      strconv.Itoa(cores),
			"memory":     strconv.Itoa(mib),
			"rootfs":     fmt.Sprintf("%s:%s", storage, disk),
			"net0":       fmt.Sprintf("name=eth0,bridge=%s,ip=dhcp", bridge),
			// PVE's own default, and the safer one: a privileged container shares
			// the host's uid space.
			"unprivileged": boolParam(!opts.Privileged),
			"password":     opts.Password,
			"start":        boolParam(opts.Start),
		}
		if opts.SSHKeys != "" {
			params["ssh-public-keys"] = opts.SSHKeys
		}
		if len(opts.Tags) > 0 {
			params["tags"] = strings.Join(opts.Tags, ",")
		}
		return form(params), KindLXC, nil
	}

	if storage == "" {
		storage = "local-lvm"
	}
	params := map[string]string{
		"vmid":   strconv.Itoa(vmid),
		"name":   opts.Name,
		"cores":  strconv.Itoa(cores),
		"memory": strconv.Itoa(mib),
		"net0":   fmt.Sprintf("virtio,bridge=%s", bridge),
		"scsihw": "virtio-scsi-single",
		"ostype": "l26",
		"agent":  "1",
		"start":  boolParam(opts.Start),
		"scsi0":  fmt.Sprintf("%s:%s", storage, disk),
		"boot":   "order=scsi0",
	}
	switch {
	case opts.Image != "":
		// A cloud image becomes the boot disk, and cloud-init needs a drive to
		// present its data on.
		params["scsi0"] = fmt.Sprintf("%s:0,import-from=%s", storage, opts.Image)
		params["ide2"] = fmt.Sprintf("%s:cloudinit", storage)
		params["boot"] = "order=scsi0"
	case opts.ISO != "":
		params["ide2"] = opts.ISO + ",media=cdrom"
		params["boot"] = "order=scsi0;ide2"
	}
	if opts.UEFI {
		params["bios"] = "ovmf"
		// OVMF needs somewhere to keep its variables, and PVE will not start a
		// VM with bios=ovmf and no efidisk0. pre-enrolled-keys=0 leaves Secure
		// Boot off: enrolling Microsoft's keys silently would stop an imported
		// guest with an unsigned bootloader from starting.
		params["efidisk0"] = fmt.Sprintf("%s:1,efitype=4m,pre-enrolled-keys=0", storage)
	}
	if opts.Password != "" {
		params["cipassword"] = opts.Password
	}
	if opts.User != "" {
		params["ciuser"] = opts.User
	}
	if opts.SSHKeys != "" {
		// PVE wants this URL-encoded; url.Values does that on encode.
		params["sshkeys"] = opts.SSHKeys
	}
	if len(opts.Tags) > 0 {
		params["tags"] = strings.Join(opts.Tags, ",")
	}
	return form(params), KindQemu, nil
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ── types.Backend ─────────────────────────────────────────────────
//
// The nine-method interface the CLI dispatches through. Everything richer lives
// on Client, which is where the parity work will attach the per-operation
// interfaces (docs/backend-parity.md, step 2) rather than growing this one.

// Backend adapts a Client to types.Backend.
type Backend struct{ Client *Client }

func (b Backend) ListVMs() ([]types.VM, error) { return b.Client.List() }
func (b Backend) VMExists(name string) bool    { return b.Client.Exists(name) }

func (b Backend) StartVM(name string) error {
	_, err := b.Client.Start(name)
	return err
}

func (b Backend) StopVM(name string) error {
	_, err := b.Client.Stop(name)
	return err
}

func (b Backend) DeleteVM(name string) error {
	_, err := b.Client.Delete(name)
	return err
}

func (b Backend) VMInfo(name string) ([]byte, error) {
	status, err := b.Client.Status(name)
	if err != nil {
		return nil, err
	}
	cfg, err := b.Client.GuestConfig(name)
	if err != nil {
		return nil, err
	}
	address, _ := b.Client.Address(name)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Status:      %s\n", status.Status)
	if status.Paused() {
		fmt.Fprintf(&sb, "             (paused)\n")
	}
	fmt.Fprintf(&sb, "Cores:       %d\n", cfg.Cores)
	fmt.Fprintf(&sb, "Memory:      %d MiB\n", cfg.Memory)
	fmt.Fprintf(&sb, "Address:     %s\n", orDash(address))
	fmt.Fprintf(&sb, "Guest agent: %v\n", status.AgentConnected())
	fmt.Fprintf(&sb, "Tags:        %s\n", orDash(cfg.Tags))
	if status.Lock != "" {
		fmt.Fprintf(&sb, "Lock:        %s\n", status.Lock)
	}
	return []byte(sb.String()), nil
}

// SSH connects to the guest over plain ssh. PVE offers no tunnel of its own —
// there is no virtctl equivalent — so the address comes from the guest agent
// (VM) or the container's interface list, and the rest is ordinary ssh.
func (b Backend) SSH(name, username, identityFile, command string, port int, password string, localForwards []string) error {
	address, err := b.Client.Address(name)
	if err != nil {
		return err
	}
	if address == "" {
		return fmt.Errorf("proxmox: %s has no address Corral can see; "+
			"install and enable the QEMU guest agent, or connect through the console", name)
	}
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = "root"
	}
	if port == 0 {
		port = 22
	}
	args := []string{"-o", "StrictHostKeyChecking=accept-new", "-p", strconv.Itoa(port)}
	if identityFile != "" {
		args = append(args, "-i", identityFile)
	}
	for _, forward := range localForwards {
		args = append(args, "-L", forward)
	}
	args = append(args, username+"@"+address)
	if command != "" {
		args = append(args, command)
	}
	return runInteractive("ssh", args...)
}

// Viewer has no local counterpart: PVE's console is a browser websocket, not a
// VNC port a local client can dial. The refusal names the alternative rather
// than failing obscurely.
func (b Backend) Viewer(name string) error {
	return fmt.Errorf("proxmox consoles are browser-only (PVE serves them over a websocket); "+
		"open the console in `corral web`, or use `corral ssh %s`", name)
}

// Logs maps onto the guest's task history, which is the closest thing PVE has.
func (b Backend) Logs(name string) error {
	entries, err := b.Client.Events(name)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No recent tasks for this guest.")
		return nil
	}
	for _, e := range entries {
		status := e.ExitStatus
		if status == "" {
			status = e.Status
		}
		fmt.Printf("%-24s %-12s %s\n", e.Type, status, e.User)
	}
	return nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
