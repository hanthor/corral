package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// runner is the seam for the adapters that shell out (libvirt, Incus, local
// QEMU). KubeVirt goes through its own client, which has its own seam.
var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) { runner = r }

// ── KubeVirt ──────────────────────────────────────────────────────
//
// The original implementation, behind the shared contract. KubeVirt is the
// only backend that quiesces the guest for us: with the guest agent connected
// it freezes the filesystem, so a running instance still snapshots
// consistently.

type KubeVirt struct{}

func (KubeVirt) client(ref types.InstanceRef) *kubevirt.Client {
	return kubevirt.NewClientForContext(ref.Namespace, ref.Context)
}

func (k KubeVirt) Create(ref types.InstanceRef, name string) (Snapshot, error) {
	created, err := k.client(ref).Snapshot(ref.Name, name)
	if err != nil {
		return Snapshot{}, err
	}
	// The freeze happens asynchronously inside KubeVirt, and readiness is
	// reported on the resource — so this is what was requested, not yet what
	// has completed. List reflects the settled state.
	return Snapshot{Name: created, Source: ref.Name, Consistency: Filesystem}, nil
}

func (k KubeVirt) List(ref types.InstanceRef) ([]Snapshot, error) {
	infos, err := k.client(ref).ListSnapshots(ref.Name)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(infos))
	for _, i := range infos {
		out = append(out, Snapshot{
			Name: i.Name, Source: i.Source, Created: i.Created,
			Ready: i.Ready, Consistency: Filesystem,
		})
	}
	sortNewestFirst(out)
	return out, nil
}

func (k KubeVirt) Restore(ref types.InstanceRef, name string) error {
	return k.client(ref).RestoreSnapshot(ref.Name, name)
}

func (k KubeVirt) Delete(ref types.InstanceRef, name string) error {
	return k.client(ref).DeleteSnapshot(name)
}

// ── libvirt ───────────────────────────────────────────────────────
//
// virsh snapshot-* against the domain. --atomic so a failure can't leave the
// domain half-snapshotted. A running domain is captured as-is: libvirt can
// quiesce through the guest agent, but only when one is installed and
// responding, so claiming filesystem consistency unconditionally would be a
// lie. Running is reported as crash-consistent.

type Libvirt struct{}

func (Libvirt) virsh(ref types.InstanceRef, args ...string) ([]byte, error) {
	uri := ref.Context
	if uri == "" {
		uri = "qemu:///system"
	}
	return runner.Run("virsh", append([]string{"-c", uri}, args...)...)
}

func (l Libvirt) running(ref types.InstanceRef) bool {
	out, err := l.virsh(ref, "domstate", ref.Name)
	return err == nil && strings.Contains(strings.ToLower(string(out)), "running")
}

func (l Libvirt) Create(ref types.InstanceRef, name string) (Snapshot, error) {
	if name == "" {
		name = AutoName(ref.Name, time.Now().Unix())
	}
	consistency := Offline
	if l.running(ref) {
		consistency = Crash
	}
	out, err := l.virsh(ref, "snapshot-create-as", "--domain", ref.Name, "--name", name, "--atomic")
	if err != nil {
		return Snapshot{}, fmt.Errorf("virsh snapshot-create-as: %s", commandError(out, err))
	}
	return Snapshot{
		Name: name, Source: ref.Name, Ready: true,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Consistency: consistency,
	}, nil
}

func (l Libvirt) List(ref types.InstanceRef) ([]Snapshot, error) {
	// --name prints one snapshot name per line, which is stable across virsh
	// versions in a way the table format is not.
	out, err := l.virsh(ref, "snapshot-list", "--domain", ref.Name, "--name")
	if err != nil {
		return nil, fmt.Errorf("virsh snapshot-list: %s", commandError(out, err))
	}
	var snaps []Snapshot
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		snaps = append(snaps, Snapshot{
			Name: name, Source: ref.Name, Ready: true,
			Created:     l.creationTime(ref, name),
			Consistency: Crash,
		})
	}
	sortNewestFirst(snaps)
	return snaps, nil
}

// creationTime reads one snapshot's timestamp. Empty when virsh won't say —
// Prune falls back to name ordering, and auto names carry their own timestamp.
func (l Libvirt) creationTime(ref types.InstanceRef, name string) string {
	out, err := l.virsh(ref, "snapshot-info", "--domain", ref.Name, "--snapshotname", name)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), "Creation time") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (l Libvirt) Restore(ref types.InstanceRef, name string) error {
	out, err := l.virsh(ref, "snapshot-revert", "--domain", ref.Name, "--snapshotname", name)
	if err != nil {
		return fmt.Errorf("virsh snapshot-revert: %s", commandError(out, err))
	}
	return nil
}

