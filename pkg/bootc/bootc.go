// Package bootc separates building a bootable-container disk from putting it
// on a backend (#130).
//
// The bootc plugin did both at once: a privileged Job on the cluster ran
// `bootc install to-disk` into a PVC, and the same code then created the
// KubeVirt VM around it. Nothing between those two steps was reusable, so a
// laptop or a libvirt host could not have a bootc VM at all — even though the
// disk a bootc image produces is an ordinary bootable disk that any hypervisor
// can run.
//
// Splitting it gives two small interfaces:
//
//	Builder  bootc container image  →  a bootable disk image
//	Target   a bootable disk image  →  an instance on some backend
//
// They compose in any pairing that makes sense. Building on the cluster and
// running on the cluster is the existing path. Building locally with podman
// and running locally under QEMU is new, and so is building locally and
// defining a libvirt domain.
package bootc

import (
	"fmt"
	"strings"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) { runner = r }

// BuildRequest is one disk build.
type BuildRequest struct {
	// Image is the bootc container image, e.g. ghcr.io/ublue-os/bluefin:latest.
	Image string
	// Dest is the disk file to produce.
	Dest string
	// Size is the disk to allocate, e.g. "20G". bootc needs the whole disk up
	// front because it partitions it.
	Size string
	// SSHKey is injected as root's authorized_keys, since a freshly built
	// bootc disk otherwise has no way in.
	SSHKey string
	// Filesystem overrides the one the detected backend implies. Empty is the
	// right answer almost always: the backend decides, because composefs
	// images ship a btrfs-only initramfs and ostree images want xfs.
	Filesystem string
}

// BuildResult describes the disk produced.
type BuildResult struct {
	Path   string `json:"path"`
	Format string `json:"format"` // "raw" or "qcow2"
	Bytes  int64  `json:"bytes"`
	// Backend is the bootc storage backend the image turned out to need,
	// "ostree" or "composefs". Reported because it determines the root
	// filesystem and the bootloader layout.
	Backend string `json:"backend,omitempty"`
}

// Builder turns a bootc container image into a bootable disk.
type Builder interface {
	// Available reports why this builder cannot run here, or nil. Checked
	// before a multi-gigabyte pull rather than after.
	Available() error
	// Build produces the disk, writing progress as it goes.
	Build(req BuildRequest, progress func(string)) (BuildResult, error)
}

// CreateOpts are the instance settings a Target applies around the disk.
type CreateOpts struct {
	CPU    int
	Memory string
	// Disk is the size to present. A bootc disk is already sized by the build;
	// a target may grow it but must never shrink it.
	Disk   string
	SSHKey string
}

// Target puts a built disk onto a backend as a runnable instance.
type Target interface {
	// Requires lists host prerequisites, named so a missing one is actionable.
	Requires() []string
	// Import creates the instance from disk. The disk is consumed — moved or
	// converted into the backend's own storage — so a caller that wants to
	// keep it should copy it first.
	Import(ref types.InstanceRef, disk string, opts CreateOpts) error
}

// Unsupported reports a backend that cannot host a bootc disk, or a builder
// that cannot run here. Same role as its counterparts in pkg/snapshot,
// pkg/export and pkg/device: it separates "not possible here" from "it broke".
type Unsupported struct {
	What   string
	Reason string
	Remedy string
}

func (e *Unsupported) Error() string {
	msg := fmt.Sprintf("bootc is not available for %s: %s", e.What, e.Reason)
	if e.Remedy != "" {
		msg += " — " + e.Remedy
	}
	return msg
}

func unsupported(what, reason, remedy string) error {
	return &Unsupported{What: what, Reason: reason, Remedy: remedy}
}

// TargetFor returns the target for a backend.
func TargetFor(backend string) (Target, error) {
	switch backend {
	case "qemu":
		return QEMUTarget{}, nil
	case "libvirt":
		return LibvirtTarget{}, nil
	case "incus":
		// Assessed rather than attempted, which the issue explicitly allows.
		//
		// An Incus VM boots from Incus's own image store, and there is no
		// supported way to hand it a foreign pre-partitioned disk as its root
		// device: `incus import` takes an Incus backup tarball, not a disk
		// image, and attaching a raw disk to an --empty VM leaves the guest
		// without the Incus agent, config drive, or the metadata Incus expects
		// to manage. The result would look like it worked and then behave
		// unlike every other Incus instance.
		//
		// The honest path for Incus is publishing the built disk as an Incus
		// image (`incus image import` with a metadata tarball), which is a
		// different feature — image publishing — and belongs in its own issue.
		return nil, unsupported("the incus backend",
			"an Incus VM boots from Incus's own image store and has no supported way to adopt a foreign pre-partitioned disk",
			"build for libvirt or local QEMU, or track Incus image publishing separately")
	case "kubevirt":
		// The cluster path predates this package and lives in pkg/kubevirt,
		// wired to PVCs and a privileged builder Job with no counterpart here.
		return nil, fmt.Errorf("KubeVirt's bootc path lives in pkg/kubevirt — use the cluster builder")
	case "":
		return nil, fmt.Errorf("no backend given; a bootc target needs a context")
	default:
		return nil, unsupported("backend "+backend, "no target is implemented", "")
	}
}

// TargetSupported reports whether a backend can host a locally built bootc
// disk, without constructing anything.
func TargetSupported(backend string) bool {
	switch backend {
	case "qemu", "libvirt":
		return true
	}
	return false
}

// SupportedBackends is what the plugin declares in its marketplace metadata.
// kubevirt is included because its own path still works; incus is not, per
// TargetFor's assessment.
var SupportedBackends = []string{"kubevirt", "qemu", "libvirt"}

func commandError(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}
