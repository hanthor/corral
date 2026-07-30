package snapshot

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func withFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	SetRunner(fake)
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	return fake
}

func libvirtRef(name string) types.InstanceRef {
	return types.InstanceRef{Backend: "libvirt", Context: "qemu:///system", Name: name}
}

func incusRef(name string) types.InstanceRef {
	return types.InstanceRef{Backend: "incus", Context: "lab", Name: name}
}

// ── contract ──────────────────────────────────────────────────────

func TestFor_EveryImplementedBackend(t *testing.T) {
	for _, backend := range []string{"kubevirt", "libvirt", "incus", "qemu"} {
		if _, err := For(types.InstanceRef{Backend: backend, Name: "vm"}); err != nil {
			t.Errorf("no adapter for %q: %v", backend, err)
		}
		if !Supported(backend) {
			t.Errorf("Supported(%q) = false but an adapter exists", backend)
		}
	}
}

// An unknown backend must be refusable without shelling out, and the refusal
// has to be distinguishable from a runtime failure — the UI grays a button on
// one and shows an error on the other.
func TestFor_UnknownBackendIsTypedRefusal(t *testing.T) {
	_, err := For(types.InstanceRef{Backend: "vmware", Name: "vm"})
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if u.Backend != "vmware" {
		t.Errorf("Backend = %q, want vmware", u.Backend)
	}
	if Supported("vmware") {
		t.Error("Supported reported an unimplemented backend")
	}
}

// A reference with no backend can't be dispatched, and it is a caller bug
// rather than a backend limitation — so it must not masquerade as Unsupported.
func TestFor_BackendlessRefIsNotUnsupported(t *testing.T) {
	_, err := For(types.InstanceRef{Name: "vm"})
	if err == nil {
		t.Fatal("want an error for a reference with no backend")
	}
	var u *Unsupported
	if errors.As(err, &u) {
		t.Errorf("a malformed reference reported as a backend limitation: %v", err)
	}
}

// ── retention ─────────────────────────────────────────────────────

