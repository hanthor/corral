//go:build e2ebootc

// Real verification of the local bootc builder and targets (#130).
//
// Separate build tag from the other adapter suites: this one pulls a
// multi-gigabyte container image and runs a privileged `bootc install
// to-disk`, which takes minutes and needs root. It is not something to attach
// to every CI run, and it is emphatically not something to run by accident.
//
// Run with: sudo -E go test -tags e2ebootc -timeout 30m ./pkg/bootc/
// or unprivileged with CORRAL_BOOTC_SUDO=1 to let it use sudo per command.
//
// Everything is written into t.TempDir(): the build targets a *file*, never a
// block device, which is the one mistake that would matter here.

package bootc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// bootcTestImage is small and official. Overridable so the suite can be
// pointed at whatever image is already cached on a given host.
func bootcTestImage() string {
	if v := os.Getenv("CORRAL_BOOTC_TEST_IMAGE"); v != "" {
		return v
	}
	return "quay.io/fedora/fedora-bootc:41"
}

func realBuilder(t *testing.T) LocalBuilder {
	t.Helper()
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	b := LocalBuilder{Sudo: os.Getenv("CORRAL_BOOTC_SUDO") != ""}
	if err := b.Available(); err != nil {
		t.Skipf("local bootc build not possible here: %v", err)
	}
	return b
}

// The whole point: a real bootc image really does produce a bootable disk
// through the exact command this package constructs.
func TestE2E_LocalBootcBuild(t *testing.T) {
	b := realBuilder(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "bootc.raw")

	var stages []string
	result, err := b.Build(BuildRequest{
		Image: bootcTestImage(),
		Dest:  dest,
		// Small, because the test only needs to prove the disk is real and
		// bootable-shaped, not to hold a working system's worth of packages.
		Size: "10G",
	}, func(msg string) {
		stages = append(stages, msg)
		t.Log(msg)
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Bytes == 0 {
		t.Fatal("produced an empty disk")
	}
	t.Logf("built %s (%d bytes, %s), stages: %d", result.Path, result.Bytes, result.Format, len(stages))

	if result.Backend != "ostree" {
		t.Errorf("backend = %q; the test image ships bootupd and should detect as ostree", result.Backend)
	}

	// A disk is not bootable because bootc exited 0. It needs a GPT label, an
	// EFI system partition, and — the part that actually decides whether a
	// fresh VM boots — a bootloader on the removable fallback path. bootupd
	// writes EFI/<vendor>/ plus an efibootmgr NVRAM entry, and a new VM has
	// empty NVRAM, so EFI/BOOT/BOOTX64.EFI is what makes the difference
	// between booting and a blank screen.
	assertBootable(t, dest)
	assertRemovableBootloader(t, dest)
}

// assertBootable checks the produced image really is a partitioned, UEFI-
// bootable disk, using whatever partition reader the host has.
func assertBootable(t *testing.T, disk string) {
	t.Helper()

	// qemu-img sees any image; a raw disk it cannot read at all is broken.
	if bin, err := exec.LookPath("qemu-img"); err == nil {
		out, err := exec.Command(bin, "info", disk).CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-img cannot read the built disk: %s: %v", out, err)
		}
		t.Logf("qemu-img: %s", firstLine(string(out)))
	}

	for _, reader := range [][]string{
		{"sfdisk", "--list", disk},
		{"fdisk", "-l", disk},
		{"parted", "-s", disk, "print"},
	} {
		bin, err := exec.LookPath(reader[0])
		if err != nil {
			continue
		}
		args := reader[1:]
		if os.Geteuid() != 0 && os.Getenv("CORRAL_BOOTC_SUDO") != "" {
			bin, args = "sudo", append([]string{"-n", reader[0]}, args...)
		}
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			continue
		}
		listing := string(out)
		t.Logf("%s:\n%s", reader[0], strings.TrimSpace(listing))
		lower := strings.ToLower(listing)
		if !strings.Contains(lower, "gpt") {
			t.Errorf("%s shows no GPT label — bootc installs GPT and UEFI firmware will not boot this", reader[0])
		}
		if !strings.Contains(lower, "efi") && !strings.Contains(lower, "esp") {
			t.Errorf("%s shows no EFI system partition — a UEFI guest will not boot this disk", reader[0])
		}
		return
	}
	t.Log("no partition reader available; disk contents unverified beyond size")
}

