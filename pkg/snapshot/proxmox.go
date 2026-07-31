package snapshot

// ── Proxmox ───────────────────────────────────────────────────────
//
// PVE's snapshot API behind the shared contract. It is the only backend that can
// give a *running* guest a genuinely consistent capture without a guest agent:
// vmstate=1 writes the guest's RAM into the snapshot, so a rollback resumes
// rather than boots. Corral asks for that whenever the guest is running, and
// reports Filesystem consistency when it got it — a memory-inclusive capture is
// at least as good as a frozen filesystem, and weaker claims would be a lie in
// the other direction.
//
// Containers cannot take a memory snapshot, so a running LXC is honestly
// crash-consistent.

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/proxmoxbe"
	"github.com/tuna-os/corral/pkg/types"
)

// ProxmoxClientFor resolves a client for a reference. It is a package variable
// so the context configuration (host, token, TLS trust) can be supplied by
// whatever owns it, and so tests can point it at an httptest server.
var ProxmoxClientFor = func(ref types.InstanceRef) (*proxmoxbe.Client, error) {
	client, err := proxmoxbe.ClientForContext(ref.Context)
	if err != nil {
		return nil, fmt.Errorf("%w — add the cluster with "+
			"`corral context add <name> --backend proxmox --context <host>`", err)
	}
	return client, nil
}

type Proxmox struct{}

func (p Proxmox) client(ref types.InstanceRef) (*proxmoxbe.Client, error) {
	return ProxmoxClientFor(ref)
}

func (p Proxmox) Create(ref types.InstanceRef, name string) (Snapshot, error) {
	client, err := p.client(ref)
	if err != nil {
		return Snapshot{}, err
	}
	status, err := client.Status(ref.Name)
	if err != nil {
		return Snapshot{}, err
	}
	running := status.Status == "running"

	task, err := client.Snapshot(ref.Name, name, running)
	if err != nil {
		return Snapshot{}, err
	}
	if err := client.WaitTask(task, proxmoxbe.DefaultTimeout); err != nil {
		return Snapshot{}, err
	}

	// Read back rather than assume: the caller is told what PVE actually
	// recorded, including whether the memory made it in.
	snaps, err := client.ListSnapshots(ref.Name)
	if err != nil {
		return Snapshot{}, err
	}
	for _, s := range snaps {
		if name == "" || s.Name == name {
			if name == "" && s.Parent != "" {
				// Without a requested name, the newest is the one whose parent
				// chain ends the list; ListSnapshots preserves PVE's order.
				continue
			}
			return p.toSnapshot(ref, s), nil
		}
	}
	return Snapshot{Name: name, Source: ref.Name, Consistency: p.consistency(running, false)}, nil
}

func (p Proxmox) List(ref types.InstanceRef) ([]Snapshot, error) {
	client, err := p.client(ref)
	if err != nil {
		return nil, err
	}
	snaps, err := client.ListSnapshots(ref.Name)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, p.toSnapshot(ref, s))
	}
	sortNewestFirst(out)
	return out, nil
}

func (p Proxmox) Restore(ref types.InstanceRef, name string) error {
	client, err := p.client(ref)
	if err != nil {
		return err
	}
	task, err := client.RestoreSnapshot(ref.Name, name)
	if err != nil {
		return err
	}
	return client.WaitTask(task, proxmoxbe.DefaultTimeout)
}

func (p Proxmox) Delete(ref types.InstanceRef, name string) error {
	client, err := p.client(ref)
	if err != nil {
		return err
	}
	task, err := client.DeleteSnapshot(ref.Name, name)
	if err != nil {
		return err
	}
	return client.WaitTask(task, proxmoxbe.DefaultTimeout)
}

func (p Proxmox) toSnapshot(ref types.InstanceRef, s proxmoxbe.SnapshotInfo) Snapshot {
	return Snapshot{
		Name:    s.Name,
		Source:  ref.Name,
		Created: s.Created(),
		// PVE snapshots are usable as soon as the task completes; there is no
		// separate readiness condition to wait on the way KubeVirt has.
		Ready:       true,
		Consistency: p.consistency(s.Running == 1, s.WithMemory()),
	}
}

// consistency reports what the capture actually caught.
func (p Proxmox) consistency(running, withMemory bool) Consistency {
	switch {
	case !running:
		return Offline
	case withMemory:
		return Filesystem
	default:
		return Crash
	}
}
