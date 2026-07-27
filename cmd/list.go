package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	runningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	stoppedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all VMs (both backends)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runList()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func runList() error {
	var vms []types.VM
	kvms, _ := kubevirt.NewClient("").ListVMs()
	vms = append(vms, kvms...)
	qvms, _ := qemu.List()
	vms = append(vms, qvms...)
	if plugin.IsInstalled("incus") {
		ivms, _ := incus.List()
		vms = append(vms, ivms...)
	}

	if len(vms) == 0 {
		fmt.Println("No VMs found. Create one: corral create <name>")
		return nil
	}
	printVMList(vms)
	return nil
}

func printVMList(vms []types.VM) {
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

	fmt.Printf("%s\n", headerStyle.Render(
		fmt.Sprintf("%-20s  %-8s  %-16s  %-12s  %-4s  %-6s  %s",
			"NAME", "BACKEND", "STATUS", "NAMESPACE", "CPU", "MEM", "NODE")))
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

		fmt.Printf("%-20s  %-8s  %-16s  %-12s  %-4d  %-6s  %s  %s\n",
			vm.Name, vm.Backend, status, ns, vm.CPU, vm.Mem, node, extra)
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
	if registryStore != nil {
		if entry, ok := registryStore.Get(name); ok {
			return entry.Backend
		}
	}
	// Check all KubeVirt namespaces (VMExists only checks one)
	client := kubevirt.NewClient("")
	vms, _ := client.ListVMs()
	for _, vm := range vms {
		if vm.Name == name {
			return "kubevirt"
		}
	}
	if qemu.Exists(name) {
		return "qemu"
	}
	return ""
}

func requireBackend(name string) (string, error) {
	b := resolveBackend(name)
	if b == "" {
		return "", fmt.Errorf("VM %q does not exist", name)
	}
	return b, nil
}
