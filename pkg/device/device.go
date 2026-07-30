// Package device is the backend-neutral host-device passthrough contract
// (#129): discovering what a host can hand to a guest, and attaching it.
//
// GPU support was KubeVirt-specific — `permittedHostDevices` in the KubeVirt CR
// plus `spec.domain.devices.gpus` on the VM. libvirt and Incus both do
// passthrough, but neither has a cluster-level allowlist, so the useful common
// shape is not "the KubeVirt model, ported" but the two things every backend
// really has: a device identified by its PCI address, and an attachment named
// on one instance.
//
// # Why this package is careful
//
// Passthrough is the most destructive thing Corral does. Binding a device to
// vfio-pci takes it away from the host driver: do that to the GPU driving the
// console and the machine goes dark; do it to the disk controller and the host
// loses its root filesystem. So mutation here is deliberately conservative —
// the boot GPU is refused outright, every attachment reports the restart and
// migration consequences before it happens, and anything the adapter is unsure
// of is a typed refusal rather than a best guess.
//
// Per ADR-0007 the adapters live in core, but only `corral-gpu` calls the
// mutating half, and it is the plugin that declares the privileged permissions
// in its marketplace metadata.
package device

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) { runner = r }

// Class is a coarse device kind, enough for a caller to filter sensibly
// without Corral pretending to a full PCI class database.
type Class string

const (
	GPU     Class = "gpu"
	Audio   Class = "audio"
	Network Class = "network"
	Storage Class = "storage"
	Other   Class = "other"
)

// Device is one passthrough-capable host device.
type Device struct {
	// ID is how this device is named to the backend when attaching. For
	// libvirt and Incus that is the PCI address; for KubeVirt it is the
	// resource name the cluster advertises, because that is the only handle a
	// scheduler-mediated backend gives you.
	ID string `json:"id"`
	// Address is the PCI address (0000:01:00.0) where the backend reports one.
	Address     string `json:"address,omitempty"`
	Vendor      string `json:"vendor,omitempty"`      // vendor ID, e.g. 10de
	Product     string `json:"product,omitempty"`     // product ID, e.g. 2204
	Description string `json:"description,omitempty"` // human label
	Driver      string `json:"driver,omitempty"`      // host driver currently bound
	Class       Class  `json:"class"`
	// Node is the cluster node holding the device. Empty for single-host
	// backends, where "which machine" is answered by the context.
	Node string `json:"node,omitempty"`
	// BootDisplay marks the device currently driving the host's console.
	// Attaching it is refused: the host loses its display, usually before
	// anyone realises why.
	BootDisplay bool `json:"bootDisplay,omitempty"`
	// Ready reports whether the device is already bound to a passthrough
	// driver. A device that is not ready can often still be attached — libvirt
	// with managed='yes' rebinds it at start — but the caller should know.
	Ready bool `json:"ready"`
	// Attachable reports whether this backend can actually hand the device to
	// a guest. Not every discovered device can be: a cloud VM's synthetic GPU
	// is a VMBus device with no PCI address at all, and both libvirt and Incus
	// address passthrough devices by PCI address. Such a device is still worth
	// listing — an operator asking "what GPUs are here" should be told — but
	// attaching it is refused with the reason rather than attempted.
	Attachable bool `json:"attachable"`
	// Unattachable explains why, when Attachable is false.
	Unattachable string `json:"unattachable,omitempty"`
}

// Attachment is a device bound to an instance.
type Attachment struct {
	Name   string `json:"name"`   // caller-chosen handle for detach
	Device string `json:"device"` // the Device.ID that was attached
}

// Consequence is something that will happen if an attachment proceeds. The
// issue asks for restart/migration/security implications to be surfaced
// *before* mutation, so they are returned rather than logged.
type Consequence string

const (
	// RequiresRestart — the guest must be power-cycled for the change to
	// take effect. True for every VM backend here.
	RequiresRestart Consequence = "the instance must be restarted for this to take effect"
	// BlocksMigration — a passed-through device pins the guest to this host.
	BlocksMigration Consequence = "the instance can no longer be live-migrated while this device is attached"
	// HostLosesDevice — the host driver is unbound; whatever was using the
	// device on the host stops working.
	HostLosesDevice Consequence = "the host loses the use of this device until it is detached"
	// GuestGetsDMA — passthrough grants the guest DMA to host memory through
	// the device. Worth stating: it makes the guest a trust boundary.
	GuestGetsDMA Consequence = "the guest gains direct memory access through this device, so it must be as trusted as the host"
)

// Adapter is one backend's passthrough implementation.
type Adapter interface {
	// List discovers passthrough-capable devices in a context. Read-only: it
	// must never bind, unbind, or otherwise disturb a device.
	List(context string) ([]Device, error)
	// Consequences reports what attaching dev will cause, so a caller can
	// confirm before mutating.
	Consequences(dev Device) []Consequence
	// Attach binds dev to the instance under a caller-chosen name.
	Attach(ref types.InstanceRef, dev Device, name string) error
	// Detach removes a named attachment. Detaching one that isn't there is
	// not an error.
	Detach(ref types.InstanceRef, name string) error
	// Attached lists an instance's current attachments.
	Attached(ref types.InstanceRef) ([]Attachment, error)
}

// Unsupported reports a backend that cannot do passthrough, or cannot do it
// for this device. Same role as its counterparts in pkg/snapshot and
// pkg/export: it separates "not possible here" from "it broke".
type Unsupported struct {
	Backend string
	Reason  string
	Remedy  string
}

