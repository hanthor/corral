// corral-backup is the S3/R2 backup Corral plugin: on-demand VM disk backups to
// any rclone remote and restore back into a fresh disk. It reuses KubeVirt's
// exporter to pull the disk, rclone to move it to object storage (the tuna-os
// convention for R2/S3), and `virtctl image-upload` to restore via CDI.
// Installed via the marketplace (`corral plugin install backup`) and invoked as
// `corral backup …`.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/cronops"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/shell"
)

// runner shells out to rclone/virtctl; swapped in tests.
var runner shell.Runner = shell.Real{}

var (
	flagNamespace string
	flagDest      string
	flagSrc       string
	flagSize      string
	flagKeepLocal bool
)

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "backup", Version: "0.2.0", Description: "KubeVirt disk backup and restore", Capabilities: []string{"cli-command", "backup"}, Permissions: []string{"execute kubectl and rclone", "read VM disks", "write object storage"}, SupportedBackends: []string{"kubevirt"}}) {
		return
	}
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "backup",
		Short: "Back up and restore VM disks to S3/R2 (via rclone)",
		Long: "Back up KubeVirt VM disks to any rclone remote and restore them.\n\n" +
			"rclone must already be configured with your remote (e.g. an R2/S3\n" +
			"bucket): see `rclone config`. The remote is referenced as\n" +
			"`<remote>:<bucket>/<path>`.",
	}
	root.PersistentFlags().StringVarP(&flagNamespace, "namespace", "n", "", "VM namespace (default: corral's default)")
	root.AddCommand(createCmd(), restoreCmd(), listCmd(), scheduleCmd(), unscheduleCmd(), schedulesCmd())
	return root
}

func ns() string {
	if flagNamespace != "" {
		return flagNamespace
	}
	return kubevirt.DefaultNamespace
}

func createCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create <vm>",
		Short: "Export a stopped VM's disk and upload it to the remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runBackup(args[0])
		},
	}
	c.Flags().StringVar(&flagDest, "dest", "", "rclone destination dir, e.g. r2:backups/corral (required)")
	c.Flags().BoolVar(&flagKeepLocal, "keep-local", false, "keep the local export file after upload")
	c.MarkFlagRequired("dest")
	return c
}

func restoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore <new-disk-name>",
		Short: "Download a backup from the remote and upload it into a new PVC (via CDI)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRestore(args[0])
		},
	}
	c.Flags().StringVar(&flagSrc, "src", "", "rclone source file, e.g. r2:backups/corral/web-2026….img.gz (required)")
	c.Flags().StringVar(&flagSize, "size", "", "size of the restored disk, e.g. 20Gi (required)")
	c.MarkFlagRequired("src")
	c.MarkFlagRequired("size")
	return c
}

func listCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List backups under a remote dir",
		RunE: func(_ *cobra.Command, _ []string) error {
			out, err := runner.Run("rclone", "lsf", flagDest)
			if err != nil {
				return fmt.Errorf("rclone lsf %s: %w", flagDest, err)
			}
			fmt.Print(string(out))
			return nil
		},
	}
	c.Flags().StringVar(&flagDest, "dest", "", "rclone dir to list, e.g. r2:backups/corral (required)")
	c.MarkFlagRequired("dest")
	return c
}

// runBackup exports the VM disk locally then rclone-copies it to the remote.
func runBackup(vm string) error {
	if err := ensureRclone(); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "corral-backup-")
	if err != nil {
		return err
	}
	if !flagKeepLocal {
		defer os.RemoveAll(tmp)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	fname := fmt.Sprintf("%s-%s.img.gz", vm, stamp)
	local := filepath.Join(tmp, fname)

	fmt.Fprintf(os.Stderr, "exporting %s disk → %s …\n", vm, local)
	if _, err := kubevirt.NewClient(ns()).Export(vm, "", local); err != nil {
		return fmt.Errorf("export %s (is it stopped?): %w", vm, err)
	}

	dest := strings.TrimSuffix(flagDest, "/") + "/" + fname
	fmt.Fprintf(os.Stderr, "uploading → %s …\n", dest)
	if out, err := runner.Run("rclone", "copyto", local, dest); err != nil {
		return fmt.Errorf("rclone copyto: %s", strings.TrimSpace(string(out)))
	}
	fmt.Printf("backed up %s → %s\n", vm, dest)
	if flagKeepLocal {
		fmt.Printf("local copy kept at %s\n", local)
	}
	return nil
}

// runRestore downloads a backup and uploads it into a new CDI-backed PVC.
func runRestore(diskName string) error {
	if err := ensureRclone(); err != nil {
		return err
	}
	virtctl, err := kubevirt.NewClient(ns()).Virtctl()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "corral-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	local := filepath.Join(tmp, filepath.Base(flagSrc))

	fmt.Fprintf(os.Stderr, "downloading %s → %s …\n", flagSrc, local)
	if out, err := runner.Run("rclone", "copyto", flagSrc, local); err != nil {
		return fmt.Errorf("rclone copyto: %s", strings.TrimSpace(string(out)))
	}

	fmt.Fprintf(os.Stderr, "uploading into PVC %s (%s) via CDI …\n", diskName, flagSize)
	// CDI's upload proxy decompresses gzip and detects the image format.
	out, err := runner.Run(virtctl, "image-upload", "dv", diskName,
		"--size", flagSize, "--image-path", local, "--namespace", ns(), "--insecure")
	if err != nil {
		return fmt.Errorf("virtctl image-upload: %s", strings.TrimSpace(string(out)))
	}
	fmt.Printf("restored %s → DataVolume %s/%s\n", flagSrc, ns(), diskName)
	fmt.Printf("attach it to a VM with: corral create <name> --pvc %s\n", diskName)
	return nil
}

