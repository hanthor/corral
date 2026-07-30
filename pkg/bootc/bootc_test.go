package bootc

import (
	"errors"
	"os"
	"path/filepath"
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

// ── the build/import split ────────────────────────────────────────

func TestTargetFor_ImplementedBackends(t *testing.T) {
	for _, backend := range []string{"qemu", "libvirt"} {
		if _, err := TargetFor(backend); err != nil {
			t.Errorf("no target for %q: %v", backend, err)
		}
		if !TargetSupported(backend) {
			t.Errorf("TargetSupported(%q) = false but a target exists", backend)
		}
		target, _ := TargetFor(backend)
		if len(target.Requires()) == 0 {
			t.Errorf("%s names no prerequisites, so a missing tool cannot be reported", backend)
		}
	}
}

// The issue allows "Incus VM support or a documented capability rejection".
// This is the rejection, and it has to explain itself and point somewhere —
// an Incus VM boots from Incus's own image store and cannot adopt a foreign
// pre-partitioned disk in any supported way.
func TestTargetFor_IncusIsADocumentedRejection(t *testing.T) {
	_, err := TargetFor("incus")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(u.Reason, "image store") {
		t.Errorf("rejection does not explain why Incus is different: %q", u.Reason)
	}
	if u.Remedy == "" {
		t.Error("rejection offers nowhere to go")
	}
	if TargetSupported("incus") {
		t.Error("TargetSupported reported a backend with no target")
	}
	// And the plugin must not advertise it.
	for _, b := range SupportedBackends {
		if b == "incus" {
			t.Error("SupportedBackends advertises incus, which has no target")
		}
	}
}

// KubeVirt still works, through its own path — that has to read as "look
// elsewhere", not as "unsupported", or the existing feature looks removed.
func TestTargetFor_KubeVirtPointsAtItsOwnPath(t *testing.T) {
	_, err := TargetFor("kubevirt")
	if err == nil {
		t.Fatal("want an error directing the caller to pkg/kubevirt")
	}
	var u *Unsupported
	if errors.As(err, &u) {
		t.Errorf("KubeVirt reported as unsupported, but its bootc path works: %v", err)
	}
	if !strings.Contains(err.Error(), "pkg/kubevirt") {
		t.Errorf("error does not say where the path lives: %v", err)
	}
	var advertised bool
	for _, b := range SupportedBackends {
		advertised = advertised || b == "kubevirt"
	}
	if !advertised {
		t.Error("SupportedBackends dropped kubevirt, whose path still works")
	}
}

// ── the builder's command ─────────────────────────────────────────

// Every one of these flags is load-bearing, and a silently dropped one
// produces a disk that does not boot — or a build that refuses to start.
func TestInstallArgs_CarriesWhatBootcNeeds(t *testing.T) {
	args := strings.Join(LocalBuilder{}.installArgs(BuildRequest{
		Image: "quay.io/fedora/fedora-bootc:41", Dest: "/tmp/disk.raw",
	}, ostreeBackend, ""), " ")

	for what, want := range map[string]string{
		"privileged, since bootc partitions and mounts": "--privileged",
		"the host's /dev, for loop devices":             "/dev:/dev",
		"loopback mode, since the target is a file":     "--via-loopback",
		"wipe, or bootc refuses a disk with a table":    "--wipe",
		"the bootc install subcommand":                  "bootc install to-disk",
		"the target image inside the container":         "/target.img",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the podman invocation is missing %s (%q):\n%s", what, want, args)
		}
	}
}

// Found by a real build: quay.io/fedora/fedora-bootc:41 fails with
// "Creating rootfs: No root filesystem specified" unless --filesystem is
// given. The image carries no default, and bootc will not choose one.
//
// Which filesystem is not a free choice either — it comes from the backend the
// image needs, per the rule the cluster builder established in f63917e.
func TestInstallArgs_CarriesTheBackendsFilesystemAndFlag(t *testing.T) {
	for _, tc := range []struct {
		backend          Backend
		wantFS, wantFlag string
	}{
		{ostreeBackend, "--filesystem xfs", "--generic-image"},
		{composefsBackend, "--filesystem btrfs", "--composefs-backend"},
	} {
		args := strings.Join(LocalBuilder{}.installArgs(
			BuildRequest{Image: "img", Dest: "/tmp/d.raw"}, tc.backend, ""), " ")
		if !strings.Contains(args, tc.wantFS) {
			t.Errorf("%s backend: missing %q\n%s", tc.backend.Kind, tc.wantFS, args)
		}
		if !strings.Contains(args, tc.wantFlag) {
			t.Errorf("%s backend: missing %q\n%s", tc.backend.Kind, tc.wantFlag, args)
		}
	}
}

