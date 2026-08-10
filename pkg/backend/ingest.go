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
	"os"

	"github.com/tuna-os/corral/pkg/bootc"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/proxmoxbe"
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

// ── kubevirt ──────────────────────────────────────────────────────

// Ingest uploads the disk into a CDI DataVolume and creates a VM that adopts
// the resulting PVC as its boot disk.
//
// `virtctl image-upload` is reused rather than reimplementing CDI's upload
// protocol (UploadTokenRequest, then streaming to cdi-uploadproxy with a bearer
// token): pkg/kubevirt already wraps it, including the retries and progress
// that a multi-gigabyte upload over a self-signed proxy needs.
func (a kubevirtAdapter) Ingest(ref types.InstanceRef, disk string, shape Shape) error {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = kubevirt.DefaultNamespace
	}
	size := shape.Disk
	if size == "" {
		size = "20Gi"
	}
	claim := ref.Name + "-disk"

	// The upload creates the DataVolume and its PVC. Progress goes to stderr,
	// which is where the CLI's other long operations report.
	if err := kubevirt.UploadDataVolume(claim, namespace, disk, size,
		kubevirt.PreferredStorageClass(), os.Stderr); err != nil {
		return fmt.Errorf("uploading the disk into %s/%s: %w", namespace, claim, err)
	}

	// PVC, not ImportURL: the disk is already in the cluster, and pointing CDI
	// at a URL would make it fetch what was just uploaded.
	return kubevirt.CreateVM(types.CreateOpts{
		Name: ref.Name, Namespace: namespace, Backend: "kubevirt",
		CPU: shape.CPU, Mem: shape.Mem, Disk: size,
		PVC: claim, SSHPublicKey: shape.SSHKey, UEFI: shape.UEFI,
	})
}

// AcceptsUEFI: the generated VM sets firmware.bootloader.efi when asked, so a
// UEFI guest has an ESP to boot from. Secure Boot is deliberately not enabled —
// it needs an EFI vars volume and a signed bootloader, and switching it on
// silently would break exactly the imported guests this serves.
func (kubevirtAdapter) AcceptsUEFI() bool { return true }

// ── proxmox ───────────────────────────────────────────────────────

// Ingest uploads the disk to a storage that advertises the `import` content
// type and creates a VM with `import-from` pointed at it.
//
// This is the one operation where ADR-0009's "API only" is conditional: a PVE
// without an import-content storage genuinely cannot take a disk image over
// HTTPS, and ImportStorage refuses with the three ways forward rather than
// leaving a half-created VM behind.
func (a proxmoxAdapter) Ingest(ref types.InstanceRef, disk string, shape Shape) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	storage, err := client.ImportStorage()
	if err != nil {
		return err
	}
	node := client.Node()
	if node == "" {
		nodes, err := client.Nodes()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.Ready() {
				node = n.Node
				break
			}
		}
	}
	if node == "" {
		return fmt.Errorf("proxmox: no ready node to upload the disk to")
	}

	volume, err := client.UploadImport(node, storage.Storage, disk)
	if err != nil {
		return err
	}

	task, err := client.Create(proxmoxbe.CreateOpts{
		Name: ref.Name, Node: node,
		Cores: shape.CPU, Mem: shape.Mem, Disk: shape.Disk,
		Storage: storage.Storage, Image: volume,
		SSHKeys: shape.SSHKey, Tags: shape.Tags,
		UEFI: shape.UEFI,
	})
	if err != nil {
		return err
	}
	// Creation with an import is minutes of disk copying; waiting means a
	// caller that got no error has a VM, not a queued task.
	return client.WaitTask(task, proxmoxbe.DefaultTimeout)
}

// AcceptsUEFI: PVE expresses this as `bios: ovmf` plus an EFI disk, which the
// create path sets when asked.
func (proxmoxAdapter) AcceptsUEFI() bool { return true }

// ── incus ─────────────────────────────────────────────────────────

// Ingest publishes a local disk image as a temporary Incus VM image, launches an
// Incus VM from it, and deletes the temporary image.
func (a incusAdapter) Ingest(ref types.InstanceRef, disk string, shape Shape) error {
	return a.client.IngestVM(ref.Name, disk, shape.CPU, shape.Mem)
}

// AcceptsUEFI: Incus VMs always boot using OVMF (UEFI).
func (incusAdapter) AcceptsUEFI() bool { return true }

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
	default:
		return fmt.Sprintf("the %s backend has no ingest path", backend)
	}
}
