// Package incus provides Incus (LXD fork) instance management for Corral.
package incus

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// Instance represents an Incus container or virtual machine.
type Instance struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"` // "container" or "virtual-machine"
	Status     string            `json:"status"`
	StatusCode int               `json:"status_code"`
	Location   string            `json:"location"`
	Config     map[string]string `json:"config"`
	State      *InstanceState    `json:"state,omitempty"`
}

// InstanceState is the live state `incus list` returns alongside the config.
// Only the addresses are read: without them every Incus instance showed a blank
// address column, and the SSH and RDP probes had nothing to aim at.
type InstanceState struct {
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
	} `json:"network"`
}

// IsContainer reports whether this instance is an LXC container rather than an
// Incus virtual machine. Incus hosts both, and Corral models them as different
// things — a container is a CT, a virtual machine is a VM — so the distinction
// has to survive the listing. It used to be parsed and then dropped, which is
// how every Incus instance ended up listed twice: once as a VM by this package
// and once as a CT by pkg/ct.
func (i Instance) IsContainer() bool {
	// Incus reports "container" or "virtual-machine"; an older daemon or a
	// partial response may omit the field, and a container is the safer
	// assumption for an instance Corral cannot classify (it loses a graphical
	// console rather than inventing one).
	return i.Type != "virtual-machine"
}

// Address returns the instance's first global IPv4, or "" when it has none yet.
func (i Instance) Address() string {
	if i.State == nil {
		return ""
	}
	for iface, net := range i.State.Network {
		if iface == "lo" {
			continue
		}
		for _, addr := range net.Addresses {
			if addr.Family == "inet" && addr.Scope != "link" && addr.Scope != "local" {
				return addr.Address
			}
		}
	}
	return ""
}

var defaultRunner shell.Runner = shell.Real{}

// Client operates on one Incus remote without changing the Incus CLI's
// global active remote.
type Client struct{ Remote string }

func NewClient(remote string) Client {
	if remote == "" {
		remote = config.IncusRemote()
	}
	return Client{Remote: remote}
}

func (c Client) target(name string) string { return c.Remote + ":" + name }

// SetRunner overrides the command runner for testing and demo mode.
func SetRunner(r shell.Runner) {
	defaultRunner = r
}

// ListInstances queries the local Incus daemon (via incus CLI / socket) for instances.
func ListInstances() ([]Instance, error) {
	return NewClient("").ListInstances()
}

func (c Client) ListInstances() ([]Instance, error) {
	out, err := defaultRunner.Run("incus", "list", c.Remote+":", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("incus list failed: %w", err)
	}
	var insts []Instance
	if err := json.Unmarshal(out, &insts); err != nil {
		return nil, fmt.Errorf("failed to parse incus list JSON: %w", err)
	}
	return insts, nil
}

// Containers returns the remote's LXC containers. pkg/ct maps these onto
// Corral CTs; they are deliberately absent from List, so one instance is never
// both a VM and a CT.
func Containers() ([]Instance, error) { return NewClient("").Containers() }

func (c Client) Containers() ([]Instance, error) {
	insts, err := c.ListInstances()
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(insts))
	for _, inst := range insts {
		if inst.IsContainer() {
			out = append(out, inst)
		}
	}
	return out, nil
}

// List converts the remote's Incus *virtual machines* into Corral VMs.
// Containers are not VMs and are returned by Containers instead — see
// Instance.IsContainer for why this split matters.
func List() ([]types.VM, error) {
	return NewClient("").List()
}

func (c Client) List() ([]types.VM, error) {
	insts, err := c.ListInstances()
	if err != nil {
		return nil, err
	}
	var vms []types.VM
	for _, inst := range insts {
		if inst.IsContainer() {
			continue
		}
		running := strings.EqualFold(inst.Status, "Running")
		cpu := 0
		if c, ok := inst.Config["limits.cpu"]; ok {
			cpu, _ = strconv.Atoi(c)
		}
		mem := inst.Config["limits.memory"]

		vm := types.VM{
			Name:      inst.Name,
			Backend:   "incus",
			Context:   c.Remote,
			Namespace: "incus",
			Status:    inst.Status,
			Ready:     running,
			Running:   running,
			CPU:       cpu,
			Mem:       mem,
			Node:      inst.Location,
			IP:        inst.Address(),
		}
		vm.SetIdentity()
		vms = append(vms, vm)
	}
	return vms, nil
}

