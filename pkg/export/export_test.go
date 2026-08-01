package export

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

func withFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	SetRunner(fake)
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	return fake
}

// localVM lays out a stopped local VM with a disk, and returns its ref.
func localVM(t *testing.T, name string) types.InstanceRef {
	t.Helper()
	home, units := t.TempDir(), t.TempDir()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"),
		[]byte(`{"name":"`+name+`","cpu":1,"memory":"1G"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	qemu.SetStateDirs(home, units)
	qemu.SetSystemctl(func(...string) ([]byte, error) { return []byte("inactive\n"), nil })
	t.Cleanup(func() {
		qemu.SetStateDirs("", "")
		qemu.SetSystemctl(nil)
	})
	return types.InstanceRef{Backend: "qemu", Name: name}
}

// ── contract ──────────────────────────────────────────────────────

func TestFor_EveryImplementedBackend(t *testing.T) {
	for _, backend := range []string{"kubevirt", "qemu", "libvirt", "incus"} {
		if _, err := For(types.InstanceRef{Backend: backend, Name: "vm"}); err != nil {
			t.Errorf("no adapter for %q: %v", backend, err)
		}
		if !Supported(backend) {
			t.Errorf("Supported(%q) = false but an adapter exists", backend)
		}
		if len(Formats(backend)) == 0 {
			t.Errorf("%s advertises no formats", backend)
		}
	}
	if Supported("vmware") {
		t.Error("Supported reported an unimplemented backend")
	}
}

// An Incus instance archive is not a disk image, so the native format stays
// incus-tar: it is the right artifact for a backup, carrying the configuration
// and every volume. qcow2 is offered second, because without it an Incus VM
// cannot be moved to another backend at all — but it is a narrower thing (the
// boot disk only), and which one a caller gets should not be an accident.
func TestFormats_IncusOffersTheArchiveFirstAndADiskSecond(t *testing.T) {
	formats := Formats("incus")
	if len(formats) != 2 || formats[0] != IncusTar || formats[1] != Qcow2 {
		t.Fatalf("incus formats = %v, want [%v %v]", formats, IncusTar, Qcow2)
	}
	native, err := pickFormat("incus", "", formats)
	if err != nil || native != IncusTar {
		t.Errorf("an unspecified format should stay the archive, got %q (%v)", native, err)
	}
	if _, err := pickFormat("incus", RawGz, formats); err == nil {
		t.Error("incus accepted raw.gz, which it cannot produce")
	}
}

func TestPickFormat_EmptyMeansNative(t *testing.T) {
	got, err := pickFormat("qemu", "", []Format{Qcow2, RawGz})
	if err != nil {
		t.Fatal(err)
	}
	if got != Qcow2 {
		t.Errorf("format = %q, want the backend's native choice", got)
	}
}

// ── local QEMU ────────────────────────────────────────────────────

func TestQEMU_RefusesWhileRunning(t *testing.T) {
	fake := withFake(t)
	ref := localVM(t, "dev")
	qemu.SetSystemctl(func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "is-active" {
			return []byte("active\n"), nil
		}
		return []byte(""), nil
	})

	_, err := (QEMU{}).Export(context.Background(),
		Request{Ref: ref, Dest: filepath.Join(t.TempDir(), "out.qcow2")}, nil)
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(u.Remedy, "corral stop dev") {
		t.Errorf("refusal does not say what to do: %q", u.Remedy)
	}
	for _, call := range fake.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "convert") {
			t.Fatalf("read a disk the guest is writing to: %v", call.Args)
		}
	}
}

// Cancellation has to be honoured before the expensive part starts — a
// multi-gigabyte copy that can't be stopped is a bug.
func TestQEMU_HonoursCancellation(t *testing.T) {
	fake := withFake(t)
	ref := localVM(t, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (QEMU{}).Export(ctx, Request{Ref: ref, Dest: filepath.Join(t.TempDir(), "out.qcow2")}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	for _, call := range fake.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "convert") {
			t.Fatalf("started converting despite a cancelled context: %v", call.Args)
		}
	}
}

// A backend that exits 0 without producing anything must not be reported as a
// successful backup — that is how someone discovers an empty archive at
// restore time.
func TestExport_EmptyOutputIsAFailure(t *testing.T) {
	fake := withFake(t)
	ref := localVM(t, "dev")
	dest := filepath.Join(t.TempDir(), "out.qcow2")
	fake.AddPrefixResponse("/fake/bin/qemu-img", "", nil) // succeeds, writes nothing

	if _, err := (QEMU{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil); err == nil {
		t.Fatal("an export that produced no file reported success")
	}
}

func TestQEMU_ReportsProgressWithATotal(t *testing.T) {
	fake := withFake(t)
	ref := localVM(t, "dev")
	dest := filepath.Join(t.TempDir(), "out.qcow2")
	fake.AddPrefixResponse("/fake/bin/qemu-img info", `{"virtual-size":1073741824}`, nil)
	fake.AddPrefixResponse("/fake/bin/qemu-img convert", "", nil)

	var seen []Progress
	// The conversion writes nothing (fake runner), so this errors at the
	// output check — the progress reported before that is what's under test.
	_, _ = (QEMU{}).Export(context.Background(), Request{Ref: ref, Dest: dest},
		func(p Progress) { seen = append(seen, p) })

	if len(seen) == 0 {
		t.Fatal("no progress reported")
	}
	if seen[0].Total != 1073741824 {
		t.Errorf("total = %d, want the disk's virtual size", seen[0].Total)
	}
	if seen[0].Stage == "" {
		t.Error("progress carries no stage name")
	}
}

// ── libvirt ───────────────────────────────────────────────────────

// A remote hypervisor's disk files are not on this filesystem. Attempting the
// copy anyway produces a baffling "no such file" from qemu-img.
func TestLibvirt_RefusesARemoteHypervisor(t *testing.T) {
	withFake(t)
	ref := types.InstanceRef{Backend: "libvirt", Context: "qemu+ssh://hv/system", Name: "web"}

	_, err := (Libvirt{}).Export(context.Background(),
		Request{Ref: ref, Dest: filepath.Join(t.TempDir(), "out.qcow2")}, nil)
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(u.Reason, "remote") {
		t.Errorf("refusal does not explain why: %q", u.Reason)
	}
}

func TestIsRemoteURI(t *testing.T) {
	for uri, want := range map[string]bool{
		"":                      false,
		"qemu:///system":        false,
		"qemu:///session":       false,
		"qemu+ssh://hv/system":  true,
		"qemu+tcp://hv/system":  true,
		"qemu://hypervisor/sys": true,
	} {
		if got := isRemoteURI(uri); got != want {
			t.Errorf("isRemoteURI(%q) = %v, want %v", uri, got, want)
		}
	}
}

// An export from a running domain is crash-consistent and has to say so —
// the issue asks for the fallback to be disclosed, not hidden.
func TestLibvirt_DisclosesCrashConsistency(t *testing.T) {
	fake := withFake(t)
	dir := t.TempDir()
	disk := filepath.Join(dir, "web.qcow2")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.qcow2")

	ref := types.InstanceRef{Backend: "libvirt", Context: "qemu:///system", Name: "web"}
	fake.AddResponse("virsh -c qemu:///system domblklist web --details",
		"Type   Device Target Source\n-----------------------------------\nfile   disk   vda    "+disk+"\n", nil)
	fake.AddResponse("virsh -c qemu:///system domstate web", "running", nil)
	fake.AddPrefixResponse("/fake/bin/qemu-img info", `{"virtual-size":100}`, nil)
	// Produce a real output file so finish() succeeds.
	fake.AddPrefixResponse("/fake/bin/qemu-img convert", "", nil)
	if err := os.WriteFile(dest, []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (Libvirt{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consistency != snapshot.Crash {
		t.Errorf("consistency = %q, want crash for a running domain", result.Consistency)
	}
}

func TestLibvirt_StoppedDomainIsOfflineConsistent(t *testing.T) {
	fake := withFake(t)
	dir := t.TempDir()
	disk := filepath.Join(dir, "web.qcow2")
	os.WriteFile(disk, []byte("disk"), 0o644)
	dest := filepath.Join(dir, "out.qcow2")
	os.WriteFile(dest, []byte("converted"), 0o644)

	ref := types.InstanceRef{Backend: "libvirt", Context: "qemu:///system", Name: "web"}
	fake.AddResponse("virsh -c qemu:///system domblklist web --details",
		"Type   Device Target Source\n---\nfile   disk   vda    "+disk+"\n", nil)
	fake.AddResponse("virsh -c qemu:///system domstate web", "shut off", nil)
	fake.AddPrefixResponse("/fake/bin/qemu-img", "", nil)

	result, err := (Libvirt{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consistency != snapshot.Offline {
		t.Errorf("consistency = %q, want offline for a stopped domain", result.Consistency)
	}
}

// A network- or volume-backed disk can't be copied off the filesystem, and the
// refusal should say that rather than silently exporting nothing.
func TestLibvirt_RefusesANonFileDisk(t *testing.T) {
	fake := withFake(t)
	ref := types.InstanceRef{Backend: "libvirt", Context: "qemu:///system", Name: "web"}
	fake.AddResponse("virsh -c qemu:///system domblklist web --details",
		"Type     Device Target Source\n---\nnetwork  disk   vda    rbd:pool/img\n", nil)

	_, err := (Libvirt{}).Export(context.Background(),
		Request{Ref: ref, Dest: filepath.Join(t.TempDir(), "out.qcow2")}, nil)
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
}

// ── Incus ─────────────────────────────────────────────────────────

func TestIncus_ExportsToTheNamedRemote(t *testing.T) {
	fake := withFake(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "dev.tar.gz")
	os.WriteFile(dest, []byte("archive"), 0o644)

	fake.AddResponse("incus export lab:dev "+dest+" --instance-only", "", nil)
	fake.AddPrefixResponse("incus list", "[]", nil)

	ref := types.InstanceRef{Backend: "incus", Context: "lab", Name: "dev"}
	result, err := (Incus{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != IncusTar {
		t.Errorf("format = %q, want %q", result.Format, IncusTar)
	}
	if result.Bytes == 0 {
		t.Error("result reports no size")
	}
}

// ── Incus as a disk source (ADR-0010) ─────────────────────────────

// writeTar builds an instance archive holding the named members.
func writeTar(t *testing.T, path string, members map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := tar.NewWriter(f)
	for name, body := range members {
		if err := w.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractInstanceDisk_PullsTheVMImage(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "inst.tar")
	writeTar(t, archive, map[string]string{
		"backup/index.yaml":          "name: dev\n",
		"backup/virtual-machine.img": "DISKBYTES",
	})

	dest := filepath.Join(dir, "disk.img")
	if err := extractInstanceDisk(archive, dest); err != nil {
		t.Fatalf("extractInstanceDisk: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DISKBYTES" {
		t.Fatalf("extracted %q, want the disk's contents", got)
	}
}

// Incus renamed this member between versions; matching on the basename means a
// third spelling is a one-line change rather than a silent failure.
func TestExtractInstanceDisk_AcceptsTheOlderName(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "inst.tar")
	writeTar(t, archive, map[string]string{"backup/root.img": "OLD"})

	dest := filepath.Join(dir, "disk.img")
	if err := extractInstanceDisk(archive, dest); err != nil {
		t.Fatalf("extractInstanceDisk: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "OLD" {
		t.Fatalf("extracted %q", got)
	}
}

// A container archive has no disk, and saying "this is a container" is the
// difference between an actionable message and a baffling one.
func TestExtractInstanceDisk_ExplainsAContainer(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "ct.tar")
	writeTar(t, archive, map[string]string{
		"backup/index.yaml":                    "name: ct\n",
		"backup/container/rootfs/etc/hostname": "ct\n",
	})

	err := extractInstanceDisk(archive, filepath.Join(dir, "disk.img"))
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want a typed refusal, got %T: %v", err, err)
	}
	if !strings.Contains(u.Reason, "container") {
		t.Errorf("refusal does not name the cause: %q", u.Reason)
	}
	if u.Remedy == "" {
		t.Error("refusal offers no way forward")
	}
}

func TestIncus_ExportsAQcow2Disk(t *testing.T) {
	fake := withFake(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "dev.qcow2")
	archive := dest + ".tar"

	// The fake runner does not write files, so the archive `incus export` would
	// have produced and the qcow2 `qemu-img` would have written are staged here.
	writeTar(t, archive, map[string]string{"backup/virtual-machine.img": "RAWDISK"})
	if err := os.WriteFile(dest, []byte("qcow2"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.AddPrefixResponse("incus export", "", nil)
	fake.AddPrefixResponse("incus list", "[]", nil)
	fake.AddPrefixResponse("/fake/bin/qemu-img info", `{"virtual-size":42}`, nil)
	fake.AddPrefixResponse("/fake/bin/qemu-img convert", "", nil)

	ref := types.InstanceRef{Backend: "incus", Context: "lab", Name: "dev"}
	result, err := (Incus{}).Export(context.Background(),
		Request{Ref: ref, Dest: dest, Format: Qcow2}, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Format != Qcow2 {
		t.Errorf("format = %q, want qcow2", result.Format)
	}

	// The scratch archive and the intermediate raw disk are both gigabytes and
	// both must be gone: a move that fills /tmp is its own outage.
	for _, leftover := range []string{archive, dest + ".img"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s was left behind", leftover)
		}
	}

	// The extracted disk is what gets converted, not the archive.
	var converted bool
	for _, call := range fake.Calls() {
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "convert") {
			converted = true
			if !strings.Contains(joined, dest+".img") {
				t.Errorf("converted the wrong file: %s", joined)
			}
		}
	}
	if !converted {
		t.Error("no conversion was attempted")
	}
}

// Compression is skipped on the scratch archive deliberately — gzipping several
// gigabytes only to read them back a second later is pure cost.
func TestIncus_DiskExportSkipsArchiveCompression(t *testing.T) {
	fake := withFake(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "dev.qcow2")

	fake.AddPrefixResponse("incus list", "[]", nil)
	fake.AddPrefixResponse("incus export", "", nil)
	_, _ = (Incus{}).Export(context.Background(),
		Request{Ref: types.InstanceRef{Backend: "incus", Name: "dev"}, Dest: dest, Format: Qcow2}, nil)

	var found bool
	for _, call := range fake.Calls() {
		if call.Name != "incus" || len(call.Args) == 0 || call.Args[0] != "export" {
			continue
		}
		found = true
		joined := strings.Join(call.Args, " ")
		if !strings.Contains(joined, "--compression=none") {
			t.Errorf("scratch archive was compressed: %s", joined)
		}
	}
	if !found {
		t.Fatal("no incus export was attempted")
	}
}
