// corral-schedule is the autostart/shutdown-windows Corral plugin: dev VMs
// only during business hours, and so on. Installed via the marketplace
// (`corral plugin install schedule`) and invoked as `corral schedule`.
//
// Two execution models, because the honest answer differs by backend (#133):
//
//   - KubeVirt with --in-cluster: one Kubernetes CronJob per boundary, flipping
//     the VM's runStrategy. The cluster fires it, so it works with the
//     workstation closed. This is the original behaviour and stays the right
//     choice for a cluster VM.
//   - Everything else: the schedule is stored locally against the instance's
//     canonical reference, and `corral schedule tick` — driven by a systemd
//     timer — evaluates what is due and dispatches through the backend
//     adapter. A laptop VM, an Incus remote, and a libvirt host have no
//     CronJob controller to do it for them.
//
// The portability limit is inherent and worth stating plainly: a locally
// scheduled boundary only fires while this machine is awake and the timer is
// running. A VM that must start at 09:00 whether or not anyone is at their
// desk belongs on the cluster path.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/cronops"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/schedule"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
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

// resolveRef turns the command line into a canonical instance reference. A
// bare name is only meaningful once a context is chosen, so the selected
// Corral context supplies the backend — the same rule the rest of the CLI
// follows.
func resolveRef(name, contextName, namespace string) (types.InstanceRef, error) {
	target := config.DefaultContext()
	if contextName != "" {
		found, ok := config.FindContext(contextName)
		if !ok {
			return types.InstanceRef{}, fmt.Errorf("unknown context %q (see corral context ls)", contextName)
		}
		target = found
	}
	ref := types.InstanceRef{Backend: target.Backend, Context: target.Context, Name: name}
	if target.Backend == "kubevirt" {
		ref.Namespace = namespace
	}
	return ref, ref.Validate()
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{
		Name:        "schedule",
		Version:     "0.2.0",
		Description: "Instance start and stop schedules",
		// The plugin now drives any backend Corral can power on and off; the
		// per-instance check happens at `add` time against that backend's
		// capabilities.
		Capabilities:      []string{"cli-command", "scheduler"},
		Permissions:       []string{"create Kubernetes CronJobs", "mutate instance lifecycle", "write ~/.local/share/corral/schedules.json"},
		SupportedBackends: []string{"kubevirt", "libvirt", "incus", "qemu"},
	}) {
		return
	}
	var (
		namespace       string
		contextName     string
		startAt, stopAt string
		inCluster       bool
		runner          shell.Runner = shell.Real{}
	)
	store := schedule.NewStore()

	add := &cobra.Command{
		Use:   "add <instance> --start <cron> --stop <cron>",
		Short: "Add autostart/shutdown windows for an instance",
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
			if inCluster {
				if err := addWindows(args[0], namespace, startAt, stopAt); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "in-cluster schedule for %s: start=%q stop=%q\n", args[0], startAt, stopAt)
				return nil
			}
			ref, err := resolveRef(args[0], contextName, namespace)
			if err != nil {
				return err
			}
			// Store.Put validates: a schedule the backend could never honour
			// fails here, not silently at 3am on the first tick.
			if err := store.Put(schedule.Entry{Ref: ref, Start: startAt, Stop: stopAt}); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "schedule for %s: start=%q stop=%q\n", ref.String(), startAt, stopAt)
			fmt.Fprintln(os.Stderr, "drive it from a timer — see `corral schedule tick --help`")
			return nil
		},
	}
	add.Flags().StringVar(&startAt, "start", "", "Cron expression to start the instance (e.g. \"0 9 * * 1-5\")")
	add.Flags().StringVar(&stopAt, "stop", "", "Cron expression to stop the instance (e.g. \"0 18 * * 1-5\")")
	add.Flags().BoolVar(&inCluster, "in-cluster", false, "Store the schedule as Kubernetes CronJobs so the cluster fires it (KubeVirt only)")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List instance start/stop schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			if inCluster {
				out, err := runner.Run("kubectl", "get", "cronjobs", "-n", namespace,
					"-l", scheduleLabel, "-o",
					"custom-columns=NAME:.metadata.name,VM:.metadata.labels.corral\\.dev/schedule,SCHEDULE:.spec.schedule,LAST:.status.lastScheduleTime")
				if err != nil {
					return fmt.Errorf("%s", strings.TrimSpace(string(out)))
				}
				fmt.Print(string(out))
				return nil
			}
			entries, err := store.List()
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Fprintln(os.Stderr, "No local schedules. Cluster-side schedules: corral schedule ls --in-cluster")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "INSTANCE\tBACKEND\tCONTEXT\tSTART\tSTOP")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.Ref.Name, e.Ref.Backend, orDash(e.Ref.Context), orDash(e.Start), orDash(e.Stop))
			}
			return w.Flush()
		},
	}

	rm := &cobra.Command{
		Use:   "rm <instance>",
		Short: "Remove an instance's start/stop schedules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if inCluster {
				out, err := runner.Run("kubectl", "delete", "cronjob",
					startJobName(args[0]), stopJobName(args[0]),
					"-n", namespace, "--ignore-not-found")
				if err != nil {
					return fmt.Errorf("%s", strings.TrimSpace(string(out)))
				}
				return nil
			}
			ref, err := resolveRef(args[0], contextName, namespace)
			if err != nil {
				return err
			}
			return store.Remove(ref)
		},
	}

	tick := &cobra.Command{
		Use:   "tick",
		Short: "Run every locally-stored schedule that is due now",
		Long: `Evaluate the stored schedules and start or stop whatever is due
this minute. Intended to be driven once a minute by a systemd user timer:

  # ~/.config/systemd/user/corral-schedule.service
  [Service]
  Type=oneshot
  ExecStart=%h/.local/bin/corral schedule tick

  # ~/.config/systemd/user/corral-schedule.timer
  [Timer]
  OnCalendar=minutely
  [Install]
  WantedBy=timers.target

  systemctl --user enable --now corral-schedule.timer
  loginctl enable-linger $USER   # so it keeps running while logged out

A context that cannot be reached is reported and retried on the next tick; it
never removes the schedule. Exits non-zero if any due action failed, so the
timer's status reflects reality.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := store.List()
			if err != nil {
				return err
			}
			results := schedule.Tick(entries, time.Now())
			for _, r := range results {
				fmt.Fprintln(os.Stderr, r.String())
			}
			if failed := schedule.Failed(results); len(failed) > 0 {
				return fmt.Errorf("%d scheduled action(s) failed", len(failed))
			}
			return nil
		},
	}

	root := &cobra.Command{
		Use:   "corral-schedule",
		Short: "Corral plugin — instance autostart/shutdown windows (cron)",
		// A tick that reports an unreachable context is a runtime outcome, not
		// a usage mistake: printing the flag reference after it buries the one
		// line that matters. main already prints the error, so cobra must not
		// print it a second time.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace (KubeVirt only)")
	root.PersistentFlags().StringVar(&contextName, "context", "", "Corral context the instance lives in (default: the selected context)")
	ls.Flags().BoolVar(&inCluster, "in-cluster", false, "List Kubernetes CronJob schedules instead of local ones")
	rm.Flags().BoolVar(&inCluster, "in-cluster", false, "Remove Kubernetes CronJob schedules instead of local ones")
	root.AddCommand(add, ls, rm, tick)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