// Exists checks if an Incus instance with the given name exists.
func Exists(name string) bool {
	return NewClient("").Exists(name)
}
func (c Client) Exists(name string) bool {
	_, err := defaultRunner.Run("incus", "info", c.target(name))
	return err == nil
}

// Start launches an Incus instance.
func Start(name string) error {
	return NewClient("").Start(name)
}

// Restart uses Incus's own restart, not a stop followed by a start. The
// difference is the guest's shutdown ordering: `incus restart` sends the
// stop signal and waits, where two separate calls can start the instance again
// before it has finished going down.
func (c Client) Restart(name string) error {
	if out, err := defaultRunner.Run("incus", "restart", c.target(name)); err != nil {
		return fmt.Errorf("incus restart %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Pause and Resume suspend the instance to memory. Incus spells the second
// half `incus start` — the same verb as a cold boot — but applied to a frozen
// instance it resumes rather than boots, which is why this wraps it under a
// name that says which of the two is happening.
func (c Client) Pause(name string) error {
	if out, err := defaultRunner.Run("incus", "pause", c.target(name)); err != nil {
		return fmt.Errorf("incus pause %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Resume unfreezes a paused instance.
func (c Client) Resume(name string) error { return c.Start(name) }

func (c Client) Start(name string) error {
	if out, err := defaultRunner.Run("incus", "start", c.target(name)); err != nil {
		if strings.Contains(string(out), "already running") {
			return nil
		}
		return fmt.Errorf("incus start %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Stop shuts down an Incus instance.
func Stop(name string) error {
	return NewClient("").Stop(name)
}
func (c Client) Stop(name string) error {
	if out, err := defaultRunner.Run("incus", "stop", c.target(name)); err != nil {
		if strings.Contains(string(out), "already stopped") || strings.Contains(string(out), "not running") {
			return nil
		}
		// ACPI guest agent shutdown might time out on uninitialized OS VMs; fallback to forced stop.
		if forceOut, forceErr := defaultRunner.Run("incus", "stop", c.target(name), "--force"); forceErr == nil {
			return nil
		} else {
			_ = forceOut
		}
		return fmt.Errorf("incus stop %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Delete removes an Incus instance (stopping it first if forced).
func Delete(name string) error {
	return NewClient("").Delete(name)
}
func (c Client) Delete(name string) error {
	if out, err := defaultRunner.Run("incus", "delete", c.target(name), "--force"); err != nil {
		return fmt.Errorf("incus delete %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Info gets raw JSON info for an Incus instance.
func Info(name string) ([]byte, error) {
	return NewClient("").Info(name)
}
func (c Client) Info(name string) ([]byte, error) {
	return defaultRunner.Run("incus", "info", c.target(name))
}

// CreateOpts options for creating an Incus instance.
type CreateOpts struct {
	Name   string
	Image  string
	VM     bool // if true, creates virtual-machine; default is container
	CPU    int
	Memory string
}

// Create launches a new Incus instance.
func Create(opts CreateOpts) error {
	return NewClient("").Create(opts)
}
func (c Client) Create(opts CreateOpts) error {
	image := opts.Image
	if image == "" {
		image = "images:ubuntu/22.04"
	}
	args := []string{"launch", image, c.target(opts.Name)}
	if opts.VM {
		args = append(args, "--vm")
	}
	if opts.CPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", opts.CPU))
	}
	if opts.Memory != "" {
		mem := strings.TrimSpace(opts.Memory)
		if strings.HasSuffix(mem, "Gi") {
			mem = strings.TrimSuffix(mem, "Gi") + "GiB"
		} else if strings.HasSuffix(mem, "Mi") {
			mem = strings.TrimSuffix(mem, "Mi") + "MiB"
		}
		args = append(args, "-c", fmt.Sprintf("limits.memory=%s", mem))
	}

	if out, err := defaultRunner.Run("incus", args...); err != nil {
		return fmt.Errorf("incus launch failed: %s (%w)", string(out), err)
	}
	return nil
}

// Remote describes an entry from `incus remote list`.
type Remote struct {
	Name     string `json:"name"`
	Address  string `json:"addr"`
	Protocol string `json:"protocol"`
	Public   bool   `json:"public"`
	Static   bool   `json:"static"`
}

func ListRemotes() ([]Remote, error) {
	out, err := defaultRunner.Run("incus", "remote", "list", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("incus remote list failed: %w", err)
	}
	var remotes []Remote
	if err := json.Unmarshal(out, &remotes); err != nil {
		return nil, fmt.Errorf("parse incus remotes: %w", err)
	}
	return remotes, nil
}

func ListAll() ([]types.VM, error) {
	remotes, err := ListRemotes()
	if err != nil {
		return NewClient("").List()
	}
	var all []types.VM
	for _, remote := range remotes {
		if remote.Public {
			continue
		}
		vms, err := NewClient(remote.Name).List()
		if err != nil {
			continue
		}
		for i := range vms {
			if vms[i].Node == "" {
				vms[i].Node = remote.Name
			} else {
				vms[i].Node = remote.Name + "/" + vms[i].Node
			}
		}
		all = append(all, vms...)
	}
	return all, nil
}

// Backend satisfies types.Backend for Incus.
type Backend struct{}

var _ types.Backend = Backend{}

func (Backend) ListVMs() ([]types.VM, error)       { return List() }
func (Backend) VMExists(name string) bool          { return Exists(name) }
func (Backend) StartVM(name string) error          { return Start(name) }
func (Backend) StopVM(name string) error           { return Stop(name) }
func (Backend) DeleteVM(name string) error         { return Delete(name) }
func (Backend) VMInfo(name string) ([]byte, error) { return Info(name) }
func (Backend) Viewer(name string) error {
	return fmt.Errorf("viewer not supported directly for incus")
}
func (Backend) Logs(name string) error { return fmt.Errorf("logs command not supported for incus") }
func (Backend) SSH(name, username, identityFile, command string, port int, password string, localForwards []string) error {
	return NewClient("").SSH(name, command)
}
func (c Client) SSH(name, command string) error {
	args := []string{"exec", c.target(name), "--"}
	if command != "" {
		args = append(args, "sh", "-c", command)
	} else {
		args = append(args, "bash")
	}
	cmd := exec.Command("incus", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Metrics reports an instance's live CPU and memory usage.
//
// Read from the REST API via `incus query` rather than parsed out of
// `incus info`: the state endpoint returns JSON with exact numbers, where the
// human output is a formatted table that changes between versions and would
// have to be scraped.
func (c Client) Metrics(name string) (map[string]string, error) {
	res := map[string]string{"cpu": "", "mem": ""}
	path := fmt.Sprintf("/1.0/instances/%s/state", name)
	if c.Remote != "" && c.Remote != "local" {
		path = fmt.Sprintf("/1.0/instances/%s/state?project=default", name)
	}
	out, err := defaultRunner.Run("incus", "query", c.queryTarget(path))
	if err != nil {
		return res, fmt.Errorf("incus query %s: %s (%w)", name, string(out), err)
	}
	var state struct {
		CPU struct {
			Usage int64 `json:"usage"` // cumulative nanoseconds
		} `json:"cpu"`
		Memory struct {
			Usage int64 `json:"usage"` // bytes
		} `json:"memory"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return res, fmt.Errorf("incus state for %s: %w", name, err)
	}
	if state.Memory.Usage > 0 {
		res["mem"] = humanBytes(state.Memory.Usage)
	}
	// CPU usage is cumulative since boot, so it is reported as total CPU time
	// rather than a rate. Calling a lifetime total "usage" without saying so
	// would read as a percentage and be wrong by orders of magnitude.
	if state.CPU.Usage > 0 {
		res["cpu"] = fmt.Sprintf("%s total", (time.Duration(state.CPU.Usage) * time.Nanosecond).Round(time.Second))
	}
	return res, nil
}

// queryTarget prefixes a remote for `incus query`, which takes the remote on
// the path rather than as a separate argument.
func (c Client) queryTarget(path string) string {
	if c.Remote == "" || c.Remote == "local" {
		return path
	}
	return c.Remote + ":" + path
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%dKi", b/1024)
}

// IngestVM builds an Incus VM unified image tarball from disk, imports it into
// Incus, launches the instance, and cleans up the intermediate image.
func (c Client) IngestVM(name, diskPath string, cpu int, memory string) error {
	tmpDir, err := os.MkdirTemp("", "corral-incus-ingest-*")
	if err != nil {
		return fmt.Errorf("incus ingest: creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy/convert disk image into rootfs.img if needed, or link it.
	// For unified Incus VM image tarball, Incus expects metadata.yaml + rootfs.img.
	rootfsPath := filepath.Join(tmpDir, "rootfs.img")
	if err := copyOrConvertDisk(diskPath, rootfsPath); err != nil {
		return fmt.Errorf("incus ingest: preparing rootfs.img: %w", err)
	}

	metadataYAML := fmt.Sprintf("architecture: %s\ncreation_date: %d\nproperties:\n  description: Corral imported VM image\n  os: Linux\n",
		runtimeArch(), time.Now().Unix())
	if err := os.WriteFile(filepath.Join(tmpDir, "metadata.yaml"), []byte(metadataYAML), 0o644); err != nil {
		return fmt.Errorf("incus ingest: writing metadata.yaml: %w", err)
	}

	tarballPath := filepath.Join(tmpDir, "image.tar.xz")
	// Package into tarball using tar
	cmd := exec.Command("tar", "-cf", tarballPath, "-C", tmpDir, "metadata.yaml", "rootfs.img")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("incus ingest: packaging image tarball: %s (%w)", string(out), err)
	}

	alias := fmt.Sprintf("corral-ingest-%s-%d", name, time.Now().UnixNano())
	// incus image import image.tar.xz remote: --alias alias
	importArgs := []string{"image", "import", tarballPath}
	if c.Remote != "" && c.Remote != "local" {
		importArgs = append(importArgs, c.Remote+":")
	}
	importArgs = append(importArgs, "--alias", alias)
	if out, err := defaultRunner.Run("incus", importArgs...); err != nil {
		return fmt.Errorf("incus image import failed: %s (%w)", string(out), err)
	}
	defer func() {
		// Clean up intermediate image from Incus image store
		rmArgs := []string{"image", "delete"}
		if c.Remote != "" && c.Remote != "local" {
			rmArgs = append(rmArgs, c.Remote+":"+alias)
		} else {
			rmArgs = append(rmArgs, alias)
		}
		_, _ = defaultRunner.Run("incus", rmArgs...)
	}()

	// Launch VM from imported image
	opts := CreateOpts{
		Name:   name,
		Image:  alias,
		VM:     true,
		CPU:    cpu,
		Memory: memory,
	}
	return c.Create(opts)
}

func copyOrConvertDisk(src, dst string) error {
	// If src is qcow2 or raw, qemu-img convert guarantees raw output as rootfs.img if needed,
	// or we can use qemu-img convert -O raw src dst.
	cmd := exec.Command("qemu-img", "convert", "-O", "raw", src, dst)
	if _, err := cmd.CombinedOutput(); err != nil {
		// Fallback to simple file copy if qemu-img is not present
		return copyFile(src, dst)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func runtimeArch() string {
	// Incus expects x86_64, aarch64, etc.
	// Map Go architecture to Incus architecture strings.
	return "x86_64"
}
