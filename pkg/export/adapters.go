package export

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

// ── KubeVirt ──────────────────────────────────────────────────────
//
// virtctl vmexport against the VM's primary PVC. The pre-existing path, behind
// the contract. KubeVirt refuses to export a disk that a running VMI has
// attached, so an export here is always taken from a stopped instance.

type KubeVirt struct{}

func (KubeVirt) Formats() []Format { return []Format{RawGz, Qcow2} }

func (k KubeVirt) Export(ctx context.Context, req Request, progress ProgressFunc) (Result, error) {
	format, err := pickFormat("kubevirt", req.Format, k.Formats())
	if err != nil {
		return Result{}, err
	}
	if err := ensureDestDir(req.Dest); err != nil {
		return Result{}, err
	}
	if err := cancelled(ctx); err != nil {
		return Result{}, err
	}
	client := kubevirt.NewClientForContext(req.Ref.Namespace, req.Ref.Context)

	progress.report("exporting disk", 0, 0)
	if format == Qcow2 {
		// qcow2 needs the raw image on disk first: qemu-img cannot convert a
		// stream. Scratch space sized to the uncompressed disk is required,
		// which is why raw.gz stays the native choice.
		raw := req.Dest + ".raw"
		defer os.Remove(raw)
		if _, err := client.ExportRaw(req.Ref.Name, "", raw); err != nil {
			return Result{}, err
		}
		if err := cancelled(ctx); err != nil {
			return Result{}, err
		}
		progress.report("converting to qcow2", 0, 0)
		if out, err := runner.Run(qemuImg(), "convert", "-O", "qcow2", "-c", raw, req.Dest); err != nil {
			return Result{}, fmt.Errorf("qemu-img convert: %s", commandError(out, err))
		}
	} else if _, err := client.Export(req.Ref.Name, "", req.Dest); err != nil {
		return Result{}, err
	}
	// KubeVirt will not export a disk attached to a running VMI, so whatever
	// came out was taken from a stopped instance.
	return finish(req.Dest, format, snapshot.Offline)
}

// ── local QEMU ────────────────────────────────────────────────────
//
// qemu-img convert straight off the VM's disk. Same safety rule as the
// snapshot adapter and for the same reason: reading a disk while a guest
// writes to it produces a torn image that will not boot, so a running VM is
// refused rather than exported badly.

type QEMU struct{}

func (QEMU) Formats() []Format { return []Format{Qcow2, RawGz} }

func (q QEMU) Export(ctx context.Context, req Request, progress ProgressFunc) (Result, error) {
	format, err := pickFormat("qemu", req.Format, q.Formats())
	if err != nil {
		return Result{}, err
	}
	if req.Ref.Context != "" {
		return Result{}, unsupported("qemu", "the local backend has no contexts", "drop --context for local VMs")
	}
	disk := filepath.Join(qemu.VMHome(), req.Ref.Name, "disk.qcow2")
	if _, err := os.Stat(disk); err != nil {
		return Result{}, fmt.Errorf("local VM %q has no disk at %s: %w", req.Ref.Name, disk, err)
	}
	if running(req.Ref.Name) {
		return Result{}, unsupported("qemu",
			fmt.Sprintf("%s is running, and copying a disk the guest is writing to produces a torn image", req.Ref.Name),
			fmt.Sprintf("stop it first: corral stop %s", req.Ref.Name))
	}
	if err := ensureDestDir(req.Dest); err != nil {
		return Result{}, err
	}
	if err := cancelled(ctx); err != nil {
		return Result{}, err
	}

	// qemu-img reports no incremental progress, but it can say how large the
	// uncompressed disk is — enough for a caller to show a meaningful total
	// rather than a spinner with no scale.
	progress.report("converting disk", 0, virtualSize(disk))
	args := []string{"convert", "-O", "qcow2", "-c", disk, req.Dest}
	if format == RawGz {
		// qemu-img has no gzip output, so raw first and compress after.
		raw := req.Dest + ".raw"
		defer os.Remove(raw)
		if out, err := runner.Run(qemuImg(), "convert", "-O", "raw", disk, raw); err != nil {
			return Result{}, fmt.Errorf("qemu-img convert: %s", commandError(out, err))
		}
		if err := cancelled(ctx); err != nil {
			return Result{}, err
		}
		progress.report("compressing", 0, 0)
		if err := gzipFile(raw, req.Dest); err != nil {
			return Result{}, err
		}
		return finish(req.Dest, format, snapshot.Offline)
	}
	if out, err := runner.Run(qemuImg(), args...); err != nil {
		return Result{}, fmt.Errorf("qemu-img convert: %s", commandError(out, err))
	}
	return finish(req.Dest, format, snapshot.Offline)
}

func running(name string) bool {
	vms, err := qemu.List()
	if err != nil {
		return false
	}
	for _, vm := range vms {
		if vm.Name == name {
			return vm.Running
		}
	}
	return false
}

// ── libvirt ───────────────────────────────────────────────────────
//
// virsh reports the domain's disk path; qemu-img copies it. This only works
// when the disk is on a filesystem this machine can read — a remote
// qemu+ssh:// host's disks are not, and pretending otherwise would produce a
// confusing "no such file" from qemu-img instead of an explanation.

type Libvirt struct{}

func (Libvirt) Formats() []Format { return []Format{Qcow2, RawGz} }

func (l Libvirt) virsh(ref types.InstanceRef, args ...string) ([]byte, error) {
	uri := ref.Context
	if uri == "" {
		uri = "qemu:///system"
	}
	return runner.Run("virsh", append([]string{"-c", uri}, args...)...)
}

