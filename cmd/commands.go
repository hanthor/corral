package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

var (
	forceDelete      bool
	sshUser          string
	sshIdentity      string
	sshCommand       string
	sshPort          int
	sshPassword      string
	sshLocalForwards []string
)

var startCmd = &cobra.Command{
	Use:     "start [name]",
	Short:   "Start a VM",
	Example: `  corral start myvm`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "start")
		if err != nil {
			return err
		}
		if p, ok, err := resolvePeerTarget(name); ok {
			if err != nil {
				return err
			}
			return p.action("POST", "start")
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			return kubevirt.NewClient(ns).StartVM(name)
		}
		if backend == "incus" {
			return incus.Start(name)
		}
		if backend == "libvirt" {
			return libvirt.NewClient("").Start(name)
		}
		return qemu.Start(name)
	},
}

var stopCmd = &cobra.Command{
	Use:     "stop [name]",
	Short:   "Stop a VM",
	Example: `  corral stop myvm`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "stop")
		if err != nil {
			return err
		}
		if p, ok, err := resolvePeerTarget(name); ok {
			if err != nil {
				return err
			}
			return p.action("POST", "stop")
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			return kubevirt.NewClient(ns).StopVM(name)
		}
		if backend == "incus" {
			return incus.Stop(name)
		}
		if backend == "libvirt" {
			return libvirt.NewClient("").Stop(name)
		}
		return qemu.Stop(name)
	},
}

var deleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a VM and its disks",
	Example: `  corral delete myvm       # asks to confirm
  corral delete myvm -f    # skip the confirmation`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "delete")
		if err != nil {
			return err
		}
		p, peer, peerErr := resolvePeerTarget(name)
		if peerErr != nil {
			return peerErr
		}
		backend := ""
		if !peer {
			backend, err = requireBackend(name)
			if err != nil {
				return err
			}
		}
		if !forceDelete {
			fmt.Fprintf(os.Stderr, "Delete VM %q and its disks? [y/N] ", name)
			var resp string
			fmt.Fscanln(os.Stdin, &resp)
			if resp != "y" && resp != "Y" && resp != "yes" {
				return fmt.Errorf("aborted")
			}
		}
		if peer {
			return p.action("DELETE", "")
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			if err := kubevirt.NewClient(ns).DeleteVM(name); err != nil {
				return err
			}
		} else if backend == "incus" {
			if err := incus.Delete(name); err != nil {
				return err
			}
		} else if backend == "libvirt" {
			if err := libvirt.NewClient("").Delete(name); err != nil {
				return err
			}
		} else if err := qemu.Delete(name); err != nil {
			return err
		}
		if registryStore != nil {
			registryStore.Remove(name)
		}
		return nil
	},
}

var infoCmd = &cobra.Command{
	Use:     "info [name]",
	Short:   "Show VM details",
	Example: `  corral info myvm`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "view details for")
		if err != nil {
			return err
		}
		if p, ok, err := resolvePeerTarget(name); ok {
			if err != nil {
				return err
			}
			return p.action("GET", "")
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			data, err := kubevirt.NewClient(ns).VMInfo(name)
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		if backend == "incus" {
			data, err := incus.Info(name)
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		if backend == "libvirt" {
			data, err := libvirt.NewClient("").Info(name)
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		data, err := qemu.Info(name)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	},
}

var viewerCmd = &cobra.Command{
	Use:     "viewer [name]",
	Short:   "Launch VNC viewer for a VM",
	Example: `  corral viewer myvm`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "view")
		if err != nil {
			return err
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			return kubevirt.NewClient(ns).Viewer(name)
		}
		return qemu.Viewer(name)
	},
}

// resolveSSHCredentials resolves the username/password for `corral ssh
// <name>`, checking (in order) the explicit flag, then whatever's
// remembered in the registry, then $USER/"root" for username. If
// flagUser was passed explicitly and differs from what's remembered, it's
// saved back to the registry — so a user only needs `-u` once per host,
// not on every `corral ssh` call.
func resolveSSHCredentials(name, backend, flagUser, flagPassword string) (user, password string) {
	var saved types.RegistryEntry
	var hadEntry bool
	if registryStore != nil {
		saved, hadEntry = registryStore.Get(name)
	}

	user = flagUser
	if user == "" {
		user = saved.Username
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = "root"
	}

	if flagUser != "" && flagUser != saved.Username && registryStore != nil {
		updated := saved
		updated.Username = flagUser
		if !hadEntry {
			updated.Backend = backend
		}
		registryStore.Set(name, updated)
	}

	password = flagPassword
	if password == "" {
		password = saved.Password
	}
	return user, password
}

