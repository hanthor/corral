// Package export is the backend-neutral disk/export contract (#131).
//
// Backup was coupled to KubeVirt storage: the plugin called
// kubevirt.Client.Export, which drives `virtctl vmexport` against a PVC.
// Nothing about "copy this instance's disk somewhere" is Kubernetes-specific,
// but each backend produces a different artifact by a different route, and the
// differences matter to whoever has to restore from it.
//
// So the contract carries what the caller actually needs to know: what artifact
// came out, how big it was, and — reusing pkg/snapshot's vocabulary — how
// consistent it is. An export taken from a running instance is crash-consistent
// and says so rather than pretending otherwise.
//
// Credentials stay out of this package and out of the core binary. Exporting
// writes a local file; getting that file to object storage is the backup
// plugin's job, with the plugin's own rclone config.
package export

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

// runner is the seam for adapters that shell out.
var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) { runner = r }

// Format is the artifact an export produces. Backends do not all produce the
// same kind of thing, and flattening that difference would mislead: an Incus
// export is an instance tarball, not a disk image, and cannot be attached to a
// VM the way a qcow2 can.
type Format string

const (
	// RawGz is a raw disk image, gzipped. The portable disk format.
	RawGz Format = "raw.gz"
	// Qcow2 is a compressed qcow2 disk image.
	Qcow2 Format = "qcow2"
	// IncusTar is Incus's own instance archive — configuration and volumes,
	// restorable only with `incus import`.
	IncusTar Format = "incus-tar"
)

// Request is one export.
type Request struct {
	Ref types.InstanceRef
	// Dest is the local file to write. Bulk data always lands on local disk
	// first; shipping it onward is the caller's business.
	Dest string
	// Format is the artifact wanted. Empty means the backend's native choice,
	// which is what Formats reports first.
	Format Format
}

// Result describes what was produced.
type Result struct {
	Path        string               `json:"path"`
	Format      Format               `json:"format"`
	Bytes       int64                `json:"bytes"`
	Consistency snapshot.Consistency `json:"consistency"`
}

// Progress is reported as the export runs. Total is 0 when the backend cannot
// say how large the artifact will be — most cannot until it is finished, so
// callers must render an indeterminate state rather than divide by zero.
type Progress struct {
	Stage string
	Done  int64
	Total int64
}

// ProgressFunc receives progress updates. Adapters call it from the goroutine
// that is doing the work, so an implementation must not block.
type ProgressFunc func(Progress)

func (p ProgressFunc) report(stage string, done, total int64) {
	if p != nil {
		p(Progress{Stage: stage, Done: done, Total: total})
	}
}

// Adapter is one backend's export implementation.
type Adapter interface {
	// Export writes the instance's disk to req.Dest. It honours ctx
	// cancellation: a multi-gigabyte copy that cannot be stopped is a bug, not
	// a feature.
	Export(ctx context.Context, req Request, progress ProgressFunc) (Result, error)
	// Formats lists what this backend can produce, native choice first.
	Formats() []Format
}

// Unsupported reports a backend that cannot export, or cannot export this
// instance in its current state. Same role as snapshot.Unsupported: it lets a
// caller distinguish "not possible here" from "it broke".
type Unsupported struct {
	Backend string
	Reason  string
	Remedy  string
}

func (e *Unsupported) Error() string {
	msg := fmt.Sprintf("export is not available for the %s backend: %s", e.Backend, e.Reason)
	if e.Remedy != "" {
		msg += " — " + e.Remedy
	}
	return msg
}

func unsupported(backend, reason, remedy string) error {
	return &Unsupported{Backend: backend, Reason: reason, Remedy: remedy}
}

// For returns the adapter for a reference's backend.
func For(ref types.InstanceRef) (Adapter, error) {
	switch ref.Backend {
	case "kubevirt":
		return KubeVirt{}, nil
	case "qemu":
		return QEMU{}, nil
	case "libvirt":
		return Libvirt{}, nil
	case "incus":
		return Incus{}, nil
	case "":
		return nil, fmt.Errorf("instance reference has no backend; export needs a fully-scoped instance")
	default:
		return nil, unsupported(ref.Backend, "no adapter is implemented", "")
	}
}

// Supported reports whether export is implemented for a backend, without
// touching the instance — for capability reporting and UI gating.
func Supported(backend string) bool {
	switch backend {
	case "kubevirt", "qemu", "libvirt", "incus":
		return true
	}
	return false
}

// Formats reports what a backend can produce without constructing an adapter.
func Formats(backend string) []Format {
	adapter, err := For(types.InstanceRef{Backend: backend, Name: "x"})
	if err != nil {
		return nil
	}
	return adapter.Formats()
}

// ── shared helpers ────────────────────────────────────────────────

// pickFormat resolves the requested format against what a backend offers.
func pickFormat(backend string, want Format, offered []Format) (Format, error) {
	if want == "" {
		return offered[0], nil
	}
	for _, f := range offered {
		if f == want {
			return f, nil
		}
	}
	names := make([]string, 0, len(offered))
	for _, f := range offered {
		names = append(names, string(f))
	}
	return "", unsupported(backend,
		fmt.Sprintf("cannot produce %s", want),
		"supported formats: "+strings.Join(names, ", "))
}

// finish stats the produced artifact so the caller learns its real size, and
// catches the case where a backend exits 0 having written nothing.
func finish(path string, format Format, consistency snapshot.Consistency) (Result, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, fmt.Errorf("export reported success but %s is missing: %w", path, err)
	}
	if info.Size() == 0 {
		return Result{}, fmt.Errorf("export produced an empty file at %s", path)
	}
	return Result{Path: path, Format: format, Bytes: info.Size(), Consistency: consistency}, nil
}

// ensureDestDir creates the destination's parent so an adapter's own error is
// about the export rather than a missing directory.
func ensureDestDir(dest string) error {
	if dest == "" {
		return fmt.Errorf("export needs a destination path")
	}
	return os.MkdirAll(filepath.Dir(dest), 0o755)
}

// cancelled reports whether ctx ended, so adapters can return the context's
// error rather than a confusing downstream failure.
func cancelled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func commandError(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}
