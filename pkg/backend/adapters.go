package backend

// One adapter per backend: the only place a backend's own signature is
// translated into the contract.
//
// These implement exactly what their backend can do *today*, and nothing more.
// Where a family is absent the gap is real and `docs/backend-parity.md` names the
// native mechanism that would fill it — an adapter that stubs a method to satisfy
// an interface would put the lie back where the conformance tests just removed
// it.
//
// Adapters must be constructible from a bare InstanceRef, because capability
// derivation probes the type rather than a live connection (see probe). Nothing
// in a constructor may dial anything.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/proxmoxbe"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

func init() {
	Register("kubevirt", func(ref types.InstanceRef) (Adapter, error) {
		ns := ref.Namespace
		if ns == "" {
			ns = kubevirt.DefaultNamespace
		}
		return kubevirtAdapter{client: kubevirt.NewClientForContext(ns, ref.Context)}, nil
	})
	Register("qemu", func(types.InstanceRef) (Adapter, error) { return qemuAdapter{}, nil })
	Register("incus", func(ref types.InstanceRef) (Adapter, error) {
		return incusAdapter{client: incus.NewClient(ref.Context)}, nil
	})
	Register("libvirt", func(ref types.InstanceRef) (Adapter, error) {
		return libvirtAdapter{client: libvirt.NewClient(ref.Context)}, nil
	})
	Register("proxmox", func(ref types.InstanceRef) (Adapter, error) {
		// Deferred: the client needs configuration, and probe() must not fail
		// for a backend that is merely unconfigured — the families a backend
		// implements are a property of its type.
		return proxmoxAdapter{context: ref.Context}, nil
	})
}

// ── KubeVirt ──────────────────────────────────────────────────────
//
// The reference implementation, and the only backend that satisfies every
// family. That is the fact the parity work exists to change.

type kubevirtAdapter struct{ client *kubevirt.Client }

func (kubevirtAdapter) Backend() string { return "kubevirt" }

func (a kubevirtAdapter) Start(name string) error  { return a.client.StartVM(name) }
func (a kubevirtAdapter) Stop(name string) error   { return a.client.StopVM(name) }
func (a kubevirtAdapter) Delete(name string) error { return a.client.DeleteVM(name) }

func (a kubevirtAdapter) Restart(name string) error { return a.client.RestartVM(name) }

func (a kubevirtAdapter) Pause(name string) error  { return a.client.PauseVM(name) }
func (a kubevirtAdapter) Resume(name string) error { return a.client.UnpauseVM(name) }

func (a kubevirtAdapter) Scale(name string, cores int, mem string) error {
	return a.client.Scale(name, cores, mem)
}

// HotplugsLive follows the VMI's own live-migratable condition, which is what
// KubeVirt ties hotplug to.
func (a kubevirtAdapter) HotplugsLive(name string) bool {
	vms, err := a.client.ListVMs()
	if err != nil {
		return false
	}
	for _, vm := range vms {
		if vm.Name == name {
			return vm.LiveMigratable
		}
	}
	return false
}

func (a kubevirtAdapter) AddDisk(name, size string) error {
	_, err := a.client.AddVolume(name, size)
	return err
}

func (a kubevirtAdapter) RemoveDisk(name, disk string) error {
	return a.client.RemoveVolume(name, disk)
}

func (a kubevirtAdapter) ExpandDisk(name, disk, size string) error {
	// KubeVirt resizes the PVC, not a disk slot; the disk argument is the claim.
	if disk == "" {
		disk = name
	}
	return a.client.ExpandDisk(disk, size)
}

func (a kubevirtAdapter) Migrate(name, target string) error { return a.client.Migrate(name, target) }

func (a kubevirtAdapter) CanMigrate(name string) (bool, string) {
	if a.HotplugsLive(name) {
		return true, ""
	}
	return false, "the VM is not live-migratable (a persistent RWO disk, or masquerade networking is absent)"
}

func (a kubevirtAdapter) Clone(source, target string) error { return a.client.Clone(source, target) }