func (l Libvirt) Delete(ref types.InstanceRef, name string) error {
	out, err := l.virsh(ref, "snapshot-delete", "--domain", ref.Name, "--snapshotname", name)
	if err != nil {
		if isMissing(out, "does not have a snapshot", "no domain snapshot") {
			return nil
		}
		return fmt.Errorf("virsh snapshot-delete: %s", commandError(out, err))
	}
	return nil
}

// ── Incus ─────────────────────────────────────────────────────────
//
// `incus snapshot` on the instance. Stateless by default: a running instance's
// memory is not captured, which is exactly crash consistency. Incus reports
// creation timestamps, so retention ordering is real rather than name-derived.

type Incus struct{}

func (Incus) target(ref types.InstanceRef) string {
	remote := ref.Context
	if remote == "" {
		remote = "local"
	}
	return remote + ":" + ref.Name
}

func (i Incus) Create(ref types.InstanceRef, name string) (Snapshot, error) {
	if name == "" {
		name = AutoName(ref.Name, time.Now().Unix())
	}
	out, err := runner.Run("incus", "snapshot", "create", i.target(ref), name)
	if err != nil {
		return Snapshot{}, fmt.Errorf("incus snapshot create: %s", commandError(out, err))
	}
	consistency := Offline
	if i.running(ref) {
		consistency = Crash
	}
	return Snapshot{
		Name: name, Source: ref.Name, Ready: true,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Consistency: consistency,
	}, nil
}

func (i Incus) running(ref types.InstanceRef) bool {
	remote := ref.Context
	if remote == "" {
		remote = "local"
	}
	instances, err := incus.NewClient(remote).ListInstances()
	if err != nil {
		return false
	}
	for _, inst := range instances {
		if inst.Name == ref.Name {
			return strings.EqualFold(inst.Status, "running")
		}
	}
	return false
}

func (i Incus) List(ref types.InstanceRef) ([]Snapshot, error) {
	out, err := runner.Run("incus", "query", i.queryPath(ref))
	if err != nil {
		return nil, fmt.Errorf("incus query snapshots: %s", commandError(out, err))
	}
	var raw []struct {
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("unreadable snapshot listing: %w", err)
	}
	var snaps []Snapshot
	for _, s := range raw {
		// Incus reports snapshots fully qualified as "instance/snapshot".
		name := s.Name
		if _, short, found := strings.Cut(name, "/"); found {
			name = short
		}
		snaps = append(snaps, Snapshot{
			Name: name, Source: ref.Name, Ready: true,
			Created: s.CreatedAt, Consistency: Crash,
		})
	}
	sortNewestFirst(snaps)
	return snaps, nil
}

func (i Incus) queryPath(ref types.InstanceRef) string {
	remote := ref.Context
	if remote == "" {
		remote = "local"
	}
	return fmt.Sprintf("%s:/1.0/instances/%s/snapshots?recursion=1", remote, ref.Name)
}

func (i Incus) Restore(ref types.InstanceRef, name string) error {
	out, err := runner.Run("incus", "snapshot", "restore", i.target(ref), name)
	if err != nil {
		return fmt.Errorf("incus snapshot restore: %s", commandError(out, err))
	}
	return nil
}

func (i Incus) Delete(ref types.InstanceRef, name string) error {
	out, err := runner.Run("incus", "snapshot", "delete", i.target(ref), name)
	if err != nil {
		if isMissing(out, "not found", "no such object") {
			return nil
		}
		return fmt.Errorf("incus snapshot delete: %s", commandError(out, err))
	}
	return nil
}

// ── local QEMU ────────────────────────────────────────────────────
//
// qcow2 internal snapshots via qemu-img. The issue asks to "assess local QEMU
// safety and implement it only when storage format/state permits", and there
// are two hard limits:
//
//   - qemu-img must not touch a disk a running QEMU has open. It writes
//     through the same image the guest is writing to, and the result is a
//     corrupt image, not a bad snapshot. So a running VM is refused outright
//     rather than snapshotted badly. (Live snapshots are possible over QMP,
//     which is a different mechanism and out of scope here.)
//   - Only qcow2 carries internal snapshots. A raw disk cannot, at any cost.
//
// Both refusals come back as *Unsupported with the fix, because both are
// things the operator can resolve.

type QEMU struct{}

func (QEMU) diskPath(ref types.InstanceRef) string {
	return filepath.Join(qemu.VMHome(), ref.Name, "disk.qcow2")
}

// precheck enforces the two safety limits and returns the disk to operate on.
func (q QEMU) precheck(ref types.InstanceRef, operation string) (string, error) {
	if ref.Context != "" {
		return "", unsupported("qemu", "the local backend has no contexts", "drop --context for local VMs")
	}
	disk := q.diskPath(ref)
	if _, err := os.Stat(disk); err != nil {
		return "", fmt.Errorf("local VM %q has no qcow2 disk at %s: %w", ref.Name, disk, err)
	}
	if format, err := q.diskFormat(disk); err == nil && format != "qcow2" {
		return "", unsupported("qemu",
			fmt.Sprintf("%s is a %s image and only qcow2 carries internal snapshots", disk, format),
			"recreate the VM from a qcow2 image")
	}
	if q.running(ref) {
		return "", unsupported("qemu",
			fmt.Sprintf("%s is running, and writing a snapshot into a disk the guest has open corrupts it", ref.Name),
			fmt.Sprintf("stop it first: corral stop %s", ref.Name))
	}
	_ = operation
	return disk, nil
}

