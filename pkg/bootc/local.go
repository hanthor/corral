package bootc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// ── local builder ─────────────────────────────────────────────────
//
// `bootc install to-disk` run from the image itself, in a privileged podman
// container. This is upstream's own documented method: the bootc image
// contains the installer, so the container is given the target file and asked
// to partition and populate it.
//
// Privileged and root are both unavoidable. bootc install partitions a block
// device, creates filesystems, and installs a bootloader — it needs loop
// devices, mount, and SELinux relabelling. Rootless podman cannot do any of
// it, so this checks for what it needs up front and says so rather than
// failing four gigabytes into a pull.

type LocalBuilder struct {
	// Sudo runs podman through sudo. Required unless the caller is already
	// root, since the container needs privileges rootless cannot grant.
	Sudo bool
}

func (b LocalBuilder) Available() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return unsupported("the local builder",
			"podman is not installed, and bootc install runs from inside the image",
			"install podman")
	}
	if os.Geteuid() != 0 && !b.Sudo {
		return unsupported("the local builder",
			"bootc install partitions a disk and installs a bootloader, which needs root",
			"re-run with --sudo, or as root")
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		return unsupported("the local builder",
			"the kernel exposes no loop devices, and bootc install needs one to write a disk image",
			"load the loop module (modprobe loop)")
	}
	return nil
}

// podman builds the command, honouring Sudo.
func (b LocalBuilder) podman(args ...string) (string, []string) {
	if b.Sudo && os.Geteuid() != 0 {
		return "sudo", append([]string{"-n", "podman"}, args...)
	}
	return "podman", args
}

func (b LocalBuilder) Build(req BuildRequest, progress func(string)) (BuildResult, error) {
	if err := b.Available(); err != nil {
		return BuildResult{}, err
	}
	if req.Image == "" {
		return BuildResult{}, fmt.Errorf("a bootc container image is required")
	}
	if req.Dest == "" {
		return BuildResult{}, fmt.Errorf("a destination disk path is required")
	}
	if err := os.MkdirAll(filepath.Dir(req.Dest), 0o755); err != nil {
		return BuildResult{}, err
	}

	report := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}

	// Pull first, as its own step. It is the slow part by far, and a failure
	// here means "bad image reference or no network", which is worth telling
	// apart from a failure during the install.
	report("pulling " + req.Image)
	name, args := b.podman("pull", req.Image)
	if out, err := runner.Run(name, args...); err != nil {
		return BuildResult{}, fmt.Errorf("podman pull %s: %s", req.Image, commandError(out, err))
	}

	backend, err := b.DetectBackend(req.Image)
	if err != nil {
		return BuildResult{}, err
	}
	if req.Filesystem != "" {
		backend.Filesystem = req.Filesystem
	}
	report(fmt.Sprintf("image uses the %s backend (%s)", backend.Kind, backend.Filesystem))
	if backend.Kind == "composefs" {
		// The cluster builder does real work after bootc install for these:
		// re-extracting the kernel and initrd over bootc's EROFS zero-filled
		// ESP copies, and injecting the root key into
		// state/os/default/var/roothome/.ssh because
		// --root-ssh-authorized-keys is a no-op on composefs. Shipping half of
		// that would produce a desktop image that builds and then does not
		// boot, which is the failure this whole package tries to avoid.
		return BuildResult{}, unsupported("composefs images in a local build",
			"they need kernel/initrd re-extraction and root-key injection after install, which only the cluster builder implements",
			"build this image with the KubeVirt context, or use an ostree image (one shipping bootupd) locally")
	}

	keyPath, cleanupKey, err := writeKey(req.SSHKey)
	if err != nil {
		return BuildResult{}, err
	}
	defer cleanupKey()

	// bootc writes into the file it is given, so it has to exist at full size
	// first — it partitions what is there rather than growing it.
	size := req.Size
	if size == "" {
		size = "20G"
	}
	report(fmt.Sprintf("allocating a %s disk at %s", size, req.Dest))
	if err := allocate(req.Dest, size); err != nil {
		return BuildResult{}, err
	}

	report("running bootc install to-disk (this takes a few minutes)")
	name, args = b.podman(b.installArgs(req, backend, keyPath)...)
	if out, err := runner.Run(name, args...); err != nil {
		// Leave nothing half-written: a truncated disk that looks like a disk
		// is worse than no disk.
		os.Remove(req.Dest)
		return BuildResult{}, fmt.Errorf("bootc install to-disk: %s", commandError(out, err))
	}

	info, err := os.Stat(req.Dest)
	if err != nil {
		return BuildResult{}, fmt.Errorf("bootc reported success but %s is missing: %w", req.Dest, err)
	}
	report(fmt.Sprintf("built %s (%d bytes)", req.Dest, info.Size()))
	return BuildResult{Path: req.Dest, Format: "raw", Bytes: info.Size(), Backend: backend.Kind}, nil
}

