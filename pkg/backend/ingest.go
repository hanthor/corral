package backend

// Ingest: the destination half of a cross-backend move (ADR-0010).
//
// pkg/export answers "get this instance's disk out" for every backend.
// Ingester is its mirror — "make this disk a runnable instance here" — and it
// joins the operation families so that "can this backend receive a VM" is a
// type assertion rather than a switch, the same as everything else in this
// package.
//
// bootc solved this once already for two backends, and its Target interface is
// defined in exactly these words: "puts a built disk onto a backend as a
// runnable instance". Rather than write a second implementation, the qemu and
// libvirt ingesters delegate to it, so a bootc disk and a moved disk land the
// same way and there is one place to fix when adoption changes.

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/bootc"
	"github.com/tuna-os/corral/pkg/types"
)

// Shape is what a disk needs around it to become an instance. It is the subset
// of a guest's configuration that survives a move; the rest is reported as
// dropped rather than silently lost (ADR-0010).
type Shape struct {
	CPU int
	// Mem is Corral's usual string form ("4Gi", "2048Mi").
	Mem string
	// Disk is the size to present. An ingester may grow a disk to meet it and
	// must never shrink one — a smaller target would truncate a filesystem.
	Disk string
	// UEFI asks for an EFI boot path. A guest installed under UEFI that boots on
	// a BIOS machine shows a blank screen, so an ingester that cannot express
	// this must refuse rather than proceed.
	UEFI bool
	// OSType is the guest family where the source recorded one ("linux",
	// "windows", ""), which decides disk-bus defaults.
	OSType string
	SSHKey string
	Tags   []string
}

// Ingester creates an instance on this backend from a local disk image.
type Ingester interface {
	// Ingest consumes the disk at path — moving or converting it into the
	// backend's own storage — and creates the instance stopped.
	Ingest(ref types.InstanceRef, disk string, shape Shape) error
	// AcceptsUEFI reports whether this backend can boot a UEFI guest, so the
	// preflight can refuse before the disk is copied rather than after.
	AcceptsUEFI() bool
}

// IngestFamily is registered alongside the other families so the parity matrix
// derives "can receive a moved instance" from the adapter's type.
func init() {
	Families = append(Families, Family{
		Name:       "Ingester",
		Operations: nil, // no matrix row yet; move is not a per-instance op
		Implements: func(a Adapter) bool { _, ok := a.(Ingester); return ok },
	})
}

// CanIngest reports whether a backend can be the destination of a move.
func CanIngest(backend string) bool {
	adapter, err := probe(backend)
	if err != nil {
		return false
	}
	_, ok := adapter.(Ingester)
	return ok
}

// ── qemu ──────────────────────────────────────────────────────────

func (a qemuAdapter) Ingest(ref types.InstanceRef, disk string, shape Shape) error {
	return bootc.QEMUTarget{}.Import(ref, disk, bootc.CreateOpts{
		CPU: shape.CPU, Memory: shape.Mem, Disk: shape.Disk, SSHKey: shape.SSHKey,
	})
}

// AcceptsUEFI is false until the generated unit gains an OVMF firmware path.
// Saying so is the point: a UEFI guest moved here would boot to nothing, and the
// preflight refuses instead of producing one.
func (qemuAdapter) AcceptsUEFI() bool { return false }

// ── libvirt ───────────────────────────────────────────────────────

func (a libvirtAdapter) Ingest(ref types.InstanceRef, disk string, shape Shape) error {
	return bootc.LibvirtTarget{}.Import(ref, disk, bootc.CreateOpts{
		CPU: shape.CPU, Memory: shape.Mem, Disk: shape.Disk, SSHKey: shape.SSHKey,
	})
}

// AcceptsUEFI: the domain XML the libvirt target writes selects firmware, so a
// UEFI guest has somewhere to land.
func (libvirtAdapter) AcceptsUEFI() bool { return true }

// ── the ones that cannot, and why ─────────────────────────────────
//
// These are deliberately *not* Ingester implementations: a stub that returned
// "not implemented" from Ingest would make CanIngest true and let a preflight
// pass a move that cannot work. The refusal belongs where the caller asks
// whether it is possible, so IngestRefusal explains the absence.

// IngestRefusal explains why a backend cannot receive a moved instance. It
// returns "" for a backend that can.
func IngestRefusal(backend string) string {
	if CanIngest(backend) {
		return ""
	}
	switch backend {
	case "incus":
		// Assessed in pkg/bootc and unchanged here: an Incus VM boots from
		// Incus's own image store, `incus import` takes an Incus backup tarball
		// rather than a disk image, and a raw disk attached to an --empty VM
		// leaves the guest without the agent and config drive. The result would
		// look like it worked and behave unlike every other Incus instance.
		return "an Incus VM boots from Incus's own image store and has no supported way to adopt " +
			"a foreign disk; Incus can be a move source but not a destination (see ADR-0010)"
	case "kubevirt":
		return "the CDI upload path is not wired into move yet (ADR-0010's first slice covers " +
			"qemu and libvirt destinations); use `corral create --import` meanwhile"
	case "proxmox":
		return "PVE cannot accept a raw disk over its API on every version; a move here needs a " +
			"storage advertising the import content type, a shared storage path, or SSH to a node " +
			"(see ADR-0010) — not wired yet"
	default:
		return fmt.Sprintf("the %s backend has no ingest path", backend)
	}
}