func (QEMU) running(ref types.InstanceRef) bool {
	vms, err := qemu.List()
	if err != nil {
		return false
	}
	for _, vm := range vms {
		if vm.Name == ref.Name {
			return vm.Running
		}
	}
	return false
}

func (q QEMU) diskFormat(disk string) (string, error) {
	out, err := q.qemuImg("info", "--output=json", disk)
	if err != nil {
		return "", err
	}
	var info struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", err
	}
	return info.Format, nil
}

// qemuImg resolves qemu-img the same way pkg/qemu does, so a brew-only host
// (no qemu-img on the service PATH) behaves identically here.
func (QEMU) qemuImg(args ...string) ([]byte, error) {
	bin, err := runner.LookPath("qemu-img")
	if err != nil {
		// Not on PATH: try where a brew or distro install actually puts it,
		// the same locations pkg/qemu falls back to. A systemd user unit's
		// PATH routinely lacks brew.
		bin = "qemu-img"
		for _, base := range []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin", "/usr/local/bin"} {
			candidate := filepath.Join(base, "qemu-img")
			if _, err := os.Stat(candidate); err == nil {
				bin = candidate
				break
			}
		}
	}
	return runner.Run(bin, args...)
}

func (q QEMU) Create(ref types.InstanceRef, name string) (Snapshot, error) {
	disk, err := q.precheck(ref, "create")
	if err != nil {
		return Snapshot{}, err
	}
	if name == "" {
		name = AutoName(ref.Name, time.Now().Unix())
	}
	if out, err := q.qemuImg("snapshot", "-c", name, disk); err != nil {
		return Snapshot{}, fmt.Errorf("qemu-img snapshot -c: %s", commandError(out, err))
	}
	// The VM was verified stopped above, so this is a clean, fully consistent
	// image — the strongest guarantee of any backend here.
	return Snapshot{
		Name: name, Source: ref.Name, Ready: true,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Consistency: Offline,
	}, nil
}

func (q QEMU) List(ref types.InstanceRef) ([]Snapshot, error) {
	disk := q.diskPath(ref)
	if _, err := os.Stat(disk); err != nil {
		return nil, fmt.Errorf("local VM %q has no qcow2 disk at %s: %w", ref.Name, disk, err)
	}
	// Listing is read-only, so unlike the mutating paths it is safe while the
	// VM runs — the dashboard polls this.
	out, err := q.qemuImg("info", "--output=json", disk)
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %s", commandError(out, err))
	}
	var info struct {
		Snapshots []struct {
			Name        string `json:"name"`
			DateSec     int64  `json:"date-sec"`
			DateNanoSec int64  `json:"date-nsec"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("unreadable qemu-img output: %w", err)
	}
	var snaps []Snapshot
	for _, s := range info.Snapshots {
		snaps = append(snaps, Snapshot{
			Name: s.Name, Source: ref.Name, Ready: true,
			Created:     time.Unix(s.DateSec, s.DateNanoSec).UTC().Format(time.RFC3339),
			Consistency: Offline,
		})
	}
	sortNewestFirst(snaps)
	return snaps, nil
}

func (q QEMU) Restore(ref types.InstanceRef, name string) error {
	disk, err := q.precheck(ref, "restore")
	if err != nil {
		return err
	}
	if out, err := q.qemuImg("snapshot", "-a", name, disk); err != nil {
		return fmt.Errorf("qemu-img snapshot -a: %s", commandError(out, err))
	}
	return nil
}

func (q QEMU) Delete(ref types.InstanceRef, name string) error {
	disk, err := q.precheck(ref, "delete")
	if err != nil {
		return err
	}
	if out, err := q.qemuImg("snapshot", "-d", name, disk); err != nil {
		if isMissing(out, "can't find snapshot", "snapshot not found") {
			return nil
		}
		return fmt.Errorf("qemu-img snapshot -d: %s", commandError(out, err))
	}
	return nil
}

// ── shared helpers ────────────────────────────────────────────────

// commandError prefers the tool's own message: "Domain not found" beats
// "exit status 1" for someone reading a scheduler log.
func commandError(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}

// isMissing reports whether a delete failed only because the target was
// already gone. Retention races with manual cleanup, and a job that fails
// because someone beat it to a deletion is noise.
func isMissing(out []byte, needles ...string) bool {
	msg := strings.ToLower(string(out))
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}