// The local QEMU target has to adopt the built disk rather than replacing it —
// the whole build would be wasted otherwise, and the symptom is a VM that
// boots to nothing.
func TestE2E_QEMUTargetAdoptsTheBuiltDisk(t *testing.T) {
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}

	// A stand-in disk: this test is about placement and adoption, not about
	// bootc, so it does not need a real build.
	dir := t.TempDir()
	built := filepath.Join(dir, "built.raw")
	if out, err := exec.Command("qemu-img", "create", "-f", "raw", built, "64M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}

	home, units := t.TempDir(), t.TempDir()
	restoreHome, restoreCreate := vmHome, createLocalVM
	vmHome = func() string { return home }
	var got types.CreateOpts
	createLocalVM = func(opts types.CreateOpts) error { got = opts; return nil }
	t.Cleanup(func() { vmHome, createLocalVM = restoreHome, restoreCreate })
	_ = units

	ref := types.InstanceRef{Backend: "qemu", Name: "bootcvm"}
	if err := (QEMUTarget{}).Import(ref, built, CreateOpts{CPU: 2, Memory: "4Gi"}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The disk must be where pkg/qemu looks, and be a real qcow2.
	placed := filepath.Join(home, "bootcvm", "disk.qcow2")
	info, err := os.Stat(placed)
	if err != nil {
		t.Fatalf("the built disk was not placed where pkg/qemu expects: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("placed an empty disk")
	}
	out, err := exec.Command("qemu-img", "info", "--output=json", placed).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img cannot read the placed disk: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "qcow2") {
		t.Errorf("the placed disk is not qcow2: %s", out)
	}

	// And Create must have been told to adopt it.
	if !got.ExistingDisk {
		t.Error("Create was not told the disk already exists — it would overwrite the build")
	}
	if got.Name != "bootcvm" || got.Backend != "qemu" {
		t.Errorf("unexpected create opts: %+v", got)
	}
	t.Logf("adopted: %+v", got)
}

// A real libvirt has to accept the generated domain, or a bootc VM cannot be
// defined at all. Same approach as the Windows and device suites: define, read
// back, never start.
func TestE2E_LibvirtTargetDomainIsAccepted(t *testing.T) {
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	if _, err := exec.LookPath("virsh"); err != nil {
		t.Skip("virsh not installed")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	uri := ""
	for _, candidate := range []string{"qemu:///system", "qemu:///session"} {
		if exec.Command("virsh", "-c", candidate, "connect").Run() == nil {
			uri = candidate
			break
		}
	}
	if uri == "" {
		t.Skip("no usable libvirt connection")
	}

	dir := t.TempDir()
	built := filepath.Join(dir, "built.raw")
	if out, err := exec.Command("qemu-img", "create", "-f", "raw", built, "64M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}

	const domain = "corral-bootc-e2e"
	t.Cleanup(func() { exec.Command("virsh", "-c", uri, "undefine", domain, "--nvram").Run() })

	ref := types.InstanceRef{Backend: "libvirt", Context: uri, Name: domain}
	err := (LibvirtTarget{}).Import(ref, built, CreateOpts{CPU: 2, Memory: "4Gi"})
	if err != nil {
		if strings.Contains(err.Error(), "firmware") || strings.Contains(err.Error(), "loader") {
			t.Skipf("host has no UEFI firmware (%v); target declares: %v", err, (LibvirtTarget{}).Requires())
		}
		t.Fatalf("real libvirt rejected the bootc domain: %v", err)
	}
	t.Log("libvirt accepted the bootc domain definition")

	out, err := exec.Command("virsh", "-c", uri, "dumpxml", domain).CombinedOutput()
	if err != nil {
		t.Fatalf("virsh dumpxml: %s: %v", out, err)
	}
	stored := string(out)
	// UEFI is the one that silently matters: a bootc image installs a UEFI
	// bootloader, and a BIOS domain boots it to a blank screen.
	if !strings.Contains(stored, "efi") && !strings.Contains(stored, "loader") {
		t.Errorf("the stored domain has no UEFI firmware — a bootc disk will not boot:\n%s", stored)
	}
	if !strings.Contains(stored, "qcow2") {
		t.Errorf("the stored domain does not reference the converted qcow2:\n%s", stored)
	}
}

// assertRemovableBootloader mounts the ESP and checks for the fallback
// bootloader path. This is the check that would have caught a build missing
// --generic-image: the disk looks perfect right up until nothing boots.
func assertRemovableBootloader(t *testing.T, disk string) {
	t.Helper()
	sudo := func(args ...string) *exec.Cmd {
		if os.Geteuid() == 0 {
			return exec.Command(args[0], args[1:]...)
		}
		if os.Getenv("CORRAL_BOOTC_SUDO") == "" {
			return nil
		}
		return exec.Command("sudo", append([]string{"-n"}, args...)...)
	}
	attach := sudo("losetup", "--find", "--show", "--partscan", disk)
	if attach == nil {
		t.Skip("cannot attach a loop device without root")
	}
	out, err := attach.CombinedOutput()
	if err != nil {
		t.Skipf("losetup unavailable here: %s: %v", out, err)
	}
	loop := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if c := sudo("losetup", "-d", loop); c != nil {
			c.Run()
		}
	})

	mnt := t.TempDir()
	var mounted bool
	for _, part := range []string{"p1", "p2", "p3"} {
		c := sudo("mount", loop+part, mnt)
		if c == nil {
			t.Skip("cannot mount without root")
		}
		if c.Run() != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(mnt, "EFI")); err == nil {
			mounted = true
			t.Cleanup(func() {
				if u := sudo("umount", mnt); u != nil {
					u.Run()
				}
			})
			break
		}
		if u := sudo("umount", mnt); u != nil {
			u.Run()
		}
	}
	if !mounted {
		t.Fatal("no partition on the built disk contains an /EFI directory — nothing will boot this")
	}

	fallback := filepath.Join(mnt, "EFI", "BOOT")
	entries, err := os.ReadDir(fallback)
	if err != nil {
		t.Fatalf("no EFI/BOOT on the ESP: a fresh VM has empty NVRAM and will boot nothing (%v)", err)
	}
	var found string
	for _, e := range entries {
		if strings.HasPrefix(strings.ToUpper(e.Name()), "BOOT") && strings.HasSuffix(strings.ToUpper(e.Name()), ".EFI") {
			found = e.Name()
		}
	}
	if found == "" {
		t.Fatalf("EFI/BOOT holds no BOOTX64.EFI — the disk builds but does not boot; entries: %v", entries)
	}
	t.Logf("removable bootloader present: EFI/BOOT/%s", found)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