// Retention must never touch a snapshot a human took. Someone snapshotting
// before a risky upgrade should not lose it to a schedule they forgot about.
func TestPrune_KeepsManualSnapshots(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/db/snapshots?recursion=1", `[
		{"name":"db/before-upgrade","created_at":"2026-01-01T00:00:00Z"},
		{"name":"db/auto-db-100","created_at":"2026-01-02T00:00:00Z"},
		{"name":"db/auto-db-200","created_at":"2026-01-03T00:00:00Z"},
		{"name":"db/auto-db-300","created_at":"2026-01-04T00:00:00Z"}
	]`, nil)
	fake.AddResponse("incus snapshot delete lab:db auto-db-100", "", nil)

	deleted, err := Prune(incusRef("db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "auto-db-100" {
		t.Fatalf("deleted %v, want only the oldest auto snapshot", deleted)
	}
}

func TestPrune_NothingToDoUnderTheLimit(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/db/snapshots?recursion=1",
		`[{"name":"db/auto-db-100","created_at":"2026-01-02T00:00:00Z"}]`, nil)

	deleted, err := Prune(incusRef("db"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Errorf("pruned %v with fewer snapshots than the limit", deleted)
	}
}

func TestPrune_RejectsAZeroLimit(t *testing.T) {
	withFake(t)
	// keep=0 would delete every scheduled snapshot on the next tick — almost
	// certainly a config mistake, so it is refused rather than obeyed.
	if _, err := Prune(incusRef("db"), 0); err == nil {
		t.Fatal("want an error for keep=0")
	}
}

// Ordering has to survive the timestamp gaining a digit, which name-sorting
// alone gets wrong (auto-db-9 vs auto-db-10).
func TestPrune_OrdersByTimeNotStringLength(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("virsh -c qemu:///system snapshot-list --domain web --name",
		"auto-web-9\nauto-web-10\nauto-web-11\n", nil)
	for _, s := range []string{"auto-web-9", "auto-web-10", "auto-web-11"} {
		fake.AddResponse("virsh -c qemu:///system snapshot-info --domain web --snapshotname "+s, "", errors.New("unsupported"))
	}
	fake.AddResponse("virsh -c qemu:///system snapshot-delete --domain web --snapshotname auto-web-9", "", nil)

	deleted, err := Prune(libvirtRef("web"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "auto-web-9" {
		t.Fatalf("deleted %v, want the numerically oldest (auto-web-9)", deleted)
	}
}

// Retention is scoped by the reference, so two contexts running a same-named
// instance prune independently — the failure this prevents is a schedule in
// one datacentre deleting another's snapshots.
func TestPrune_IsPerContext(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/db/snapshots?recursion=1", `[
		{"name":"db/auto-db-100","created_at":"2026-01-02T00:00:00Z"},
		{"name":"db/auto-db-200","created_at":"2026-01-03T00:00:00Z"}
	]`, nil)
	fake.AddResponse("incus query dr:/1.0/instances/db/snapshots?recursion=1", `[
		{"name":"db/auto-db-500","created_at":"2026-01-05T00:00:00Z"}
	]`, nil)
	fake.AddResponse("incus snapshot delete lab:db auto-db-100", "", nil)

	deleted, err := Prune(incusRef("db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "auto-db-100" {
		t.Fatalf("lab pruned %v, want [auto-db-100]", deleted)
	}
	// The other context is under its limit and must be left alone.
	other := types.InstanceRef{Backend: "incus", Context: "dr", Name: "db"}
	if deleted, err := Prune(other, 1); err != nil || len(deleted) != 0 {
		t.Fatalf("dr pruned %v (err %v); the other context's retention leaked", deleted, err)
	}
}

// A failure partway through has to report what was already deleted, or the
// next run looks like the first.
func TestPrune_ReportsPartialProgressOnFailure(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/db/snapshots?recursion=1", `[
		{"name":"db/auto-db-100","created_at":"2026-01-01T00:00:00Z"},
		{"name":"db/auto-db-200","created_at":"2026-01-02T00:00:00Z"},
		{"name":"db/auto-db-300","created_at":"2026-01-03T00:00:00Z"}
	]`, nil)
	fake.AddResponse("incus snapshot delete lab:db auto-db-100", "", nil)
	fake.AddResponse("incus snapshot delete lab:db auto-db-200", "storage busy", errors.New("exit 1"))

	deleted, err := Prune(incusRef("db"), 1)
	if err == nil {
		t.Fatal("want the delete failure surfaced")
	}
	if len(deleted) != 1 || deleted[0] != "auto-db-100" {
		t.Errorf("deleted = %v, want the one that succeeded before the failure", deleted)
	}
	if !strings.Contains(err.Error(), "storage busy") {
		t.Errorf("error loses the backend's own message: %v", err)
	}
}

// ── libvirt ───────────────────────────────────────────────────────

func TestLibvirt_CreateReportsConsistencyFromDomainState(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  Consistency
	}{
		{"running", Crash},
		{"shut off", Offline},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fake := withFake(t)
			fake.AddResponse("virsh -c qemu:///system domstate web", tc.state, nil)
			fake.AddResponse("virsh -c qemu:///system snapshot-create-as --domain web --name s1 --atomic", "", nil)

			snap, err := (Libvirt{}).Create(libvirtRef("web"), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if snap.Consistency != tc.want {
				t.Errorf("consistency = %q, want %q for a %s domain", snap.Consistency, tc.want, tc.state)
			}
		})
	}
}

func TestLibvirt_DeleteOfAMissingSnapshotIsNotAnError(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("virsh -c qemu:///system snapshot-delete --domain web --snapshotname gone",
		"error: Domain 'web' does not have a snapshot named 'gone'", errors.New("exit 1"))

	if err := (Libvirt{}).Delete(libvirtRef("web"), "gone"); err != nil {
		t.Errorf("deleting an already-gone snapshot failed: %v", err)
	}
}

func TestLibvirt_SurfacesVirshMessage(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("virsh -c qemu:///system domstate web", "", errors.New("exit 1"))
	fake.AddResponse("virsh -c qemu:///system snapshot-create-as --domain web --name s1 --atomic",
		"error: unsupported configuration: internal snapshots require qcow2", errors.New("exit 1"))

	_, err := (Libvirt{}).Create(libvirtRef("web"), "s1")
	if err == nil || !strings.Contains(err.Error(), "require qcow2") {
		t.Errorf("virsh's own explanation was dropped: %v", err)
	}
}

// ── Incus ─────────────────────────────────────────────────────────

func TestIncus_ListStripsTheInstanceQualifier(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/db/snapshots?recursion=1",
		`[{"name":"db/nightly","created_at":"2026-01-02T03:04:05Z"}]`, nil)

	snaps, err := (Incus{}).List(incusRef("db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Name != "nightly" {
		t.Fatalf("got %+v, want the bare snapshot name", snaps)
	}
	if snaps[0].Created != "2026-01-02T03:04:05Z" {
		t.Errorf("created = %q, want Incus's own timestamp", snaps[0].Created)
	}
}

func TestIncus_DefaultsToTheLocalRemote(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query local:/1.0/instances/db/snapshots?recursion=1", `[]`, nil)

	if _, err := (Incus{}).List(types.InstanceRef{Backend: "incus", Name: "db"}); err != nil {
		t.Fatalf("a context-less Incus ref did not fall back to local: %v", err)
	}
}

// ── local QEMU safety ─────────────────────────────────────────────
//
// The issue asks to implement local QEMU "only when storage format/state
// permits". These are the refusals that keeps it honest.

func TestQEMU_RefusesWhileTheVMIsRunning(t *testing.T) {
	home := t.TempDir()
	qemuVMDir(t, home, "dev")
	fake := withFake(t)
	fake.AddPrefixResponse("/fake/bin/qemu-img info", `{"format":"qcow2"}`, nil)
	runningVM(t, "dev")

	_, err := (QEMU{}).Create(types.InstanceRef{Backend: "qemu", Name: "dev"}, "s1")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(u.Remedy, "corral stop dev") {
		t.Errorf("refusal does not tell the operator what to do: %q", u.Remedy)
	}
	for _, call := range fake.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "snapshot -c") {
			t.Fatalf("wrote to a disk the guest has open: %v", call.Args)
		}
	}
}

func TestQEMU_RefusesARawDisk(t *testing.T) {
	home := t.TempDir()
	qemuVMDir(t, home, "dev")
	fake := withFake(t)
	fake.AddPrefixResponse("/fake/bin/qemu-img info", `{"format":"raw"}`, nil)
	stoppedVM(t)

	_, err := (QEMU{}).Create(types.InstanceRef{Backend: "qemu", Name: "dev"}, "s1")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(u.Reason, "qcow2") {
		t.Errorf("refusal does not name the format limitation: %q", u.Reason)
	}
}

// Listing is read-only, so unlike create/restore/delete it stays available
// while the VM runs — the dashboard polls it.
func TestQEMU_ListWorksWhileRunning(t *testing.T) {
	home := t.TempDir()
	qemuVMDir(t, home, "dev")
	fake := withFake(t)
	fake.AddPrefixResponse("/fake/bin/qemu-img info",
		`{"format":"qcow2","snapshots":[{"name":"auto-dev-100","date-sec":100,"date-nsec":0}]}`, nil)
	runningVM(t, "dev")

	snaps, err := (QEMU{}).List(types.InstanceRef{Backend: "qemu", Name: "dev"})
	if err != nil {
		t.Fatalf("listing a running local VM's snapshots failed: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Name != "auto-dev-100" {
		t.Fatalf("got %+v", snaps)
	}
	if snaps[0].Consistency != Offline {
		t.Errorf("consistency = %q; a local snapshot is only ever taken stopped", snaps[0].Consistency)
	}
}

// The local backend has no contexts, so a ref carrying one is a mistake worth
// naming rather than silently ignoring.
func TestQEMU_RefusesAContext(t *testing.T) {
	withFake(t)
	_, err := (QEMU{}).Create(types.InstanceRef{Backend: "qemu", Context: "somewhere", Name: "dev"}, "s1")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
}

func TestAutoName_RoundTripsThroughIsAuto(t *testing.T) {
	name := AutoName("web", 1750000000)
	if !IsAuto(Snapshot{Name: name}) {
		t.Errorf("%q not recognised as a scheduled snapshot", name)
	}
	if IsAuto(Snapshot{Name: "before-upgrade"}) {
		t.Error("a manual snapshot was classified as scheduled")
	}
	if !strings.Contains(name, "web") {
		t.Errorf("%q does not identify its instance", name)
	}
}

// ── helpers ───────────────────────────────────────────────────────

// qemuVMDir lays out the on-disk shape pkg/qemu expects, so the adapter's
// disk lookup resolves without a real VM.
func qemuVMDir(t *testing.T, home, name string) {
	t.Helper()
	dir := fmt.Sprintf("%s/%s", home, name)
	mkdirAll(t, dir)
	writeFile(t, dir+"/disk.qcow2", "not-a-real-image")
	writeFile(t, dir+"/metadata.json", fmt.Sprintf(`{"name":%q,"cpu":2,"memory":"4G"}`, name))
	setQemuHome(t, home)
}
