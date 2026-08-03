package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// The web-as-a-service commands wrap systemd so "keep the dashboard running"
// is one command instead of a hand-written unit file. User scope is the
// default — it needs no root and survives reboots once lingering is enabled;
// --system installs a machine-wide unit for shared hosts.

var (
	svcSystem  bool   // install a system unit under /etc instead of a --user one
	svcSvcAddr string // listen address baked into ExecStart
)

const webServiceUnit = "corral-web.service"

var webServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run the Corral web UI as a systemd service",
	Long: `Install, remove, or inspect a systemd unit that keeps the Corral web
UI running in the background and starts it at boot.

By default this manages a per-user unit (systemctl --user), which needs no
root. To keep it running when you are not logged in, enable lingering once:

  sudo loginctl enable-linger "$USER"

Use --system to install a machine-wide unit under /etc/systemd/system
(requires root, e.g. run under sudo).`,
}

var webServiceInstallCmd = &cobra.Command{
	Use:   "install [-- <extra corral web flags>]",
	Short: "Write the unit, enable it, and start it now",
	Long: `Write a systemd unit for "corral web", then enable and start it.

Any arguments after "--" are appended verbatim to the ExecStart line, so
theme and demo flags carry over:

  corral web service install --addr 0.0.0.0:8006 -- --accent "#3b82f6"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		bin, err := corralBinaryPath()
		if err != nil {
			return err
		}
		exec := fmt.Sprintf("%s web --addr %s", systemdQuote(bin), systemdQuote(svcSvcAddr))
		for _, a := range args {
			exec += " " + systemdQuote(a)
		}
		unit := renderWebUnit(exec, svcSystem)

		path, err := webUnitPath(svcSystem)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("creating unit directory: %w", err)
		}
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", path)

		if err := runSystemctl(svcSystem, "daemon-reload"); err != nil {
			return err
		}
		if err := runSystemctl(svcSystem, "enable", "--now", webServiceUnit); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Enabled and started %s\n", webServiceUnit)
		if !svcSystem {
			fmt.Fprintln(cmd.OutOrStdout(),
				"Tip: to keep it running while logged out, run:  sudo loginctl enable-linger \""+os.Getenv("USER")+"\"")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Dashboard: http://%s\n", svcSvcAddr)
		return nil
	},
}

var webServiceUninstallCmd = &cobra.Command{
	Use:     "uninstall",
	Aliases: []string{"remove", "rm"},
	Short:   "Stop, disable, and delete the unit",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Best-effort stop/disable — the unit may already be gone; deleting the
		// file is what actually matters, so a failing systemctl is not fatal.
		_ = runSystemctl(svcSystem, "disable", "--now", webServiceUnit)
		path, err := webUnitPath(svcSystem)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
		_ = runSystemctl(svcSystem, "daemon-reload")
		fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", path)
		return nil
	},
}

var webServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show systemctl status for the unit",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSystemctl(svcSystem, "status", "--no-pager", webServiceUnit)
	},
}

var webServicePrintCmd = &cobra.Command{
	Use:   "print [-- <extra corral web flags>]",
	Short: "Print the unit file to stdout without installing it",
	Long: `Print the systemd unit to stdout — useful for immutable/bootc hosts
where units are managed declaratively rather than written at runtime.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		bin, err := corralBinaryPath()
		if err != nil {
			return err
		}
		exec := fmt.Sprintf("%s web --addr %s", systemdQuote(bin), systemdQuote(svcSvcAddr))
		for _, a := range args {
			exec += " " + systemdQuote(a)
		}
		fmt.Fprint(cmd.OutOrStdout(), renderWebUnit(exec, svcSystem))
		return nil
	},
}

// corralBinaryPath resolves the absolute, symlink-free path of the running
// corral binary so the unit points at a stable location, not $PATH.
func corralBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the corral binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// webUnitPath returns where the unit file lives for the chosen scope.
func webUnitPath(system bool) (string, error) {
	if system {
		return filepath.Join("/etc/systemd/system", webServiceUnit), nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating your home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", webServiceUnit), nil
}

// runSystemctl shells out to systemctl, adding --user for user-scope units and
// streaming output straight through so status/errors reach the terminal.
func runSystemctl(system bool, args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found — the service commands need systemd (Linux only)")
	}
	full := args
	if !system {
		full = append([]string{"--user"}, args...)
	}
	c := exec.Command("systemctl", full...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(full, " "), err)
	}
	return nil
}

// renderWebUnit builds the unit text. A system unit waits for the network and
// runs as the invoking user (so it uses their config/registry, not root's).
func renderWebUnit(execStart string, system bool) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Corral web UI (Proxmox-style VM/CT dashboard)\n")
	b.WriteString("Documentation=https://github.com/tuna-os/corral\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", execStart)
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2\n")
	if system {
		// A system unit runs as root by default; pin it to the installing user
		// so it shares that user's kubeconfig, corral config, and registry.
		if u := os.Getenv("SUDO_USER"); u != "" {
			fmt.Fprintf(&b, "User=%s\n", u)
		} else if u := os.Getenv("USER"); u != "" && u != "root" {
			fmt.Fprintf(&b, "User=%s\n", u)
		}
	}
	b.WriteString("\n[Install]\n")
	if system {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String()
}

// systemdQuote wraps a token in double quotes when it contains characters
// systemd would otherwise split on, so paths/args with spaces survive.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"\\'") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func init() {
	webCmd.AddCommand(webServiceCmd)
	webServiceCmd.AddCommand(webServiceInstallCmd, webServiceUninstallCmd, webServiceStatusCmd, webServicePrintCmd)

	webServiceCmd.PersistentFlags().BoolVar(&svcSystem, "system", false,
		"Manage a system-wide unit under /etc/systemd/system (needs root) instead of a --user unit")
	for _, c := range []*cobra.Command{webServiceInstallCmd, webServicePrintCmd} {
		c.Flags().StringVar(&svcSvcAddr, "addr", "127.0.0.1:8006", "Listen address baked into the unit")
	}
}
