// Package incus provides Incus (LXD fork) instance management for Corral.
package incus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

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

// List converts Incus instances into Corral types.VM slice.
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
