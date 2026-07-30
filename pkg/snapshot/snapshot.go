// Package snapshot is the backend-neutral snapshot contract (#134).
//
// Snapshots were a KubeVirt-only feature: the API, the web handlers, and the
// snapsched plugin all spoke VirtualMachineSnapshot directly. Every other
// backend can snapshot too, but each with different semantics and different
// unsafe cases, so the useful abstraction is not "take a snapshot" — it is
// "take a snapshot, and say honestly how consistent it is, or refuse with a
// reason the caller can act on".
//
// Adapters live in core rather than in the plugin because ADR-0007 puts
// backend adapters and capability reporting in the main binary; scheduling and
// retention policy stay in the plugin, which consumes this contract.
package snapshot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// Consistency describes what a snapshot actually captured. Backends differ,
// and the difference decides whether a restore boots cleanly or replays a
// journal, so it is reported rather than hidden.
type Consistency string

const (
	// Offline — the instance was not running. Nothing was in flight.
	Offline Consistency = "offline"
	// Filesystem — the guest filesystem was frozen for the capture, so it is
	// consistent even though the instance kept running.
	Filesystem Consistency = "filesystem"
	// Crash — captured live with nothing quiesced. Restoring is equivalent to
	// booting after a power cut: usually fine on a journalling filesystem,
	// never guaranteed for a database mid-write.
	Crash Consistency = "crash"
)

// Snapshot is one capture, in the shape every backend can populate.
type Snapshot struct {
	Name        string      `json:"name"`
	Source      string      `json:"source"`  // instance the snapshot belongs to
	Created     string      `json:"created"` // backend-reported, RFC3339 where available
	Ready       bool        `json:"ready"`   // usable for a restore right now
	Consistency Consistency `json:"consistency"`
}

// Adapter is one backend's snapshot implementation. Every method takes a
// canonical InstanceRef: a bare name is ambiguous across contexts, and a
// retention job that prunes the wrong context's snapshots is unrecoverable.
type Adapter interface {
	// Create captures the instance. An empty name asks the adapter to
	// generate one. It returns the snapshot as recorded, including the
	// consistency actually achieved — which may be weaker than ideal.
	Create(ref types.InstanceRef, name string) (Snapshot, error)
	// List returns the instance's snapshots, newest first.
	List(ref types.InstanceRef) ([]Snapshot, error)
	// Restore rolls the instance back. Backends that require the instance to
	// be stopped enforce that themselves rather than silently destroying a
	// running guest.
	Restore(ref types.InstanceRef, name string) error
	// Delete removes one snapshot. Deleting one that is already gone is not
	// an error — retention races with a human clearing up.
	Delete(ref types.InstanceRef, name string) error
}

// Unsupported is returned when a backend cannot snapshot at all, or cannot
// snapshot this particular instance in its current state. It is a distinct
// type so callers — the web UI graying out a button, a scheduler declining to
// register a job — can tell "not possible here" from "it broke".
type Unsupported struct {
	Backend string
	Reason  string
	Remedy  string
}

func (e *Unsupported) Error() string {
	msg := fmt.Sprintf("snapshots are not available for the %s backend: %s", e.Backend, e.Reason)
	if e.Remedy != "" {
		msg += " — " + e.Remedy
	}
	return msg
}

// unsupported is the constructor adapters use for their refusal paths.
func unsupported(backend, reason, remedy string) error {
	return &Unsupported{Backend: backend, Reason: reason, Remedy: remedy}
}

// For returns the adapter for a reference's backend.
func For(ref types.InstanceRef) (Adapter, error) {
	switch ref.Backend {
	case "kubevirt":
		return KubeVirt{}, nil
	case "libvirt":
		return Libvirt{}, nil
	case "incus":
		return Incus{}, nil
	case "qemu":
		return QEMU{}, nil
	case "":
		return nil, fmt.Errorf("instance reference has no backend; snapshots need a fully-scoped instance")
	default:
		return nil, unsupported(ref.Backend, "no adapter is implemented", "file an issue if this backend supports snapshots")
	}
}

// Supported reports whether snapshots are implemented for a backend at all.
// It answers the capability question without probing the instance, so
// capability reporting and UI gating don't shell out.
func Supported(backend string) bool {
	switch backend {
	case "kubevirt", "libvirt", "incus", "qemu":
		return true
	}
	return false
}

// AutoPrefix marks snapshots a schedule created. Retention only ever deletes
// these: a snapshot someone took by hand before a risky upgrade must not be
// swept away by a policy they forgot was running.
const AutoPrefix = "auto-"

// AutoName is the name a scheduled snapshot gets. unix is the capture time in
// seconds, passed in rather than read from the clock so callers stay testable.
func AutoName(instance string, unix int64) string {
	return fmt.Sprintf("%s%s-%d", AutoPrefix, instance, unix)
}

// IsAuto reports whether a snapshot was created by a schedule.
func IsAuto(s Snapshot) bool { return strings.HasPrefix(s.Name, AutoPrefix) }

// Prune enforces retention for one instance in one context: keep the newest
// `keep` scheduled snapshots, delete the rest. Manual snapshots are never
// touched. It returns the names deleted, in the order deleted.
//
// Retention is per-reference by construction — two contexts running a
// same-named instance prune independently, because the ref carries the
// context.
func Prune(ref types.InstanceRef, keep int) ([]string, error) {
	if keep < 1 {
		return nil, fmt.Errorf("retention must keep at least 1 snapshot, got %d", keep)
	}
	adapter, err := For(ref)
	if err != nil {
		return nil, err
	}
	snaps, err := adapter.List(ref)
	if err != nil {
		return nil, err
	}
	var auto []Snapshot
	for _, s := range snaps {
		if IsAuto(s) {
			auto = append(auto, s)
		}
	}
	sortNewestFirst(auto)
	if len(auto) <= keep {
		return nil, nil
	}
	// Oldest first. If the run dies partway — storage busy, network gone — the
	// snapshots that survive are the newest ones, which is the outcome anyone
	// would choose. Deleting newest-first would eat the recent history and
	// leave the stale tail.
	excess := auto[keep:]
	var deleted []string
	for i := len(excess) - 1; i >= 0; i-- {
		s := excess[i]
		if err := adapter.Delete(ref, s.Name); err != nil {
			// Report what was already removed alongside the failure: a partial
			// prune that reports nothing looks like a no-op on the next run.
			return deleted, fmt.Errorf("pruning %s: %w", s.Name, err)
		}
		deleted = append(deleted, s.Name)
	}
	return deleted, nil
}

// sortNewestFirst orders by the backend's reported creation time, falling back
// to the name. Auto names embed a unix timestamp, so they sort correctly by
// name too — which matters for backends that report no timestamp at all.
func sortNewestFirst(snaps []Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].Created != snaps[j].Created {
			return snaps[i].Created > snaps[j].Created
		}
		return nameOrder(snaps[i].Name) > nameOrder(snaps[j].Name)
	})
}

// nameOrder zero-pads the trailing timestamp of an auto name so string
// comparison matches numeric order across a digit-count change.
func nameOrder(name string) string {
	i := strings.LastIndex(name, "-")
	if i < 0 || i == len(name)-1 {
		return name
	}
	suffix := name[i+1:]
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return name
		}
	}
	return fmt.Sprintf("%s-%020s", name[:i], suffix)
}
