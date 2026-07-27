// Package incus provides Incus (LXD fork) instance management for Corral.
package incus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

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

// SetRunner overrides the command runner for testing and demo mode.
func SetRunner(r shell.Runner) {
	defaultRunner = r
}

// ListInstances queries the local Incus daemon (via incus CLI / socket) for instances.
func ListInstances() ([]Instance, error) {
	out, err := defaultRunner.Run("incus", "list", "--format=json")
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
	insts, err := ListInstances()
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

		vms = append(vms, types.VM{
			Name:    inst.Name,
			Backend: "incus",
			Status:  inst.Status,
			Ready:   running,
			Running: running,
			CPU:     cpu,
			Mem:     mem,
			Node:    inst.Location,
		})
	}
	return vms, nil
}

// Exists checks if an Incus instance with the given name exists.
func Exists(name string) bool {
	cmd := exec.Command("incus", "info", name)
	return cmd.Run() == nil
}

// Start launches an Incus instance.
func Start(name string) error {
	cmd := exec.Command("incus", "start", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already running") {
			return nil
		}
		return fmt.Errorf("incus start %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Stop shuts down an Incus instance.
func Stop(name string) error {
	cmd := exec.Command("incus", "stop", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already stopped") || strings.Contains(string(out), "not running") {
			return nil
		}
		// ACPI guest agent shutdown might time out on uninitialized OS VMs; fallback to forced stop.
		forceCmd := exec.Command("incus", "stop", name, "--force")
		if forceOut, forceErr := forceCmd.CombinedOutput(); forceErr == nil {
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
	cmd := exec.Command("incus", "delete", name, "--force")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("incus delete %s: %s (%w)", name, string(out), err)
	}
	return nil
}

// Info gets raw JSON info for an Incus instance.
func Info(name string) ([]byte, error) {
	return defaultRunner.Run("incus", "info", name)
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
	image := opts.Image
	if image == "" {
		image = "images:ubuntu/22.04"
	}
	args := []string{"launch", image, opts.Name}
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

	cmd := exec.Command("incus", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("incus launch failed: %s (%w)", string(out), err)
	}
	return nil
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
func (Backend) Viewer(name string) error           { return fmt.Errorf("viewer not supported directly for incus") }
func (Backend) Logs(name string) error             { return fmt.Errorf("logs command not supported for incus") }
func (Backend) SSH(name, username, identityFile, command string, port int, password string, localForwards []string) error {
	args := []string{"exec", name, "--"}
	if command != "" {
		args = append(args, "sh", "-c", command)
	} else {
		args = append(args, "bash")
	}
	cmd := exec.Command("incus", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