// Backend is the bootc storage backend an image needs. It is a property of
// the image, not a choice: composefs images ship a btrfs-only initramfs and
// no bootupd, ostree images ship bootupd and want xfs.
type Backend struct {
	Kind       string // "ostree" or "composefs"
	Filesystem string
	Flag       string // --generic-image or --composefs-backend
}

var (
	ostreeBackend    = Backend{Kind: "ostree", Filesystem: "xfs", Flag: "--generic-image"}
	composefsBackend = Backend{Kind: "composefs", Filesystem: "btrfs", Flag: "--composefs-backend"}
)

// DetectBackend reads the image to decide which backend it needs, mirroring
// what pkg/kubevirt's cluster builder does — the rule was worked out there
// (f63917e) and getting it wrong produces a disk that does not boot.
//
// Per the bootc docs the composefs-rs backend requires systemd-boot and no
// bootupd; traditional ostree images ship bootupd.
//
// The probe inspects the image *filesystem* with `podman cp` and never
// executes anything in it. Universal Blue desktop images ship Rust uutils,
// where /usr/bin/test is a symlink into a multicall binary that podman
// --entrypoint cannot dispatch, so any "run a binary in the image" probe
// misdetects exactly the images this matters most for.
func (b LocalBuilder) DetectBackend(image string) (Backend, error) {
	name, args := b.podman("create", image)
	out, err := runner.Run(name, args...)
	if err != nil {
		return Backend{}, fmt.Errorf("podman create %s: %s", image, commandError(out, err))
	}
	container := strings.TrimSpace(lastLine(string(out)))
	defer func() {
		rm, rmArgs := b.podman("rm", "-f", container)
		_, _ = runner.Run(rm, rmArgs...)
	}()

	has := func(path string) bool {
		cp, cpArgs := b.podman("cp", container+":"+path, "-")
		_, err := runner.Run(cp, cpArgs...)
		return err == nil
	}
	switch {
	case has("/usr/sbin/bootupctl"), has("/usr/bin/bootupctl"):
		return ostreeBackend, nil
	case has("/usr/lib/systemd/boot/efi/systemd-bootx64.efi"),
		has("/usr/lib/systemd/boot/efi/systemd-bootaa64.efi"):
		return composefsBackend, nil
	default:
		// Neither marker: treat as ostree, which is what the cluster builder
		// does and the safer guess — xfs and a removable bootloader path.
		return ostreeBackend, nil
	}
}

// installArgs builds the podman invocation. Split out so a test can assert the
// flags without running anything: every one of them is load-bearing and a
// silently dropped one produces a disk that does not boot.
func (b LocalBuilder) installArgs(req BuildRequest, backend Backend, keyPath string) []string {
	dest, _ := filepath.Abs(req.Dest)
	args := []string{
		"run", "--rm",
		// bootc install needs to partition, mount, and relabel.
		"--privileged",
		"--pid=host",
		// The container writes the host's disk file, and needs the host's
		// /dev to reach loop devices.
		"-v", "/dev:/dev",
		"-v", "/var/lib/containers:/var/lib/containers",
		"-v", dest + ":/target.img",
		"--security-opt", "label=type:unconfined_t",
	}
	if keyPath != "" {
		args = append(args, "-v", keyPath+":/buildkey.pub:ro")
	}
	args = append(args, req.Image,
		"bootc", "install", "to-disk",
		// A file, not a block device: --via-loopback is what makes bootc
		// attach the image file as a loop device rather than refusing.
		//
		// The cluster builder cannot use this — pod security contexts there
		// lack loop module access, which is what e141dbb worked around — but a
		// privileged container on a real host has loop devices, so the simpler
		// route is available here.
		"--via-loopback",
		// The backend flag is not cosmetic. For ostree images bootupd installs
		// the bootloader to EFI/<vendor>/ plus an efibootmgr NVRAM entry, and a
		// fresh VM has empty NVRAM — so without --generic-image's removable
		// EFI/BOOT/BOOTX64.EFI fallback the disk builds fine and then boots to
		// nothing.
		backend.Flag,
		"--filesystem", backend.Filesystem,
		// Without this bootc refuses to overwrite a disk that already has a
		// partition table — and the file was just allocated, so there is
		// nothing to preserve.
		"--wipe",
	)
	if keyPath != "" {
		args = append(args, "--root-ssh-authorized-keys", "/buildkey.pub")
	}
	args = append(args, "/target.img")
	return args
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// allocate creates a sparse file of the requested size. Sparse so a 20G disk
// costs only what is written — bootc populates a few gigabytes at most.
func allocate(path, size string) error {
	bytes, err := parseSize(size)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(bytes); err != nil {
		os.Remove(path)
		return fmt.Errorf("allocating %s: %w", path, err)
	}
	return nil
}

// parseSize accepts the sizes the rest of Corral uses: 20G, 20Gi, 20480M.
func parseSize(s string) (int64, error) {
	value := strings.TrimSpace(s)
	if value == "" {
		return 0, fmt.Errorf("empty size")
	}
	var multiplier int64 = 1
	upper := strings.ToUpper(value)
	switch {
	case strings.HasSuffix(upper, "GIB"), strings.HasSuffix(upper, "GI"), strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
	case strings.HasSuffix(upper, "MIB"), strings.HasSuffix(upper, "MI"), strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
	case strings.HasSuffix(upper, "TIB"), strings.HasSuffix(upper, "TI"), strings.HasSuffix(upper, "T"):
		multiplier = 1 << 40
	}
	digits := strings.TrimRight(upper, "GIMBT")
	var n int64
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size like 20G", s)
	}
	return n * multiplier, nil
}

