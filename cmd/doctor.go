package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/doctor"
)

var doctorFix bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the cluster setup Corral's features need (and reconcile fixes)",
	Long: `Check whether the cluster has the pieces Corral's features need —
KubeVirt, CDI, the right feature gates + LiveUpdate, expandable/snapshot
storage, the export proxy, metrics — and report what's missing or
misconfigured. --fix reconciles the safe, config-only issues.`,
	Example: `  corral doctor
  corral doctor --fix`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if doctorFix {
			fixed, err := doctor.Fix()
			if err != nil {
				return err
			}
			if len(fixed) == 0 {
				fmt.Println("Nothing to reconcile.")
			} else {
				fmt.Printf("Reconciled: %v\n\n", fixed)
			}
		}

		contexts := config.Contexts()
		if rootContext != "" {
			if selected, ok := config.FindContext(rootContext); ok {
				contexts = []config.ContextConfig{selected}
			} else {
				contexts = []config.ContextConfig{{Name: rootContext, Backend: rootBackend, Context: rootContext}}
			}
		}
		checks := doctor.RunContexts(contexts)
		// Peers are part of the fleet the dashboard aggregates, so a fleet-wide
		// diagnosis has to cover them. --context selects a compute context, and
		// naming one means the caller is not asking about peers.
		if rootContext == "" {
			checks = append(checks, doctor.RunPeers(config.Peers())...)
		}
		anyFixable := false
		failedRequired := false
		lastTarget := ""
		for _, c := range checks {
			target := c.Context + " [" + c.Backend + "]"
			if target != lastTarget {
				fmt.Printf("\n%s\n", target)
				lastTarget = target
			}
			mark := "\033[32m✓\033[0m"
			if !c.OK {
				if c.Severity == "warning" {
					mark = "\033[33m!\033[0m"
				} else {
					mark = "\033[31m✗\033[0m"
					failedRequired = true
				}
			}
			suffix := ""
			if !c.OK && c.Fixable {
				suffix = " \033[33m(fixable)\033[0m"
				anyFixable = true
			}
			fmt.Printf("  %s %-30s %s%s\n", mark, c.Name, c.Detail, suffix)
		}
		if anyFixable && !doctorFix {
			fmt.Println("\nReconcile the fixable items: corral doctor --fix")
		}
		if failedRequired {
			return fmt.Errorf("one or more required backend checks failed%s", selectedDoctorHint())
		}
		return nil
	},
}

func selectedDoctorHint() string {
	if rootContext == "" {
		return " (use --context NAME to inspect one target)"
	}
	return " for " + strings.TrimSpace(rootContext)
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "Reconcile fixable (config-only) issues")
}
