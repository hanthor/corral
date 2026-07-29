// corral-schedule is the autostart/shutdown-windows Corral plugin: it creates
// one CronJob per boundary that flips the VM's runStrategy (Always/Halted) on
// a cron schedule — e.g. dev VMs only during business hours. Installed via the
// marketplace (`corral plugin install schedule`) and invoked as
// `corral schedule`.
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

const scheduleLabel = "corral.dev/schedule"

func startJobName(vm string) string { return "corral-start-" + vm }
func stopJobName(vm string) string  { return "corral-stop-" + vm }

func addWindows(vm, ns, startCron, stopCron string) error {
	objs := []map[string]any{
		cronops.ServiceAccount(ns),
		cronops.Role(ns),
		cronops.RoleBinding(ns),
	}
	if startCron != "" {
		objs = append(objs, cronops.CronJob(startJobName(vm), ns, startCron,
			cronops.PowerScript(vm, ns, true),
			map[string]string{scheduleLabel: vm}))
	}
	if stopCron != "" {
		objs = append(objs, cronops.CronJob(stopJobName(vm), ns, stopCron,
			cronops.PowerScript(vm, ns, false),
			map[string]string{scheduleLabel: vm}))
	}
	for _, obj := range objs {
		if err := kubevirt.Apply(obj); err != nil {
			return err
		}
	}
	return nil
}

func validCron(expr string) error {
	if len(strings.Fields(expr)) != 5 {
		return fmt.Errorf("%q is not a 5-field cron expression (e.g. \"0 9 * * 1-5\")", expr)
	}
	return nil
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "schedule", Version: "0.1.0", Description: "VM start and stop schedules", Capabilities: []string{"cli-command", "scheduler"}, Permissions: []string{"create Kubernetes CronJobs", "mutate VM lifecycle"}, SupportedBackends: []string{"kubevirt"}}) {
		return
	}
	var (
		namespace       string
		startAt, stopAt string
		runner          shell.Runner = shell.Real{}
	)

	add := &cobra.Command{
		Use:   "add <vm> --start <cron> --stop <cron>",
		Short: "Add autostart/shutdown windows for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if startAt == "" && stopAt == "" {
				return fmt.Errorf("at least one of --start / --stop is required")
			}
			for _, expr := range []string{startAt, stopAt} {
				if expr != "" {
					if err := validCron(expr); err != nil {
						return err
					}
				}
			}
			if err := addWindows(args[0], namespace, startAt, stopAt); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "schedule for %s: start=%q stop=%q\n", args[0], startAt, stopAt)
			return nil
		},
	}
	add.Flags().StringVar(&startAt, "start", "", "Cron expression to start the VM (e.g. \"0 9 * * 1-5\")")
	add.Flags().StringVar(&stopAt, "stop", "", "Cron expression to stop the VM (e.g. \"0 18 * * 1-5\")")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List VM start/stop schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := runner.Run("kubectl", "get", "cronjobs", "-n", namespace,
				"-l", scheduleLabel, "-o",
				"custom-columns=NAME:.metadata.name,VM:.metadata.labels.corral\\.dev/schedule,SCHEDULE:.spec.schedule,LAST:.status.lastScheduleTime")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			fmt.Print(string(out))
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm <vm>",
		Short: "Remove a VM's start/stop schedules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := runner.Run("kubectl", "delete", "cronjob",
				startJobName(args[0]), stopJobName(args[0]),
				"-n", namespace, "--ignore-not-found")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		},
	}

	root := &cobra.Command{
		Use:   "corral-schedule",
		Short: "Corral plugin — VM autostart/shutdown windows (cron)",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace")
	root.AddCommand(add, ls, rm)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
