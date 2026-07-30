//go:build e2esnapshot

// Real-daemon verification of the snapshot adapters (#134). Unit tests pin the
// contract against fake runners; these run the same code against an actual
// qemu-img, an actual libvirt domain, and an actual Incus instance, because
// the part most likely to be wrong is the exact command syntax and output
// shape of each tool — which a fake can only assert is unchanged, not correct.
//
// Run with: go test -tags e2esnapshot ./pkg/snapshot/
// Each test skips when its backend is absent, so the file is safe to run
// anywhere; CI installs all three (.github/workflows/e2e-incus.yml).

package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed", name)
	}
}

// realRunner puts the adapters back on the actual shell for the duration of
// one test — withFake is the default everywhere else in this package.
func realRunner(t *testing.T) {
	t.Helper()
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
}

// ── local QEMU ────────────────────────────────────────────────────

func TestE2E_QEMU(t *testing.T) {
	requireBinary(t, "qemu-img")
	realRunner(t)

	home := t.TempDir()
	dir := filepath.Join(home, "snape2e")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk.qcow2")
	mustRun(t, "qemu-img", "create", "-f", "qcow2", disk, "64M")
	writeFile(t, filepath.Join(dir, "metadata.json"), `{"name":"snape2e","cpu":1,"memory":"1G"}`)
	setQemuHome(t, home)
	stoppedVM(t)

	ref := types.InstanceRef{Backend: "qemu", Name: "snape2e"}
	assertSnapshotLifecycle(t, QEMU{}, ref, Offline)

	// The safety refusal, against the same real image: qemu-img writing into a
	// disk a live guest has open corrupts it, so a running VM is refused.
	runningVM(t, "snape2e")
	if _, err := (QEMU{}).Create(ref, "while-running"); err == nil {
		t.Error("snapshotted a running VM's disk")
	} else {
		t.Logf("running VM refused: %v", err)
	}
}

