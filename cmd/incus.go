package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/incus"
)

var incusRemote string
var incusCmd = &cobra.Command{Use: "incus", Short: "Manage Incus instances and Corral's Incus remote"}

func incusClient() incus.Client { return incus.NewClient(incusRemote) }

var incusListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
	vms, err := incusClient().List()
	if err != nil {
		return err
	}
	printVMList(vms)
	return nil
}}
var incusCreateCmd = &cobra.Command{Use: "create <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	image, _ := cmd.Flags().GetString("image")
	vm, _ := cmd.Flags().GetBool("vm")
	cpu, _ := cmd.Flags().GetInt("cpu")
	mem, _ := cmd.Flags().GetString("mem")
	return incusClient().Create(incus.CreateOpts{Name: args[0], Image: image, VM: vm, CPU: cpu, Memory: mem})
}}

func init() {
	incusCmd.PersistentFlags().StringVar(&incusRemote, "remote", "", "One-shot Incus remote")
	incusCreateCmd.Flags().String("image", "images:ubuntu/24.04", "Incus image")
	incusCreateCmd.Flags().Bool("vm", false, "Create a virtual machine instead of a container")
	incusCreateCmd.Flags().Int("cpu", 0, "CPU limit")
	incusCreateCmd.Flags().String("mem", "", "Memory limit")
	nameCmd := func(use string, fn func(incus.Client, string) error) *cobra.Command {
		return &cobra.Command{Use: use + " <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return fn(incusClient(), args[0]) }}
	}
	incusCmd.AddCommand(incusListCmd, incusCreateCmd,
		nameCmd("start", func(c incus.Client, n string) error { return c.Start(n) }),
		nameCmd("stop", func(c incus.Client, n string) error { return c.Stop(n) }),
		nameCmd("delete", func(c incus.Client, n string) error { return c.Delete(n) }),
		nameCmd("ssh", func(c incus.Client, n string) error { return c.SSH(n, "") }),
		&cobra.Command{Use: "info <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
			b, err := incusClient().Info(args[0])
			if err == nil {
				fmt.Println(string(b))
			}
			return err
		}},
		&cobra.Command{Use: "get-remote", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) { fmt.Println(config.IncusRemote()) }},
		&cobra.Command{Use: "set-remote <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return config.SetIncusRemote(args[0]) }},
		&cobra.Command{Use: "list-remotes", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
			rs, err := incus.ListRemotes()
			if err != nil {
				return err
			}
			for _, r := range rs {
				fmt.Printf("%s\t%s\n", r.Name, r.Address)
			}
			return nil
		}},
	)
	rootCmd.AddCommand(incusCmd)
}
