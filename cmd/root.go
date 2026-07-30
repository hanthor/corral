package cmd

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/demo"
	"github.com/tuna-os/corral/pkg/plugin"
	"github.com/tuna-os/corral/pkg/registry"
)

var registryStore *registry.Store
var verbose bool
var rootDemo bool
var rootContext string
var rootBackend string

var rootCmd = &cobra.Command{
	Use:   "corral",
	Short: "Corral — herd your VMs into your tailnet",
	Long: `Corral manages one fleet across local QEMU, KubeVirt clusters,
Incus remotes, libvirt URIs, and federated Corral peers. Tailscale is a
first-class option for VNC, SSH, RDP, and custom ports, not a requirement.

Run without arguments to launch the interactive TUI.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		backend := rootBackend
		if backend == "" {
			backend = config.DefaultBackend()
		}
		context := rootContext
		if target, ok := config.FindContext(rootContext); ok {
			backend, context = target.Backend, target.Context
		}
		if rootContext != "" {
			if backend == "incus" {
				os.Setenv("CORRAL_INCUS_REMOTE", context)
			} else if backend == "libvirt" {
				os.Setenv("CORRAL_LIBVIRT_URI", context)
			} else {
				os.Setenv("CORRAL_KUBE_CONTEXT", context)
			}
		} else if context := config.KubeContext(); context != "" {
			os.Setenv("CORRAL_KUBE_CONTEXT", context)
		}
		if rootDemo {
			demo.Enable()
		}
		s, err := registry.NewStore()
		if err != nil {
			return fmt.Errorf("init registry: %w", err)
		}
		registryStore = s
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		// The alt screen is declared on the View in v2, not passed here.
		p := tea.NewProgram(newTUIModel())
		if _, err := p.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if postQuitAction != nil {
			postQuitAction()
		}
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&rootDemo, "demo", false,
		"Run against a built-in fake cluster (no kubectl/cluster needed) — explore the TUI, CLI, or web UI safely")
	rootCmd.PersistentFlags().StringVar(&rootContext, "context", "", "One-shot backend context (Incus remote or kubeconfig context)")
	rootCmd.PersistentFlags().StringVarP(&rootBackend, "backend", "b", "", "One-shot backend (qemu, kubevirt, incus, or libvirt)")

	// A runtime failure ("VM not found", cluster unreachable) is not a syntax
	// error — dumping the whole usage block after it buries the message.
	// Bad flags still get usage via the flag-error hook below.
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true // Execute() prints the error once itself
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		c.Println(c.UsageString())
		return err
	})
}

func Execute() {
	// kubectl-style plugin dispatch: if the first arg isn't a built-in command
	// or flag but a `corral-<arg>` plugin exists, hand off to it.
	if len(os.Args) > 1 {
		first := os.Args[1]
		if first != "" && first[0] != '-' {
			if c, _, err := rootCmd.Find(os.Args[1:]); (err != nil || c == rootCmd) && plugin.IsInstalled(first) {
				if err := plugin.Dispatch(first, os.Args[2:]); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				return
			}
		}
	}
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