func (e *Unsupported) Error() string {
	msg := fmt.Sprintf("device passthrough is not available for the %s backend: %s", e.Backend, e.Reason)
	if e.Remedy != "" {
		msg += " — " + e.Remedy
	}
	return msg
}

func unsupported(backend, reason, remedy string) error {
	return &Unsupported{Backend: backend, Reason: reason, Remedy: remedy}
}

// Refused reports a device this adapter will not touch. Distinct from
// Unsupported: the backend can do passthrough, but doing it to *this* device
// would break the host.
type Refused struct {
	Device Device
	Reason string
}

func (e *Refused) Error() string {
	return fmt.Sprintf("refusing to pass through %s (%s): %s", e.Device.ID, e.Device.Description, e.Reason)
}

// For returns the adapter for a backend.
func For(backend string) (Adapter, error) {
	switch backend {
	case "kubevirt":
		return KubeVirt{}, nil
	case "libvirt":
		return Libvirt{}, nil
	case "incus":
		return Incus{}, nil
	case "qemu":
		// Corral's local backend builds its own QEMU command line and has no
		// vfio plumbing in it. Rather than emit a half-working -device
		// vfio-pci that the systemd unit generator knows nothing about, this
		// says so.
		return nil, unsupported("qemu",
			"Corral's local QEMU backend has no vfio wiring",
			"use libvirt on the same host for passthrough")
	case "":
		return nil, fmt.Errorf("no backend given; device discovery needs a context")
	default:
		return nil, unsupported(backend, "no adapter is implemented", "")
	}
}

// Supported reports whether passthrough is implemented for a backend.
func Supported(backend string) bool {
	switch backend {
	case "kubevirt", "libvirt", "incus":
		return true
	}
	return false
}

// checkSafe is the gate every adapter's Attach runs first. It is deliberately
// in the shared package rather than per adapter: the failure it prevents —
// taking down the host's console — is identical everywhere, and a backend
// added later should not have to remember to reimplement it.
func checkSafe(dev Device) error {
	if dev.BootDisplay {
		return &Refused{Device: dev, Reason: "it is the host's boot display; passing it through would leave the host with no console"}
	}
	if !dev.Attachable {
		reason := dev.Unattachable
		if reason == "" {
			reason = "this backend cannot address it for passthrough"
		}
		return &Refused{Device: dev, Reason: reason}
	}
	if dev.ID == "" {
		return fmt.Errorf("device has no identifier")
	}
	return nil
}

// pciAttachable fills in Attachable/Unattachable for the backends that address
// devices by PCI address. Found on a real runner: a cloud VM's synthetic GPU
// (hyperv_drm, vendor 1414) is a VMBus device and reports no PCI address, so
// there is nothing to put in a <hostdev> or an `incus config device add`.
func pciAttachable(dev *Device) {
	if dev.Address != "" {
		dev.Attachable = true
		return
	}
	dev.Attachable = false
	dev.Unattachable = "it reports no PCI address — a synthetic or platform device, which cannot be passed through by address"
}

// baseConsequences are true of every backend here. An adapter may add to them
// but should not drop any.
func baseConsequences(dev Device) []Consequence {
	out := []Consequence{RequiresRestart, BlocksMigration, GuestGetsDMA}
	if dev.Driver != "" && dev.Driver != "vfio-pci" {
		// Something on the host is using it right now.
		out = append(out, HostLosesDevice)
	}
	return out
}

// gpuMarkers are product/vendor strings that mean "this is a graphics device".
//
// The lspci-style class words are not enough on their own: libvirt's node-device
// XML reports the *product* name, and a card's product name is "GA102 [GeForce
// RTX 3090]" with vendor "NVIDIA Corporation" — no "VGA", no "display", no
// "3D controller" anywhere in it. Matching only the class words silently
// classified every discrete GPU as Other, which is the one class a caller is
// actually looking for.
var gpuMarkers = []string{
	// class words, as libvirt/lspci report them for the class itself
	"vga", "display controller", "3d controller", "gpu", "graphics",
	// vendor and family names, as they appear in product strings
	"nvidia", "geforce", "quadro", "tesla", "radeon", "amd/ati",
	"firepro", "arc a", "iris xe",
}

// classify maps a PCI class code or a descriptive string onto a Class. Both
// forms turn up: libvirt reports the product name, Incus groups by device kind,
// and lspci-style descriptions are what humans recognise.
func classify(s string) Class {
	l := strings.ToLower(s)
	for _, marker := range gpuMarkers {
		if strings.Contains(l, marker) {
			return GPU
		}
	}
	switch {
	case strings.Contains(l, "audio"), strings.Contains(l, "sound"):
		return Audio
	case strings.Contains(l, "ethernet"), strings.Contains(l, "network"):
		return Network
	case strings.Contains(l, "sata"), strings.Contains(l, "nvme"),
		strings.Contains(l, "raid"), strings.Contains(l, "storage"):
		return Storage
	default:
		return Other
	}
}

// sortDevices gives a stable ordering: GPUs first because they are what
// anyone is looking for, then by address.
func sortDevices(devs []Device) {
	sort.SliceStable(devs, func(i, j int) bool {
		if (devs[i].Class == GPU) != (devs[j].Class == GPU) {
			return devs[i].Class == GPU
		}
		if devs[i].Address != devs[j].Address {
			return devs[i].Address < devs[j].Address
		}
		return devs[i].ID < devs[j].ID
	})
}

func commandError(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}
