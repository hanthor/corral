package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/types"
)

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	runningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	stoppedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

var selectedVM *types.VM

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List VMs across every configured context",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runList()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func runList() error {
	result := fleet.List(context.Background())
	vms := result.VMs

	if len(vms) == 0 {
		fmt.Println("No VMs found. Create one: corral create <name>")
		return nil
	}
	printVMList(vms)
	for target, message := range result.Errors {
		fmt.Printf("warning: context %s unavailable: %s\n", target, message)
	}
	return nil
}

func printVMList(vms []types.VM) {
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

	fmt.Printf("%s\n", headerStyle.Render(
		fmt.Sprintf("%-20s  %-10s  %-16s  %-18s  %-12s  %-4s  %-6s  %s",
			"NAME", "BACKEND", "STATUS", "CONTEXT", "NAMESPACE", "CPU", "MEM", "NODE")))
	fmt.Println("────────────────────────────────────────────────────────────────────────────────────────────────────")

	for _, vm := range vms {
		status := stoppedStyle.Render("○ Stopped")
		if vm.Ready {
			status = runningStyle.Render("● Running")
		} else if vm.Running {
			status = "◐ Starting"
		}

		ns := vm.Namespace
		if ns == "" {
			ns = "—"
		}
		node := vm.Node
		if node == "" {
			node = "—"
		}

		extra := ""
		switch vm.Backend {
		case "kubevirt":
			proxyIcon := "○"
			if vm.VNC == "on" {
				proxyIcon = "●"
			} else if vm.VNC == "pending" {
				proxyIcon = "◐"
			}
			extra = "ports:" + proxyIcon
		case "qemu":
			if vm.VNC != "" {
				extra = "vnc:" + vm.VNC
			}
		}
		if vm.Ephemeral {
			extra += "  ⏳" + ephemeralSummary(vm)
		}

		contextName := vm.Context
		if contextName == "" {
			contextName = "local"
		}
		if vm.Peer != "" {
			contextName = vm.Peer + "/" + contextName
		}
		fmt.Printf("%-20s  %-10s  %-16s  %-18s  %-12s  %-4d  %-6s  %s  %s\n",
			vm.Name, vm.Backend, status, contextName, ns, vm.CPU, vm.Mem, node, extra)
		fmt.Printf("  id: %s\n", vm.ID)
	}
}

// ephemeralSummary renders a short human hint for `corral list`: time left
// before `corral gc` stops the VM, or how it's progressing through the two
// gc stages once past that point.
func ephemeralSummary(vm types.VM) string {
	if vm.Running {
		expiresAt, err := time.Parse(time.RFC3339, vm.ExpiresAt)
		if err != nil {
			return " (no valid TTL)"
		}
		if left := time.Until(expiresAt); left > 0 {
			return fmt.Sprintf(" %s left", left.Round(time.Minute))
		}
		return " gc: stop due"
	}
	if vm.StoppedAt != "" {
		return " gc-stopped"
	}
	return ""
}

func resolveBackend(name string) string {
	if selectedVM != nil && selectedVM.Name == name {
		activateVMContext(*selectedVM)
		return selectedVM.Backend
	}
	vm, err := resolveVM(name)
	if err != nil {
		if registryStore != nil {
			if entry, ok := registryStore.Get(name); ok {
				activateVMContext(types.VM{Backend: entry.Backend, Context: entry.Context})
				return entry.Backend
			}
		}
		return ""
	}
	activateVMContext(vm)
	return vm.Backend
}

func requireBackend(name string) (string, error) {
	if selectedVM != nil && selectedVM.Name == name {
		activateVMContext(*selectedVM)
		return selectedVM.Backend, nil
	}
	vm, err := resolveVM(name)
	if err != nil {
		if registryStore != nil {
			if entry, ok := registryStore.Get(name); ok {
				activateVMContext(types.VM{Backend: entry.Backend, Context: entry.Context})
				return entry.Backend, nil
			}
		}
		return "", err
	}
	activateVMContext(vm)
	return vm.Backend, nil
}

func resolveVM(selector string) (types.VM, error) {
	return fleet.Resolve(fleet.List(context.Background()).VMs, selector)
}

func activateVMContext(vm types.VM) {
	switch vm.Backend {
	case "kubevirt":
		_ = os.Setenv("CORRAL_KUBE_CONTEXT", vm.Context)
	case "incus":
		_ = os.Setenv("CORRAL_INCUS_REMOTE", vm.Context)
	case "libvirt":
		_ = os.Setenv("CORRAL_LIBVIRT_URI", vm.Context)
	}
}
