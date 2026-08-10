package proxmoxbe

// Proxmox's REST API exposes vzdump as an asynchronous task, but its stdout
// mode is deliberately restricted to the node CLI. Export therefore uses the
// API client to resolve the guest and then runs the documented node-local
// `vzdump --stdout` path over SSH for the archive bytes.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type vzdumpRunner func(context.Context, Guest, string) error

var runVzdump vzdumpRunner = streamVzdump

// SetVzdumpRunner replaces the node archive stream for tests. Passing nil
// restores the real SSH implementation.
func SetVzdumpRunner(r func(context.Context, Guest, string) error) {
	if r == nil {
		runVzdump = streamVzdump
		return
	}
	runVzdump = r
}

// ExportVzdump writes a snapshot-mode VM or CT backup to dest. It does not
// label the result as a disk: vzdump archives can only be restored by PVE.
func (c *Client) ExportVzdump(ctx context.Context, guest Guest, dest string) error {
	if guest.Node == "" || guest.VMID == 0 {
		return fmt.Errorf("proxmox: cannot export a guest without its node and vmid")
	}
	return runVzdump(ctx, guest, dest)
}

func streamVzdump(ctx context.Context, guest Guest, dest string) error {
	user := os.Getenv("CORRAL_PROXMOX_SSH_USER")
	if user == "" {
		user = "root"
	}
	host := os.Getenv("CORRAL_PROXMOX_SSH_HOST")
	if host == "" {
		host = guest.Node
	}
	target := user + "@" + host
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		target,
		"vzdump", strconv.Itoa(guest.VMID),
		"--mode", "snapshot",
		"--stdout",
		"--compress", "zstd",
	}

	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("proxmox: creating vzdump destination: %w", err)
	}
	defer file.Close()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = file
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dest)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("proxmox: vzdump over SSH: %s: %w", detail, err)
		}
		return fmt.Errorf("proxmox: vzdump over SSH: %w", err)
	}
	return nil
}