// A raw disk cannot hold internal snapshots at all, and the adapter has to say
// so rather than let qemu-img fail obscurely.
func TestE2E_QEMU_RefusesRealRawDisk(t *testing.T) {
	requireBinary(t, "qemu-img")
	realRunner(t)

	home := t.TempDir()
	dir := filepath.Join(home, "rawvm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Named .qcow2 because that is where the adapter looks, but genuinely raw
	// — exactly the mismatch the format check exists to catch.
	mustRun(t, "qemu-img", "create", "-f", "raw", filepath.Join(dir, "disk.qcow2"), "16M")
	writeFile(t, filepath.Join(dir, "metadata.json"), `{"name":"rawvm","cpu":1,"memory":"1G"}`)
	setQemuHome(t, home)
	stoppedVM(t)

	_, err := (QEMU{}).Create(types.InstanceRef{Backend: "qemu", Name: "rawvm"}, "s1")
	if err == nil {
		t.Fatal("a raw disk accepted an internal snapshot")
	}
	if !strings.Contains(err.Error(), "qcow2") {
		t.Errorf("refusal does not name the format limitation: %v", err)
	}
	t.Logf("raw disk refused: %v", err)
}

// ── libvirt ───────────────────────────────────────────────────────

func TestE2E_Libvirt(t *testing.T) {
	requireBinary(t, "virsh")
	realRunner(t)
	requireBinary(t, "qemu-img")

	// qemu:///session is per-user: no sudo, no shared state with the host's
	// system domains, so the test cannot disturb anything real.
	const uri = "qemu:///session"
	if err := exec.Command("virsh", "-c", uri, "connect").Run(); err != nil {
		t.Skipf("no usable %s: %v", uri, err)
	}

	dir := t.TempDir()
	disk := filepath.Join(dir, "e2e.qcow2")
	mustRun(t, "qemu-img", "create", "-f", "qcow2", disk, "64M")

	const domain = "corral-snap-e2e"
	xml := fmt.Sprintf(`<domain type='qemu'>
  <name>%s</name>
  <memory unit='MiB'>128</memory>
  <vcpu>1</vcpu>
  <os><type arch='x86_64'>hvm</type></os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
</domain>`, domain, disk)

	xmlPath := filepath.Join(dir, "domain.xml")
	writeFile(t, xmlPath, xml)
	if out, err := exec.Command("virsh", "-c", uri, "define", xmlPath).CombinedOutput(); err != nil {
		t.Skipf("cannot define a session domain here: %s: %v", out, err)
	}
	t.Cleanup(func() {
		exec.Command("virsh", "-c", uri, "undefine", domain, "--snapshots-metadata").Run()
	})

	ref := types.InstanceRef{Backend: "libvirt", Context: uri, Name: domain}
	// The domain is defined but never started, so libvirt captures it offline.
	assertSnapshotLifecycle(t, Libvirt{}, ref, Offline)
}

// ── Incus ─────────────────────────────────────────────────────────

func TestE2E_Incus(t *testing.T) {
	requireBinary(t, "incus")
	realRunner(t)

	if err := exec.Command("incus", "query", "local:/1.0").Run(); err != nil {
		t.Skipf("no reachable local Incus remote: %v", err)
	}

	const instance = "corral-snap-e2e"
	if out, err := exec.Command("incus", "launch", "images:ubuntu/22.04", instance).CombinedOutput(); err != nil {
		t.Skipf("cannot launch an Incus instance here: %s: %v", out, err)
	}
	t.Cleanup(func() {
		exec.Command("incus", "delete", "--force", instance).Run()
	})

	ref := types.InstanceRef{Backend: "incus", Context: "local", Name: instance}
	// Launched, so running: Incus snapshots are stateless by default, which is
	// crash consistency and must be reported as such rather than overclaimed.
	assertSnapshotLifecycle(t, Incus{}, ref, Crash)
}

// ── shared lifecycle assertion ────────────────────────────────────

// assertSnapshotLifecycle drives one adapter through the whole contract
// against a real backend: create (twice scheduled, once manual), list, restore,
// retention, and an idempotent delete. Every backend has to behave the same
// way here — that identical behaviour is what the abstraction is claiming.
func assertSnapshotLifecycle(t *testing.T, adapter Adapter, ref types.InstanceRef, want Consistency) {
	t.Helper()

	first, err := adapter.Create(ref, AutoName(ref.Name, 100))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.Consistency != want {
		t.Errorf("consistency = %q, want %q", first.Consistency, want)
	}
	t.Logf("created %s (%s)", first.Name, first.Consistency)

	if _, err := adapter.Create(ref, AutoName(ref.Name, 200)); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if _, err := adapter.Create(ref, "before-upgrade"); err != nil {
		t.Fatalf("manual Create: %v", err)
	}

	snaps, err := adapter.List(ref)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("listed %d: %+v", len(snaps), snaps)
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(snaps))
	}

	if err := adapter.Restore(ref, AutoName(ref.Name, 100)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	deleted, err := Prune(ref, 1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != AutoName(ref.Name, 100) {
		t.Fatalf("pruned %v, want only the oldest scheduled snapshot", deleted)
	}

	after, err := adapter.List(ref)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, s := range after {
		remaining[s.Name] = true
	}
	if !remaining["before-upgrade"] {
		t.Error("retention deleted a manual snapshot")
	}
	if !remaining[AutoName(ref.Name, 200)] {
		t.Error("retention deleted the newest scheduled snapshot")
	}
	if remaining[AutoName(ref.Name, 100)] {
		t.Error("the pruned snapshot is still there")
	}

	// Deleting what retention already removed must be a no-op: a schedule
	// racing a human clearing up should not fail the run.
	if err := adapter.Delete(ref, AutoName(ref.Name, 100)); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %s: %v", name, strings.Join(args, " "), out, err)
	}
}