func (a kubevirtAdapter) MarkTemplate(name string, on bool) error {
	return a.client.MarkTemplate(name, on)
}

func (a kubevirtAdapter) SetTag(name, tag string, on bool) error {
	return a.client.SetTag(name, tag, on)
}

func (a kubevirtAdapter) Metrics(name string) (map[string]string, error) {
	return a.client.Metrics(name), nil
}

func (a kubevirtAdapter) Events(name string) ([]Event, error) {
	events, err := a.client.Events(name)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(events))
	for _, e := range events {
		out = append(out, Event{Time: e.Time, Kind: e.Type, Reason: e.Reason,
			Object: e.Object, Message: e.Message})
	}
	return out, nil
}

// ── local QEMU ────────────────────────────────────────────────────
//
// Power plus a real restart. Everything else is a gap the matrix names: the unit
// already opens a QMP socket, which is what pause and metrics would use.

type qemuAdapter struct{}

func (qemuAdapter) Backend() string { return "qemu" }

func (qemuAdapter) Start(name string) error  { return qemu.Start(name) }
func (qemuAdapter) Stop(name string) error   { return qemu.Stop(name) }
func (qemuAdapter) Delete(name string) error { return qemu.Delete(name) }

// Restart is a stop then a start because a local unit has no other reboot: the
// distinction Restarter draws is whether the backend has its own mechanism, and
// systemd restarting the unit *is* qemu's own mechanism.
func (qemuAdapter) Restart(name string) error {
	if err := qemu.Stop(name); err != nil {
		return err
	}
	return qemu.Start(name)
}

func (qemuAdapter) Pause(name string) error  { return qemu.Pause(name) }
func (qemuAdapter) Resume(name string) error { return qemu.Resume(name) }

// ── Incus ─────────────────────────────────────────────────────────

type incusAdapter struct{ client incus.Client }

func (incusAdapter) Backend() string { return "incus" }

func (a incusAdapter) Start(name string) error  { return a.client.Start(name) }
func (a incusAdapter) Stop(name string) error   { return a.client.Stop(name) }
func (a incusAdapter) Delete(name string) error { return a.client.Delete(name) }

func (a incusAdapter) Restart(name string) error { return a.client.Restart(name) }
func (a incusAdapter) Pause(name string) error   { return a.client.Pause(name) }
func (a incusAdapter) Resume(name string) error  { return a.client.Resume(name) }

func (a incusAdapter) Address(name string) (string, error) {
	instances, err := a.client.ListInstances()
	if err != nil {
		return "", err
	}
	for _, inst := range instances {
		if inst.Name == name {
			return inst.Address(), nil
		}
	}
	return "", fmt.Errorf("incus: no instance named %q", name)
}

// ── libvirt ───────────────────────────────────────────────────────

type libvirtAdapter struct{ client libvirt.Client }

func (libvirtAdapter) Backend() string { return "libvirt" }

func (a libvirtAdapter) Start(name string) error   { return a.client.Start(name) }
func (a libvirtAdapter) Stop(name string) error    { return a.client.Stop(name) }
func (a libvirtAdapter) Delete(name string) error  { return a.client.Delete(name) }
func (a libvirtAdapter) Restart(name string) error { return a.client.Restart(name) }
func (a libvirtAdapter) Pause(name string) error   { return a.client.Pause(name) }
func (a libvirtAdapter) Resume(name string) error  { return a.client.Resume(name) }

// ── Proxmox ───────────────────────────────────────────────────────
//
// The newest backend, and already second only to KubeVirt. Its methods return a
// Task; the contract's callers want completion, so each one waits — a fleet
// surface that fired and forgot would report success for a migration that failed
// thirty seconds later.

type proxmoxAdapter struct{ context string }

func (proxmoxAdapter) Backend() string { return "proxmox" }

func (a proxmoxAdapter) client() (*proxmoxbe.Client, error) {
	return proxmoxbe.ClientForContext(a.context)
}

// wait runs a task-returning operation to completion.
func (a proxmoxAdapter) wait(op func(*proxmoxbe.Client) (proxmoxbe.Task, error)) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	task, err := op(client)
	if err != nil {
		return err
	}
	return client.WaitTask(task, proxmoxbe.DefaultTimeout)
}

