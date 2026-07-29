// corral-snapsched is the scheduled-snapshots Corral plugin: a CronJob in the
// VM's namespace creates a VirtualMachineSnapshot per tick and prunes the
// oldest auto-snapshots beyond --keep. Installed via the marketplace
// (`corral plugin install snapsched`) and invoked as `corral snapsched`.
//
// Requires a snapshot-capable StorageClass (`corral doctor` checks for a
// VolumeSnapshotClass).
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/cronops"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/shell"
)

const scheduleLabel = "corral.dev/snapsched"

func cronJobName(vm string) string { return "corral-snap-" + vm }

func addSchedule(vm, ns, cron string, keep int) error {
	for _, obj := range []map[string]any{
		cronops.ServiceAccount(ns),
		cronops.Role(ns),
		cronops.RoleBinding(ns),
		cronops.CronJob(cronJobName(vm), ns, cron,
			cronops.SnapshotScript(vm, ns, keep),
			map[string]string{scheduleLabel: vm}),
	} {
		if err := kubevirt.Apply(obj); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "snapsched", Version: "0.1.0", Description: "Scheduled KubeVirt snapshots", Capabilities: []string{"cli-command", "scheduler", "snapshots"}, Permissions: []string{"create Kubernetes CronJobs and snapshots"}, SupportedBackends: []string{"kubevirt"}}) {
		return
	}
	var (
		namespace string
		every     string
		keep      int
		runner    shell.Runner = shell.Real{}
	)

	add := &cobra.Command{
		Use:   "add <vm>",
		Short: "Schedule periodic snapshots for a VM (with retention pruning)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vm := args[0]
			cron, err := cronExpr(every)
			if err != nil {
				return err
			}
			if keep < 1 {
				return fmt.Errorf("--keep must be >= 1")
			}
			if err := addSchedule(vm, namespace, cron, keep); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "snapshot schedule for %s: %q, keeping %d (CronJob %s/%s)\n",
				vm, cron, keep, namespace, cronJobName(vm))
			return nil
		},
	}
	add.Flags().StringVar(&every, "every", "24h", "Interval (30m, 1h, 6h, 12h, 24h) or a raw cron expression via --cron syntax \"m h dom mon dow\"")
	add.Flags().IntVar(&keep, "keep", 7, "Auto-snapshots to retain per VM")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List snapshot schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := runner.Run("kubectl", "get", "cronjobs", "-n", namespace,
				"-l", scheduleLabel, "-o",
				"custom-columns=VM:.metadata.labels.corral\\.dev/snapsched,SCHEDULE:.spec.schedule,SUSPENDED:.spec.suspend,LAST:.status.lastScheduleTime")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			fmt.Print(string(out))
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm <vm>",
		Short: "Remove a VM's snapshot schedule (auto-snapshots are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := runner.Run("kubectl", "delete", "cronjob", cronJobName(args[0]),
				"-n", namespace, "--ignore-not-found")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		},
	}

	root := &cobra.Command{
		Use:   "corral-snapsched",
		Short: "Corral plugin — scheduled VM snapshots with retention",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace")
	root.AddCommand(add, ls, rm)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// cronExpr accepts either a supported interval shorthand or a 5-field cron
// expression and returns the cron expression.
func cronExpr(every string) (string, error) {
	switch every {
	case "30m":
		return "*/30 * * * *", nil
	case "1h":
		return "0 * * * *", nil
	case "6h":
		return "0 */6 * * *", nil
	case "12h":
		return "0 */12 * * *", nil
	case "24h":
		return "0 3 * * *", nil // daily at 03:00
	}
	if len(strings.Fields(every)) == 5 {
		return every, nil
	}
	return "", fmt.Errorf("--every must be one of 30m/1h/6h/12h/24h or a 5-field cron expression, got %q", every)
}