// ── local QEMU target ─────────────────────────────────────────────
//
// The disk is converted to qcow2 and placed where pkg/qemu expects a VM's
// disk, then the VM is created with ExistingDisk so Create adopts it rather
// than making a fresh one.

type QEMUTarget struct{}

func (QEMUTarget) Requires() []string {
	return []string{"qemu-system-x86_64 and qemu-img", "/dev/kvm for acceptable speed"}
}

func (QEMUTarget) Import(ref types.InstanceRef, disk string, opts CreateOpts) error {
	if ref.Context != "" {
		return unsupported("the local qemu backend", "it has no contexts", "drop --context for local VMs")
	}
	if _, err := os.Stat(disk); err != nil {
		return fmt.Errorf("built disk %s is missing: %w", disk, err)
	}
	vmDir := filepath.Join(vmHome(), ref.Name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return err
	}
	// qcow2 rather than the raw bootc output: sparse on disk, snapshot-capable
	// (pkg/snapshot's local adapter needs qcow2), and what pkg/qemu expects.
	target := filepath.Join(vmDir, "disk.qcow2")
	if out, err := runner.Run(qemuImg(), "convert", "-O", "qcow2", disk, target); err != nil {
		return fmt.Errorf("qemu-img convert: %s", commandError(out, err))
	}
	return createLocalVM(types.CreateOpts{
		Name: ref.Name, Backend: "qemu",
		CPU: opts.CPU, Mem: opts.Memory, Disk: opts.Disk,
		SSHPublicKey: opts.SSHKey,
		// The disk is already built and populated; Create must adopt it.
		ExistingDisk: true,
	})
}

// ── libvirt target ────────────────────────────────────────────────
//
// A UEFI domain around the converted disk. UEFI because bootc images install
// a UEFI bootloader — a BIOS domain simply will not boot one, and the symptom
// is a blank screen rather than an error.

type LibvirtTarget struct{}

func (LibvirtTarget) Requires() []string {
	return []string{
		"virsh and a reachable libvirt connection",
		"OVMF/edk2 firmware — bootc images boot via UEFI",
		"qemu-img",
	}
}

// DomainXML is exported so the definition can be inspected and tested without
// defining anything.
func (LibvirtTarget) DomainXML(name, diskPath string, opts CreateOpts) string {
	memory := opts.Memory
	if memory == "" {
		memory = "4Gi"
	}
	cpu := opts.CPU
	if cpu <= 0 {
		cpu = 2
	}
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <memory unit='MiB'>%s</memory>
  <vcpu>%d</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features><acpi/><apic/></features>
  <cpu mode='host-passthrough'/>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='network'>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
    <graphics type='vnc' port='-1' listen='127.0.0.1'/>
    <video><model type='virtio'/></video>
    <console type='pty'/>
  </devices>
</domain>
`, xmlEscape(name), memoryMiB(memory), cpu, xmlEscape(diskPath))
}

func (l LibvirtTarget) Import(ref types.InstanceRef, disk string, opts CreateOpts) error {
	if _, err := os.Stat(disk); err != nil {
		return fmt.Errorf("built disk %s is missing: %w", disk, err)
	}
	target := filepath.Join(filepath.Dir(disk), ref.Name+".qcow2")
	if out, err := runner.Run(qemuImg(), "convert", "-O", "qcow2", disk, target); err != nil {
		return fmt.Errorf("qemu-img convert: %s", commandError(out, err))
	}
	doc := l.DomainXML(ref.Name, target, opts)
	path, cleanup, err := writeTemp(doc)
	if err != nil {
		return err
	}
	defer cleanup()
	uri := ref.Context
	if uri == "" {
		uri = "qemu:///system"
	}
	if out, err := runner.Run("virsh", "-c", uri, "define", path); err != nil {
		return fmt.Errorf("virsh define: %s", commandError(out, err))
	}
	return nil
}