var sshCmd = &cobra.Command{
	Use:   "ssh [name]",
	Short: "Open an SSH session to a VM",
	Long: `Open an interactive SSH session to a VM.

For KubeVirt VMs, this uses virtctl ssh which tunnels through the
Kubernetes API. For QEMU VMs, it connects to the VM's Tailscale IP.

-L forwards a local port through the VM, same as ssh -L: [bind_address:]port:host:hostport.
Repeatable. host is resolved from the VM's side, so "localhost" there means
the VM itself, not your machine — e.g. -L 8080:localhost:80 reaches
something the VM has listening on its own port 80.`,
	Example: `  corral ssh myvm
  corral ssh myvm --user root
  corral ssh myvm -u root -i ~/.ssh/vm_key
  corral ssh myvm -c "ls /"
  corral ssh myvm -L 8080:localhost:80
  corral ssh myvm -L 5432:localhost:5432 -L 6379:localhost:6379`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "ssh into")
		if err != nil {
			return err
		}
		if p, ok, err := resolvePeerTarget(name); ok {
			if err != nil {
				return err
			}
			user := sshUser
			if user == "" {
				user = os.Getenv("USER")
			}
			if user == "" {
				user = "root"
			}
			return p.ssh(user, sshCommand)
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}

		user, password := resolveSSHCredentials(name, backend, sshUser, sshPassword)

		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			return kubevirt.NewClient(ns).SSH(name, user, sshIdentity, sshCommand, sshPort, password, sshLocalForwards)
		}
		if backend == "incus" {
			return incus.NewClient("").SSH(name, sshCommand)
		}
		if backend == "libvirt" {
			return libvirt.NewClient("").SSH(name, sshCommand)
		}
		return qemu.SSH(name, user, sshIdentity, sshCommand, sshPort, password, sshLocalForwards)
	},
}

var logsCmd = &cobra.Command{
	Use:     "logs [name]",
	Short:   "Tail VM logs",
	Example: `  corral logs myvm`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := requireOrPrompt(args, "view logs for")
		if err != nil {
			return err
		}
		backend, err := requireBackend(name)
		if err != nil {
			return err
		}
		if backend == "kubevirt" {
			ns, _ := resolveNamespace(name)
			return kubevirt.NewClient(ns).Logs(name)
		}
		return qemu.Logs(name)
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(infoCmd)
	rootCmd.AddCommand(viewerCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(logsCmd)

	deleteCmd.Flags().BoolVarP(&forceDelete, "force", "f", false, "Skip confirmation")
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "", "SSH username (default: $USER)")
	sshCmd.Flags().StringVarP(&sshIdentity, "identity", "i", "", "Path to SSH private key")
	sshCmd.Flags().StringVarP(&sshCommand, "command", "c", "", "Command to execute (non-interactive)")
	sshCmd.Flags().IntVarP(&sshPort, "port", "p", 22, "SSH port")
	sshCmd.Flags().StringVar(&sshPassword, "password", "", "SSH password (uses cloud-init password if empty)")
	sshCmd.Flags().StringArrayVarP(&sshLocalForwards, "local-forward", "L", nil, "Forward a local port through the VM: [bind_address:]port:host:hostport (repeatable)")
}

func requireOrPrompt(args []string, action string) (string, error) {
	if len(args) == 1 && args[0] != "" {
		for _, peer := range config.Peers() {
			if strings.HasPrefix(args[0], peer.Name+"/") {
				return args[0], nil
			}
		}
		vm, err := resolveVM(args[0])
		if err != nil {
			if registryStore != nil {
				if entry, ok := registryStore.Get(args[0]); ok {
					fallback := types.VM{Name: args[0], Backend: entry.Backend, Context: entry.Context, Namespace: entry.Namespace}
					selectedVM = &fallback
					activateVMContext(fallback)
					return args[0], nil
				}
			}
			return "", err
		}
		selectedVM = &vm
		activateVMContext(vm)
		return vm.Name, nil
	}
	names := allVMNames()
	if len(names) == 0 {
		return "", fmt.Errorf("no VMs found. Create one: corral create <name>")
	}
	fmt.Fprintf(os.Stderr, "Available VMs to %s:\n", action)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %s\n", n)
	}
	return "", fmt.Errorf("please specify a VM name")
}

func allVMNames() []string {
	result := fleet.List(context.Background())
	counts := map[string]int{}
	for _, vm := range result.VMs {
		counts[vm.Name]++
	}
	var names []string
	for _, vm := range result.VMs {
		if counts[vm.Name] == 1 {
			names = append(names, vm.Name)
		} else {
			names = append(names, vm.ID)
		}
	}
	return uniq(names)
}

func resolveNamespace(name string) (string, string) {
	if selectedVM != nil && selectedVM.Name == name {
		activateVMContext(*selectedVM)
		return selectedVM.Namespace, selectedVM.Backend
	}
	if vm, err := resolveVM(name); err == nil {
		activateVMContext(vm)
		return vm.Namespace, vm.Backend
	}
	if registryStore != nil {
		if entry, ok := registryStore.Get(name); ok && entry.Namespace != "" {
			return entry.Namespace, entry.Backend
		}
	}
	// Search all KubeVirt namespaces for the VM
	client := kubevirt.NewClient("")
	vms, _ := client.ListVMs()
	for _, vm := range vms {
		if vm.Name == name {
			return vm.Namespace, "kubevirt"
		}
	}
	return kubevirt.DefaultNamespace, resolveBackend(name)
}
