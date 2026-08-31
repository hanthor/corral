// corral-vdi is the VDI Corral plugin: Phase 1 of RFC-0001
// (docs/rfc/0001-vdi-plugin.md) — static desktop pools with manual
// assignment, built by cloning an already-built "golden" VM N times
// (kubevirt.Client.Clone) and tracking assignment as plain labels on the
// VM object, not a new CRD. Installed via the marketplace
// (`corral plugin install vdi`) and invoked as `corral vdi`.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/registry"
	"github.com/tuna-os/corral/pkg/types"
	"github.com/tuna-os/corral/pkg/vdi"
)

var (
	namespace   string
	contextName string
	assumeYes   bool
)

func nsOrDefault() string {
	if namespace != "" {
		return namespace
	}
	return kubevirt.DefaultNamespace
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "vdi",
		Short: "Desktop pools — VDI desktop pools, manual assignment, and native USB redirection",
		Long: `Desktop pools built from an already-built "golden" VM (made the normal way,
via corral create / corral bootc / corral-windows). Pool members are clones
of that VM; assignment is a label on the VM object, not a new CRD — see
docs/rfc/0001-vdi-plugin.md for the design and what's still ahead of this
first slice (self-serve claim, idle reclaim, GPU pools).`,
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "Namespace (default: corral's default)")
	root.PersistentFlags().StringVar(&contextName, "context", "", "Corral context to act on (default: the selected context)")
	root.AddCommand(poolCmd(), assignCmd(), unassignCmd(), connectCmd(), usbCmd())
	return root
}

func poolCmd() *cobra.Command {
	pool := &cobra.Command{
		Use:   "pool",
		Short: "Manage desktop pools",
	}
	pool.AddCommand(poolCreateCmd(), poolListCmd(), poolDeleteCmd())
	return pool
}

func poolCreateCmd() *cobra.Command {
	var from string
	var size int
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a pool by cloning an existing VM <size> times",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			ns := nsOrDefault()
			fmt.Fprintf(os.Stderr, "cloning %q → %d pool members in ns/%s...\n", from, size, ns)
			p, err := vdi.CreatePool(vdi.CreateOpts{Name: name, Namespace: ns, From: from, Size: size})
			if err != nil {
				return err
			}
			if store, rerr := registry.NewStore(); rerr == nil {
				for _, m := range p.Members {
					store.Set(m.Name, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
				}
			}
			fmt.Printf("pool %q created: %d members\n", name, len(p.Members))
			for _, m := range p.Members {
				fmt.Printf("  %s\n", m.Name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "Existing VM to clone as the pool template (required)")
	c.Flags().IntVar(&size, "size", 1, "Number of pool members")
	c.MarkFlagRequired("from")
	return c
}

func poolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pools and their members",
		RunE: func(_ *cobra.Command, _ []string) error {
			pools, err := vdi.ListPools()
			if err != nil {
				return err
			}
			if len(pools) == 0 {
				fmt.Println("No pools found.")
				return nil
			}
			for _, p := range pools {
				fmt.Printf("%s  (ns/%s, %d members)\n", p.Name, p.Namespace, len(p.Members))
				for _, m := range p.Members {
					status := "free"
					if m.AssignedTo != "" {
						status = "assigned to " + m.AssignedTo
					}
					running := "stopped"
					if m.Running {
						running = "running"
					}
					fmt.Printf("  %-24s %-24s %s\n", m.Name, status, running)
				}
			}
			return nil
		},
	}
}

func poolDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a pool and all its members",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return vdi.DeletePool(nsOrDefault(), args[0])
		},
	}
}

func assignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assign <pool> <user>",
		Short: "Claim the first free member of a pool for a user (starts it if stopped)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			member, err := vdi.Assign(nsOrDefault(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("assigned %s → %s\n", member, args[1])
			fmt.Printf("connect:  corral vdi connect %s\n", member)
			return nil
		},
	}
}

func unassignCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unassign <member>",
		Short: "Release a member back to its pool's free set and stop it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return vdi.Unassign(nsOrDefault(), args[0])
		},
	}
}

// connectCmd prints how to reach a member — it reuses the same VNC/RDP/SSH
// paths every other Corral VM already has (virtctl vnc, the RDP port
// probe, corral ssh) rather than inventing a new connection mechanism.
// True one-click routing (picking the right protocol automatically) is
// Phase 2 territory once ADR-0002 phase 2's in-browser RDP lands.
func connectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect <member>",
		Short: "Print how to connect to an assigned desktop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name, ns := args[0], nsOrDefault()
			fmt.Printf("VNC (browser or client):  corral web  →  open %s  →  Console\n", name)
			fmt.Printf("RDP (if the guest answers on 3389):  corral viewer %s  (or a native RDP client via virtctl port-forward)\n", name)
			fmt.Printf("SSH (Linux guests):  corral ssh %s\n", name)
			fmt.Printf("(namespace: %s)\n", ns)
			return nil
		},
	}
}

func usbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usb",
		Short: "Manage USB device redirection for assigned desktops",
	}
	cmd.AddCommand(usbListCmd(), usbRedirCmd())
	return cmd
}

func usbListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List local USB devices available for redirection",
		RunE: func(_ *cobra.Command, _ []string) error {
			devs, err := vdi.ListLocalUSBDevices()
			if err != nil {
				return err
			}
			if len(devs) == 0 {
				fmt.Println("No USB devices found on local host.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SELECTOR\tBUS.DEV\tSTATUS\tDESCRIPTION")
			for _, d := range devs {
				status := "available"
				if d.Busy {
					status = "busy (" + d.BusyReason + ")"
				}
				busDev := fmt.Sprintf("%s.%s", d.BusNum, d.DevNum)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Selector(), busDev, status, d.Description)
			}
			return w.Flush()
		},
	}
}

func usbRedirCmd() *cobra.Command {
	var (
		deviceID string
		user     string
	)
	c := &cobra.Command{
		Use:   "redir <member>",
		Short: "Redirect a local USB device to an authorized assigned desktop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			member := args[0]
			ns := nsOrDefault()

			// Check backend context support
			var kubeContext string
			if contextName != "" {
				cfg, ok := config.FindContext(contextName)
				if !ok {
					return fmt.Errorf("unknown context %q (see corral context ls)", contextName)
				}
				if cfg.Backend != "kubevirt" {
					return fmt.Errorf("USB redirection is only supported on the kubevirt backend (context %q is %s)", contextName, cfg.Backend)
				}
				kubeContext = cfg.Context
			} else {
				def := config.DefaultContext()
				if def.Backend == "kubevirt" {
					kubeContext = def.Context
				}
			}

			// Validate member assignment, running state, and authorization
			m, err := vdi.ValidateUSBRedir(vdi.USBRedirOpts{
				Namespace: ns,
				Member:    member,
				Device:    deviceID,
				User:      user,
				Context:   kubeContext,
			})
			if err != nil {
				return err
			}

			// Device enumeration & selection
			devs, err := vdi.ListLocalUSBDevices()
			if err != nil {
				return err
			}

			var selectedDev *vdi.USBDevice
			if deviceID != "" {
				for _, d := range devs {
					if d.Selector() == deviceID || fmt.Sprintf("%s:%s", d.VendorID, d.ProductID) == deviceID || fmt.Sprintf("%s.%s", d.BusNum, d.DevNum) == deviceID {
						selectedDev = &d
						break
					}
				}
				if selectedDev == nil {
					return fmt.Errorf("USB device %q not found on local host (see `corral vdi usb list`)", deviceID)
				}
			} else {
				// Interactive / first available or require --device
				var avail []vdi.USBDevice
				for _, d := range devs {
					if !d.Busy {
						avail = append(avail, d)
					}
				}
				if len(avail) == 0 {
					return fmt.Errorf("no available USB devices found on local host")
				}
				if len(avail) == 1 {
					selectedDev = &avail[0]
				} else {
					return fmt.Errorf("multiple USB devices available; specify --device <selector> (see `corral vdi usb list`)")
				}
			}

			if selectedDev.Busy {
				return fmt.Errorf("device %s (%s) is busy: %s", selectedDev.Selector(), selectedDev.Description, selectedDev.BusyReason)
			}

			// Surface security, exclusivity, disconnect, and migration implications
			fmt.Fprintf(os.Stderr, "\nAbout to redirect USB device %s (%s) to %s (assigned to %s):\n",
				selectedDev.Selector(), selectedDev.Description, member, m.AssignedTo)
			for _, consequence := range vdi.USBConsequences(*selectedDev) {
				fmt.Fprintf(os.Stderr, "  • %s\n", consequence)
			}

			if !assumeYes {
				fmt.Fprint(os.Stderr, "\nProceed with USB redirection? [y/N] ")
				var answer string
				fmt.Scanln(&answer)
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return fmt.Errorf("cancelled")
				}
			}

			client := kubevirt.NewClient(ns)
			client.Context = kubeContext
			virtctlPath, err := client.Virtctl()
			if err != nil {
				return err
			}

			cmd := vdi.BuildVirtctlUSBCmd(virtctlPath, member, ns, selectedDev.Selector(), kubeContext)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			fmt.Fprintf(os.Stderr, "Starting native USB redirection for %s -> %s... (Press Ctrl+C to disconnect)\n", selectedDev.Selector(), member)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("USB redirection transport error: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&deviceID, "device", "d", "", "USB device selector (vendorId:productId or bus.dev)")
	c.Flags().StringVarP(&user, "user", "u", "", "Expected assignment owner for authorization check")
	c.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Skip confirmation prompt")
	return c
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{
		Name:        "vdi",
		Version:     "0.2.0",
		Description: "Desktop pools and native USB redirection",
		Capabilities: []string{
			"cli-command",
			"vdi-pools",
			"usb-redirection",
		},
		Permissions: []string{
			"execute kubectl and virtctl",
			"enumerate local USB devices",
			"redirect local host USB devices to guest VMs",
			"mutate KubeVirt VMs and labels",
		},
		SupportedBackends: []string{"kubevirt"},
	}) {
		return
	}
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