// --generic-image is the difference between a disk that boots and one that
// does not. bootupd writes EFI/<vendor>/ plus an efibootmgr NVRAM entry; a
// fresh VM has empty NVRAM, so without the removable EFI/BOOT/BOOTX64.EFI
// fallback the build succeeds and the guest boots to nothing.
func TestInstallArgs_OstreeGetsTheRemovableBootloaderPath(t *testing.T) {
	args := strings.Join(LocalBuilder{}.installArgs(
		BuildRequest{Image: "img", Dest: "/tmp/d.raw"}, ostreeBackend, ""), " ")
	if !strings.Contains(args, "--generic-image") {
		t.Errorf("no --generic-image: the disk will build and then not boot\n%s", args)
	}
}

// A bootc disk built with no key has no way in at all — the request carries
// one, and it has to reach bootc.
func TestInstallArgs_InjectsTheSSHKey(t *testing.T) {
	args := strings.Join(LocalBuilder{}.installArgs(
		BuildRequest{Image: "img", Dest: "/tmp/d.raw"}, ostreeBackend, "/tmp/key.pub"), " ")
	if !strings.Contains(args, "--root-ssh-authorized-keys /buildkey.pub") {
		t.Errorf("the SSH key never reaches bootc, so the disk has no way in:\n%s", args)
	}
	if !strings.Contains(args, "/tmp/key.pub:/buildkey.pub:ro") {
		t.Errorf("the key file is not mounted into the build container:\n%s", args)
	}
}

// The detection rule, mirrored from the cluster builder: bootupd means ostree,
// systemd-boot without bootupd means composefs.
func TestDetectBackend_FollowsTheClusterRule(t *testing.T) {
	for name, tc := range map[string]struct {
		present []string
		want    Backend
	}{
		"bootupd present":          {[]string{"/usr/sbin/bootupctl"}, ostreeBackend},
		"bootupd in /usr/bin":      {[]string{"/usr/bin/bootupctl"}, ostreeBackend},
		"systemd-boot, no bootupd": {[]string{"/usr/lib/systemd/boot/efi/systemd-bootx64.efi"}, composefsBackend},
		"neither marker":           {nil, ostreeBackend},
	} {
		t.Run(name, func(t *testing.T) {
			fake := withFake(t)
			fake.AddPrefixResponse("podman create", "container123", nil)
			fake.AddPrefixResponse("podman rm", "", nil)
			// Everything absent unless listed.
			fake.AddPrefixResponse("podman cp", "", errors.New("no such file"))
			for _, path := range tc.present {
				fake.AddResponse("podman cp container123:"+path+" -", "", nil)
			}

			got, err := LocalBuilder{}.DetectBackend("img")
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tc.want.Kind || got.Filesystem != tc.want.Filesystem {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The probe must never execute anything in the image: Universal Blue desktop
// images ship Rust uutils, where /usr/bin/test is a symlink into a multicall
// binary podman --entrypoint cannot dispatch — so an execution-based probe
// misdetects exactly the images this matters most for.
func TestDetectBackend_NeverExecutesTheImage(t *testing.T) {
	fake := withFake(t)
	fake.AddPrefixResponse("podman create", "container123", nil)
	fake.AddPrefixResponse("podman rm", "", nil)
	fake.AddPrefixResponse("podman cp", "", errors.New("absent"))

	if _, err := (LocalBuilder{}).DetectBackend("img"); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.Calls() {
		joined := strings.Join(call.Args, " ")
		if strings.HasPrefix(joined, "run ") || strings.Contains(joined, "--entrypoint") {
			t.Errorf("the probe executed the image: %v", call.Args)
		}
	}
}

func TestPodman_UsesSudoOnlyWhenAsked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where sudo is never added")
	}
	name, args := LocalBuilder{Sudo: true}.podman("pull", "img")
	if name != "sudo" || args[0] != "-n" {
		t.Errorf("sudo builder ran %q %v", name, args)
	}
	name, _ = LocalBuilder{}.podman("pull", "img")
	if name != "podman" {
		t.Errorf("non-sudo builder ran %q", name)
	}
}

// Available has to fail before a multi-gigabyte pull, not after, and say
// which prerequisite is missing.
func TestAvailable_RefusesUnprivilegedWithAReason(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where the privilege check passes")
	}
	err := LocalBuilder{}.Available()
	if err == nil {
		t.Skip("podman absent here, so a different check fired first")
	}
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if u.Remedy == "" {
		t.Errorf("refusal offers no way forward: %v", u)
	}
}

func TestBuild_RequiresAnImageAndDestination(t *testing.T) {
	withFake(t)
	b := LocalBuilder{Sudo: true}
	if b.Available() != nil {
		t.Skip("no local builder here")
	}
	if _, err := b.Build(BuildRequest{Dest: "/tmp/d.raw"}, nil); err == nil {
		t.Error("built with no image")
	}
	if _, err := b.Build(BuildRequest{Image: "img"}, nil); err == nil {
		t.Error("built with no destination")
	}
}

func TestParseSize(t *testing.T) {
	for input, want := range map[string]int64{
		"20G":   20 << 30,
		"20Gi":  20 << 30,
		"512M":  512 << 20,
		"1T":    1 << 40,
		"20GiB": 20 << 30,
	} {
		got, err := parseSize(input)
		if err != nil {
			t.Errorf("parseSize(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", input, got, want)
		}
	}
	for _, bad := range []string{"", "twenty", "-5G", "0G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted nonsense", bad)
		}
	}
}

