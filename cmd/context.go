package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
)

var (
	contextBackend string
	contextValue   string
)

var contextCmd = &cobra.Command{
	Use:     "context",
	Aliases: []string{"ctx"},
	Short:   "Manage the local, Kubernetes, Incus, and libvirt contexts aggregated by Corral",
}

var contextGetCmd = &cobra.Command{Use: "get", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
	c := config.DefaultContext()
	fmt.Printf("%s\t%s\t%s\n", c.Name, c.Backend, c.Context)
}}

var contextUseCmd = &cobra.Command{
	Use:     "use <name>",
	Aliases: []string{"set", "switch"},
	Short:   "Select the default destination without hiding other contexts",
	Args:    cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return config.SetDefaultContext(args[0])
	},
}

var contextAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add or update an inventory context",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return config.AddContext(config.ContextConfig{Name: args[0], Backend: contextBackend, Context: contextValue})
	},
}

var contextRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"delete", "rm"},
	Args:    cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return config.RemoveContext(args[0])
	},
}

var contextListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
	current := config.DefaultContext().Name
	for _, c := range config.Contexts() {
		mark := " "
		if c.Name == current {
			mark = "*"
		}
		fmt.Printf("%s %-18s %-10s %s\n", mark, c.Name, c.Backend, c.Context)
	}
}}

func init() {
	contextAddCmd.Flags().StringVarP(&contextBackend, "backend", "b", "", "Backend: qemu, kubevirt, incus, or libvirt")
	contextAddCmd.Flags().StringVar(&contextValue, "context", "", "kubeconfig context, Incus remote, or libvirt URI")
	_ = contextAddCmd.MarkFlagRequired("backend")
	contextCmd.AddCommand(contextGetCmd, contextUseCmd, contextAddCmd, contextRemoveCmd, contextListCmd)
	rootCmd.AddCommand(contextCmd)
}