func ensureRclone() error {
	if _, err := runner.Run("rclone", "version"); err != nil {
		return fmt.Errorf("rclone not found or not configured — install it and run `rclone config` for your S3/R2 remote")
	}
	return nil
}

// ── Scheduled backups (CronJobs) ───────────────────────────────────

const rcloneMountPath = "/root/.config/rclone"

func cronJobName(vm string) string { return "corral-backup-" + vm }
func secretName(vm string) string  { return "corral-backup-rclone-" + vm }
func scheduleLabelKey() string     { return "corral.dev/backupsched" }

// rcloneConfigPath resolves the local rclone config file the same way
// rclone itself does: $RCLONE_CONFIG if set, else ~/.config/rclone/rclone.conf.
func rcloneConfigPath() (string, error) {
	if p := os.Getenv("RCLONE_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "rclone", "rclone.conf"), nil
}

// applyRcloneSecret mirrors the caller's local rclone config (which holds
// the remote's credentials) into a namespaced Secret the CronJob pod mounts
// — the CronJob runs in-cluster with no access to the machine that ran
// `schedule`, so the credentials have to travel with it.
func applyRcloneSecret(name, ns string) error {
	path, err := rcloneConfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading local rclone config (%s) — configure your remote first with `rclone config`: %w", path, err)
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       map[string]any{"rclone.conf": base64.StdEncoding.EncodeToString(data)},
	}
	return kubevirt.Apply(secret)
}

func addSchedule(vm, ns, cron, dest string, keep int) error {
	sec := secretName(vm)
	if err := applyRcloneSecret(sec, ns); err != nil {
		return err
	}
	for _, obj := range []map[string]any{
		cronops.ServiceAccount(ns),
		cronops.Role(ns),
		cronops.RoleBinding(ns),
		cronops.CronJobWithSecret(cronJobName(vm), ns, cron,
			cronops.BackupScript(vm, ns, dest, keep),
			map[string]string{scheduleLabelKey(): vm}, sec, rcloneMountPath),
	} {
		if err := kubevirt.Apply(obj); err != nil {
			return err
		}
	}
	return nil
}

func scheduleCmd() *cobra.Command {
	var every, dest string
	var keep int
	c := &cobra.Command{
		Use:   "schedule <vm>",
		Short: "Schedule periodic backups for a VM (with retention pruning) as an in-cluster CronJob",
		Long: "Creates a CronJob that runs entirely in-cluster (no workstation needs to be\n" +
			"online) — it fetches virtctl + rclone at runtime, exports the VM's disk,\n" +
			"uploads it to the remote, and prunes backups for this VM beyond --keep.\n\n" +
			"Your local rclone config (with the remote's credentials) is copied into a\n" +
			"namespaced Secret the CronJob mounts — run `rclone config` locally first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vm := args[0]
			cron, err := cronExpr(every)
			if err != nil {
				return err
			}
			if keep < 1 {
				return fmt.Errorf("--keep must be >= 1")
			}
			if dest == "" {
				return fmt.Errorf("--to is required, e.g. --to r2:backups/corral")
			}
			if err := addSchedule(vm, ns(), cron, dest, keep); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "backup schedule for %s: %q → %s, keeping %d (CronJob %s/%s)\n",
				vm, cron, dest, keep, ns(), cronJobName(vm))
			return nil
		},
	}
	c.Flags().StringVar(&every, "every", "24h", "Interval (30m, 1h, 6h, 12h, 24h) or a 5-field cron expression")
	c.Flags().StringVar(&dest, "to", "", "rclone destination dir, e.g. r2:backups/corral (required)")
	c.Flags().IntVar(&keep, "keep", 7, "Backups to retain per VM")
	return c
}

func unscheduleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unschedule <vm>",
		Short: "Remove a VM's backup schedule (existing backups on the remote are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			vm := args[0]
			out, err := runner.Run("kubectl", "delete", "cronjob", cronJobName(vm), "-n", ns(), "--ignore-not-found")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			out, err = runner.Run("kubectl", "delete", "secret", secretName(vm), "-n", ns(), "--ignore-not-found")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

func schedulesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schedules",
		Short: "List backup schedules",
		RunE: func(_ *cobra.Command, _ []string) error {
			out, err := runner.Run("kubectl", "get", "cronjobs", "-n", ns(),
				"-l", scheduleLabelKey(), "-o",
				"custom-columns=VM:.metadata.labels.corral\\.dev/backupsched,SCHEDULE:.spec.schedule,SUSPENDED:.spec.suspend,LAST:.status.lastScheduleTime")
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			fmt.Print(string(out))
			return nil
		},
	}
}

// cronExpr accepts either a supported interval shorthand or a 5-field cron
// expression and returns the cron expression. Matches corral-snapsched's
// convention exactly, for a consistent --every UX across scheduled-op plugins.
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