func (a proxmoxAdapter) Start(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Start(name) })
}

func (a proxmoxAdapter) Stop(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Stop(name) })
}

func (a proxmoxAdapter) Delete(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Delete(name) })
}

func (a proxmoxAdapter) Restart(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Restart(name) })
}

func (a proxmoxAdapter) Pause(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Pause(name) })
}

func (a proxmoxAdapter) Resume(name string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Resume(name) })
}

func (a proxmoxAdapter) Scale(name string, cores int, mem string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.Scale(name, cores, mem)
}

// HotplugsLive asks PVE rather than guessing: the guest's own config says which
// devices are hotpluggable.
func (a proxmoxAdapter) HotplugsLive(name string) bool {
	client, err := a.client()
	if err != nil {
		return false
	}
	cfg, err := client.GuestConfig(name)
	if err != nil {
		return false
	}
	return cfg.HotplugsCPU() && cfg.HotplugsMemory()
}

func (a proxmoxAdapter) AddDisk(name, size string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	storage, err := firstImageStorage(client)
	if err != nil {
		return err
	}
	return client.AddDisk(name, storage, "", size)
}

func (a proxmoxAdapter) RemoveDisk(name, disk string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	if disk == "" {
		return fmt.Errorf("proxmox: which disk? pass a slot such as scsi1")
	}
	return client.RemoveDisk(name, disk)
}

func (a proxmoxAdapter) ExpandDisk(name, disk, size string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	if disk == "" {
		disk = "scsi0"
	}
	return client.ExpandDisk(name, disk, size)
}

func (a proxmoxAdapter) Migrate(name, target string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) { return c.Migrate(name, target) })
}

func (a proxmoxAdapter) CanMigrate(name string) (bool, string) {
	client, err := a.client()
	if err != nil {
		return false, err.Error()
	}
	pre, err := client.MigratePreconditions(name)
	if err != nil {
		// A container has no precondition endpoint; PVE restarts it on the
		// target, which is a migration Corral can still perform.
		return true, ""
	}
	return pre.CanLiveMigrate()
}

func (a proxmoxAdapter) Clone(source, target string) error {
	return a.wait(func(c *proxmoxbe.Client) (proxmoxbe.Task, error) {
		return c.Clone(source, target, true)
	})
}

func (a proxmoxAdapter) MarkTemplate(name string, on bool) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.MarkTemplate(name, on)
}

func (a proxmoxAdapter) SetTag(name, tag string, on bool) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.SetTag(name, tag, on)
}

func (a proxmoxAdapter) Metrics(name string) (map[string]string, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}
	return client.Metrics(name)
}

func (a proxmoxAdapter) Events(name string) ([]Event, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}
	tasks, err := client.Events(name)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(tasks))
	for _, t := range tasks {
		kind := "Normal"
		if t.Failed() {
			kind = "Warning"
		}
		message := t.ExitStatus
		if message == "" {
			message = t.Status
		}
		out = append(out, Event{
			Time:    strconv.FormatInt(t.StartTime, 10),
			Kind:    kind,
			Reason:  t.Type,
			Object:  t.ID,
			Message: message + " (" + t.User + ")",
		})
	}
	return out, nil
}

func (a proxmoxAdapter) Address(name string) (string, error) {
	client, err := a.client()
	if err != nil {
		return "", err
	}
	return client.Address(name)
}

// firstImageStorage picks a storage that accepts disk images, so AddDisk works
// without the caller knowing the cluster's storage layout.
func firstImageStorage(client *proxmoxbe.Client) (string, error) {
	storages, err := client.Storages()
	if err != nil {
		return "", err
	}
	for _, s := range storages {
		if s.Holds("images") {
			return s.Storage, nil
		}
	}
	return "", fmt.Errorf("proxmox: no storage on this cluster accepts disk images")
}

// backendNames is every backend with an adapter, for error messages that need to
// list them.
func backendNames() string {
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
