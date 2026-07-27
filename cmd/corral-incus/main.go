// corral-incus is the Incus backend Corral plugin: it allows managing
// local Incus containers and virtual machines via Corral.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/incus"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "corral-incus",
		Short: "Corral plugin — manage Incus containers and virtual machines",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List Incus instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			vms, err := incus.List()
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(vms, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}

	var createOpts incus.CreateOpts
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new Incus instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			createOpts.Name = args[0]
			return incus.Create(createOpts)
		},
	}
	createCmd.Flags().StringVar(&createOpts.Image, "image", "images:ubuntu/22.04", "Image to launch")
	createCmd.Flags().BoolVar(&createOpts.VM, "vm", false, "Create virtual machine instead of container")
	createCmd.Flags().IntVar(&createOpts.CPU, "cpu", 0, "CPU limit (cores)")
	createCmd.Flags().StringVar(&createOpts.Memory, "mem", "", "Memory limit (e.g. 2Gi, 512Mi)")

	startCmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start an Incus instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return incus.Start(args[0])
		},
	}

	stopCmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop an Incus instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return incus.Stop(args[0])
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an Incus instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return incus.Delete(args[0])
		},
	}

	infoCmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Get info for an Incus instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := incus.Info(args[0])
			if err != nil {
				return err
			}
			fmt.Println(string(info))
			return nil
		},
	}

	rootCmd.AddCommand(listCmd, createCmd, startCmd, stopCmd, deleteCmd, infoCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