// ── the local QEMU target ─────────────────────────────────────────

// The whole build is wasted if Create makes a fresh disk over the top, and
// the symptom is a VM that boots to nothing.
func TestQEMUTarget_AdoptsRatherThanRecreates(t *testing.T) {
	fake := withFake(t)
	fake.AddPrefixResponse("/fake/bin/qemu-img", "", nil)

	home := t.TempDir()
	restoreHome, restoreCreate := vmHome, createLocalVM
	vmHome = func() string { return home }
	var got types.CreateOpts
	createLocalVM = func(opts types.CreateOpts) error { got = opts; return nil }
	t.Cleanup(func() { vmHome, createLocalVM = restoreHome, restoreCreate })

	built := filepath.Join(t.TempDir(), "built.raw")
	if err := os.WriteFile(built, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := (QEMUTarget{}).Import(types.InstanceRef{Backend: "qemu", Name: "dev"}, built,
		CreateOpts{CPU: 2, Memory: "4Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExistingDisk {
		t.Error("Create was not told the disk already exists — it would overwrite the build")
	}

	// The conversion has to target where pkg/qemu looks for a VM's disk.
	var converted bool
	for _, call := range fake.Calls() {
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "convert") && strings.Contains(joined, filepath.Join(home, "dev", "disk.qcow2")) {
			converted = true
		}
	}
	if !converted {
		t.Errorf("the disk was not converted into the VM's own directory: %v", fake.Calls())
	}
}

func TestQEMUTarget_RefusesAContext(t *testing.T) {
	withFake(t)
	err := (QEMUTarget{}).Import(types.InstanceRef{Backend: "qemu", Context: "somewhere", Name: "dev"},
		"/tmp/built.raw", CreateOpts{})
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
}

func TestQEMUTarget_ReportsAMissingBuild(t *testing.T) {
	withFake(t)
	err := (QEMUTarget{}).Import(types.InstanceRef{Backend: "qemu", Name: "dev"},
		filepath.Join(t.TempDir(), "never-built.raw"), CreateOpts{})
	if err == nil {
		t.Fatal("imported a disk that does not exist")
	}
}

// ── the libvirt target ────────────────────────────────────────────

// A bootc image installs a UEFI bootloader. A BIOS domain boots it to a blank
// screen with no error, which is the worst way to find out.
func TestLibvirtTarget_DomainIsUEFI(t *testing.T) {
	doc := (LibvirtTarget{}).DomainXML("dev", "/disks/dev.qcow2", CreateOpts{CPU: 2, Memory: "4Gi"})
	if !strings.Contains(doc, "firmware='efi'") {
		t.Errorf("the domain is not UEFI, so a bootc disk will not boot:\n%s", doc)
	}
	for what, want := range map[string]string{
		"q35 machine type": "machine='q35'",
		"virtio boot disk": "bus='virtio'",
		"the built disk":   "/disks/dev.qcow2",
		"boot from disk":   "<boot dev='hd'/>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("domain is missing the %s (%q)", what, want)
		}
	}
}

func TestLibvirtTarget_DomainXMLIsWellFormed(t *testing.T) {
	doc := (LibvirtTarget{}).DomainXML("dev", "/disks/dev.qcow2", CreateOpts{})
	if err := wellFormed(doc); err != nil {
		t.Fatalf("generated domain XML is not well-formed: %v\n%s", err, doc)
	}
}

func TestMemoryMiB(t *testing.T) {
	for input, want := range map[string]string{
		"4Gi":  "4096",
		"4G":   "4096",
		"2048": "2048",
		"":     "4096",
		"junk": "4096",
	} {
		if got := memoryMiB(input); got != want {
			t.Errorf("memoryMiB(%q) = %q, want %q", input, got, want)
		}
	}
}