func (l Libvirt) Export(ctx context.Context, req Request, progress ProgressFunc) (Result, error) {
	format, err := pickFormat("libvirt", req.Format, l.Formats())
	if err != nil {
		return Result{}, err
	}
	if isRemoteURI(req.Ref.Context) {
		return Result{}, unsupported("libvirt",
			fmt.Sprintf("%s is a remote hypervisor and its disks are not on this filesystem", req.Ref.Context),
			"export from the hypervisor host itself, or use a shared storage pool")
	}
	if err := ensureDestDir(req.Dest); err != nil {
		return Result{}, err
	}

	disk, err := l.diskPath(req.Ref)
	if err != nil {
		return Result{}, err
	}
	consistency := snapshot.Offline
	if out, err := l.virsh(req.Ref, "domstate", req.Ref.Name); err == nil &&
		strings.Contains(strings.ToLower(string(out)), "running") {
		// Unlike the local QEMU backend there is no cheap way to stop here on
		// the caller's behalf, so this is disclosed rather than refused — the
		// issue asks for exactly that: preserve semantics where supported and
		// disclose the crash-consistent fallback.
		consistency = snapshot.Crash
	}
	if err := cancelled(ctx); err != nil {
		return Result{}, err
	}

	progress.report("converting disk", 0, virtualSize(disk))
	target := "qcow2"
	out := req.Dest
	if format == RawGz {
		target, out = "raw", req.Dest+".raw"
		defer os.Remove(out)
	}
	args := []string{"convert", "-O", target}
	if target == "qcow2" {
		args = append(args, "-c")
	}
	args = append(args, disk, out)
	if o, err := runner.Run(qemuImg(), args...); err != nil {
		return Result{}, fmt.Errorf("qemu-img convert: %s", commandError(o, err))
	}
	if format == RawGz {
		progress.report("compressing", 0, 0)
		if err := gzipFile(out, req.Dest); err != nil {
			return Result{}, err
		}
	}
	return finish(req.Dest, format, consistency)
}

// diskPath reads the domain's first disk from `virsh domblklist`, whose output
// is a two-column table after a dashed separator.
func (l Libvirt) diskPath(ref types.InstanceRef) (string, error) {
	out, err := l.virsh(ref, "domblklist", ref.Name, "--details")
	if err != nil {
		return "", fmt.Errorf("virsh domblklist: %s", commandError(out, err))
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// Type Device Target Source — only file-backed disks can be copied.
		if len(fields) == 4 && fields[0] == "file" && fields[1] == "disk" {
			return fields[3], nil
		}
	}
	return "", unsupported("libvirt",
		fmt.Sprintf("%s has no file-backed disk to export", ref.Name),
		"network- or volume-backed disks must be exported through their storage system")
}

// isRemoteURI reports whether a libvirt URI names another host, whose disk
// files this process cannot open.
func isRemoteURI(uri string) bool {
	if uri == "" {
		return false
	}
	// qemu+ssh://host/system, qemu+tcp://…, qemu://host/system all carry a
	// host; qemu:///system and qemu:///session do not.
	scheme, rest, found := strings.Cut(uri, "://")
	if !found {
		return false
	}
	_ = scheme
	return !strings.HasPrefix(rest, "/")
}

// ── Incus ─────────────────────────────────────────────────────────
//
// `incus export` produces Incus's own instance archive: configuration plus
// volumes, restorable with `incus import` and nothing else. That is genuinely
// a different artifact from a disk image, so it gets its own format rather
// than being labelled qcow2 and disappointing whoever tries to boot it.

type Incus struct{}

func (Incus) Formats() []Format { return []Format{IncusTar} }

func (i Incus) Export(ctx context.Context, req Request, progress ProgressFunc) (Result, error) {
	format, err := pickFormat("incus", req.Format, i.Formats())
	if err != nil {
		return Result{}, err
	}
	if err := ensureDestDir(req.Dest); err != nil {
		return Result{}, err
	}
	if err := cancelled(ctx); err != nil {
		return Result{}, err
	}
	remote := req.Ref.Context
	if remote == "" {
		remote = "local"
	}

	consistency := snapshot.Offline
	if i.running(remote, req.Ref.Name) {
		// `incus export` of a running instance snapshots it statelessly first,
		// which is crash consistency by another name.
		consistency = snapshot.Crash
	}

	progress.report("exporting instance", 0, 0)
	out, err := runner.Run("incus", "export", remote+":"+req.Ref.Name, req.Dest, "--instance-only")
	if err != nil {
		return Result{}, fmt.Errorf("incus export: %s", commandError(out, err))
	}
	return finish(req.Dest, format, consistency)
}

func (Incus) running(remote, name string) bool {
	instances, err := incus.NewClient(remote).ListInstances()
	if err != nil {
		return false
	}
	for _, inst := range instances {
		if inst.Name == name {
			return strings.EqualFold(inst.Status, "running")
		}
	}
	return false
}

// ── tool helpers ──────────────────────────────────────────────────

// qemuImg resolves qemu-img the way pkg/qemu does, so a brew-only host behaves
// the same here.
func qemuImg() string {
	if bin, err := runner.LookPath("qemu-img"); err == nil {
		return bin
	}
	for _, base := range []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin", "/usr/local/bin"} {
		candidate := filepath.Join(base, "qemu-img")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "qemu-img"
}

// diskInfo is here for adapters that want the uncompressed size for progress.
type diskInfo struct {
	VirtualSize int64 `json:"virtual-size"`
}

// virtualSize reports a disk's uncompressed size, or 0 when qemu-img won't say.
func virtualSize(path string) int64 {
	out, err := runner.Run(qemuImg(), "info", "--output=json", path)
	if err != nil {
		return 0
	}
	var info diskInfo
	if json.Unmarshal(out, &info) != nil {
		return 0
	}
	return info.VirtualSize
}
