package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/move"
)

var (
	moveTo           string
	moveContext      string
	moveNamespace    string
	moveName         string
	moveScratch      string
	moveSSHKey       string
	moveDryRun       bool
	moveDeleteSource bool
	moveYes          bool
)

var moveCmd = &cobra.Command{
	Use:   "move <name> --to <backend>",
	Short: "Move a VM to a different backend (cold — the guest stops)",
	Long: `Move an instance from one backend to another: export its disk, create it
on the destination, and leave the source stopped.

This is not a live migration and cannot be one — there is no shared state
between, say, a KubeVirt VMI and a systemd-managed QEMU process. The guest is
down for the whole move and comes up with a new MAC and almost certainly a new
IP. To move a guest between nodes of one backend, use ` + "`corral migrate`" + `, which
stays live where the backend supports it.

The source is stopped, never deleted, unless --delete-source is passed.`,
	Example: `  corral move web-1 --to qemu --dry-run    # show the plan and the warnings
  corral move web-1 --to libvirt
  corral move web-1 --to qemu --name web-1-local --delete-source`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(moveTo) == "" {
			return fmt.Errorf("--to is required: name the backend to move onto")
		}
		vm, err := resolveVM(args[0])
		if err != nil {
			return err
		}

		src := move.Source{VM: vm}
		plan := move.Preflight(src, move.Target{
			Backend:      moveTo,
			Context:      moveContext,
			Namespace:    moveNamespace,
			Name:         moveName,
			Scratch:      moveScratch,
			SSHKey:       moveSSHKey,
			DeleteSource: moveDeleteSource,
		})

		printPlan(plan)

		if !plan.OK() {
			return fmt.Errorf("this move was refused; nothing was changed")
		}
		if moveDryRun {
			return nil
		}
		if !moveYes && !confirmMove(plan) {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}

		result, err := move.Execute(cmd.Context(), plan, func(p move.Progress) {
			if p.Total > 0 {
				fmt.Printf("\r%-20s %3d%%", p.Stage, p.Done*100/p.Total)
			} else {
				fmt.Printf("\r%-20s", p.Stage)
			}
		})
		fmt.Println()
		if err != nil {
			return err
		}

		fmt.Printf("Moved %s → %s/%s\n", plan.Source.Name, result.Destination.Backend, result.Destination.Name)
		if result.SourceDeleted {
			fmt.Printf("The source on %s was deleted.\n", plan.Source.Backend)
		} else if result.SourceStopped {
			fmt.Printf("The source on %s is stopped and still there.\n", plan.Source.Backend)
		}
		fmt.Printf("The instance was created stopped; start it with `corral start %s`.\n", result.Destination.Name)
		for _, w := range result.Warnings {
			fmt.Printf("  ! %s\n", w)
		}
		return nil
	},
}

// printPlan renders the whole decision — steps, warnings, dropped config, and
// refusals — because the preflight's value is that an operator reads it before
// a guest goes down, not after.
func printPlan(plan move.Plan) {
	fmt.Printf("Move %s/%s → %s/%s\n\n",
		plan.Source.Backend, plan.Source.Name, plan.Destination.Backend, plan.Destination.Name)

	for i, step := range plan.Steps {
		fmt.Printf("  %d. %-14s %s\n", i+1, step.Name, step.Detail)
	}

	if len(plan.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range plan.Warnings {
			fmt.Printf("  ! %s\n", w)
		}
	}
	if len(plan.Dropped) > 0 {
		fmt.Println("\nNot carried over:")
		for _, d := range plan.Dropped {
			fmt.Printf("  - %s\n", d)
		}
	}
	if len(plan.Refusals) > 0 {
		fmt.Println("\nRefused:")
		for _, r := range plan.Refusals {
			fmt.Printf("  x %s\n", r.Reason)
			if r.Remedy != "" {
				fmt.Printf("      %s\n", r.Remedy)
			}
		}
	}
	fmt.Println()
}

// confirmMove asks before a guest goes down. Skipped by --yes, and skipped
// entirely when stdin is not a terminal so scripted use does not hang.
func confirmMove(plan move.Plan) bool {
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
		return true
	}
	verb := "This will create the instance on the destination"
	if plan.StopFirst {
		verb = "This will stop " + plan.Source.Name + " and move it"
	}
	fmt.Printf("%s. Continue? [y/N] ", verb)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func init() {
	moveCmd.Flags().StringVar(&moveTo, "to", "", "destination backend (qemu, libvirt, kubevirt, proxmox)")
	moveCmd.Flags().StringVar(&moveContext, "to-context", "", "destination context (Incus remote, kube context, libvirt URI)")
	moveCmd.Flags().StringVar(&moveNamespace, "to-namespace", "", "destination namespace")
	moveCmd.Flags().StringVar(&moveName, "name", "", "name on the destination (default: the source's name)")
	moveCmd.Flags().StringVar(&moveScratch, "scratch", "", "directory for the exported disk (default: the system temp dir)")
	moveCmd.Flags().StringVar(&moveSSHKey, "ssh-key", "", "SSH public key to inject where the destination supports it")
	moveCmd.Flags().BoolVar(&moveDryRun, "dry-run", false, "print the plan and the warnings, change nothing")
	moveCmd.Flags().BoolVar(&moveDeleteSource, "delete-source", false, "delete the source after a successful move")
	moveCmd.Flags().BoolVarP(&moveYes, "yes", "y", false, "do not prompt before stopping the guest")
	rootCmd.AddCommand(moveCmd)
}
