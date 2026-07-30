package windows

import (
	"encoding/xml"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) { runner = r }

// Adapter is one backend's Windows-install implementation.
type Adapter interface {
	// Requires describes the firmware and media this backend needs, so a
	// caller can be told what is missing before anything is created.
	Requires() Requirements
	// Define creates the instance, ready to be started into Windows Setup.
	// It does not start it: an unattended install wipes the disk it is given,
	// and that should be a separate, deliberate step.
	Define(ref types.InstanceRef, spec Spec) (Result, error)
}

// Result is what Define produced.
type Result struct {
	// AdminPassword is the generated local Administrator password for an
	// unattended install. Empty for an interactive one.
	AdminPassword string
	// Definition is the backend-native definition that was applied, kept so a
	// caller can show or store it.
	Definition string
	// AnswerISO is the answer-file medium built on the host, if any.
	AnswerISO string
}

// Unsupported reports a backend that cannot run this Windows install.
type Unsupported struct {
	Backend string
	Reason  string
	Remedy  string
}

func (e *Unsupported) Error() string {
	msg := fmt.Sprintf("this Windows workflow is not available on the %s backend: %s", e.Backend, e.Reason)
	if e.Remedy != "" {
		msg += " — " + e.Remedy
	}
	return msg
}

func unsupported(backend, reason, remedy string) error {
	return &Unsupported{Backend: backend, Reason: reason, Remedy: remedy}
}

// For returns the Windows adapter for a backend.
func For(backend string) (Adapter, error) {
	switch backend {
	case "libvirt":
		return Libvirt{}, nil
	case "incus":
		return Incus{}, nil
	case "kubevirt":
		// KubeVirt's flow predates this package and lives in pkg/kubevirt,
		// where it is wired to ConfigMap and containerdisk media that have no
		// counterpart here. Reporting it as "not this adapter" rather than
		// "unsupported" keeps the distinction honest.
		return nil, fmt.Errorf("KubeVirt's Windows flow lives in pkg/kubevirt — use kubevirt.CreateWindowsVM")
	case "qemu":
		return nil, unsupported("qemu",
			"Corral's local backend has no UEFI or TPM plumbing, both of which modern Windows requires",
			"use libvirt on the same host")
	default:
		return nil, unsupported(backend, "no adapter is implemented", "")
	}
}

// Supported reports whether this package can define a Windows guest on a
// backend.
func Supported(backend string) bool {
	switch backend {
	case "libvirt", "incus":
		return true
	}
	return false
}

// ── libvirt ───────────────────────────────────────────────────────
//
// A q35 domain with UEFI firmware, a TPM 2.0 emulator, a virtio system disk,
// and three optical drives: the Windows installer, the virtio-win drivers, and
// the answer file. Three is fine here — libvirt places SATA CD-ROMs freely,
// unlike Incus.
//
// <os firmware='efi'> asks libvirt to select an OVMF descriptor itself rather
// than hardcoding a path, because the path differs across distributions
// (/usr/share/OVMF, /usr/share/edk2/ovmf, …) and a wrong one fails at define
// time with a message about a missing file rather than about firmware.

type Libvirt struct{}

func (Libvirt) Requires() Requirements {
	return Requirements{
		Firmware:         Win11(),
		Drivers:          SecondCDROM,
		AnswerFileMedium: "an ISO built on the host and attached as a third CD-ROM",
		Prerequisites: []string{
			"OVMF/edk2 firmware (package: edk2-ovmf or ovmf)",
			"swtpm, for the emulated TPM 2.0 Windows 11 requires",
			"an ISO writer (xorriso, genisoimage, or mkisofs) for the answer file",
			"virtio-win.iso",
		},
	}
}

// DomainXML renders the libvirt domain. Exported because it is worth being
// able to inspect and test the definition without defining anything.
func (l Libvirt) DomainXML(spec Spec, diskPath, answerISO string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<domain type='kvm'>
  <name>%s</name>
  <memory unit='MiB'>%s</memory>
  <vcpu>%d</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='cdrom'/>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/><apic/>
    <hyperv mode='custom'>
      <relaxed state='on'/><vapic state='on'/><spinlocks state='on' retries='8191'/>
    </hyperv>
  </features>
  <cpu mode='host-passthrough'/>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
`, xmlEscape(spec.Name), memoryMiB(spec.Memory), orOne(spec.CPU), xmlEscape(diskPath))

	// Installer first so the empty disk falls through to it on first boot.
	fmt.Fprintf(&b, `    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
`, xmlEscape(spec.InstallerISO))
	if spec.VirtioISO != "" {
		fmt.Fprintf(&b, `    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='sdb' bus='sata'/>
      <readonly/>
    </disk>
`, xmlEscape(spec.VirtioISO))
	}
	if answerISO != "" {
		fmt.Fprintf(&b, `    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='sdc' bus='sata'/>
      <readonly/>
    </disk>
`, xmlEscape(answerISO))
	}
	// TPM 2.0 and a virtio NIC. tpm-crb is what Windows 11 expects; the
	// emulator is swtpm, which libvirt starts alongside the domain.
	//
	// The console is VNC, so there is deliberately no spicevmc agent channel:
	// libvirt refuses a domain that carries one without spice graphics
	// ("chardev 'spicevmc' not supported without spice graphics"), and the
	// channel would be dead weight anyway without a spice client at the other
	// end.
	b.WriteString(`    <tpm model='tpm-crb'>
      <backend type='emulator' version='2.0'/>
    </tpm>
    <interface type='network'>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
    <graphics type='vnc' port='-1' listen='127.0.0.1'/>
    <video><model type='qxl'/></video>
    <input type='tablet' bus='usb'/>
  </devices>
</domain>
`)
	return b.String()
}

func (l Libvirt) Define(ref types.InstanceRef, spec Spec) (Result, error) {
	if spec.InstallerISO == "" {
		return Result{}, fmt.Errorf("a Windows installation ISO is required")
	}
	if spec.VirtioISO == "" {
		return Result{}, unsupported("libvirt",
			"Windows Setup cannot see a virtio disk without the virtio-win drivers",
			"pass --virtio-iso, or let Corral download it from "+VirtioWinStableISO)
	}

	result := Result{}
	workDir := filepath.Dir(spec.InstallerISO)
	if spec.Unattended {
		password := spec.AdminPassword
		if password == "" {
			password = RandomPassword()
		}
		answerISO, err := BuildAnswerISO(workDir, AutounattendXML(spec.Name, password))
		if err != nil {
			return Result{}, err
		}
		result.AdminPassword = password
		result.AnswerISO = answerISO
	}

	diskPath := filepath.Join(workDir, spec.Name+".qcow2")
	size := spec.DiskSize
	if size == "" {
		size = "64G"
	}
	if out, err := runner.Run(qemuImg(), "create", "-f", "qcow2", diskPath, size); err != nil {
		return Result{}, fmt.Errorf("creating the system disk: %s", commandError(out, err))
	}

	doc := l.DomainXML(spec, diskPath, result.AnswerISO)
	result.Definition = doc

	path, cleanup, err := writeTemp(doc, "corral-windows-*.xml")
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	uri := ref.Context
	if uri == "" {
		uri = "qemu:///system"
	}
	if out, err := runner.Run("virsh", "-c", uri, "define", path); err != nil {
		return Result{}, fmt.Errorf("virsh define: %s", commandError(out, err))
	}
	return result, nil
}

// ── Incus ─────────────────────────────────────────────────────────
//
// Incus gives a VM one CD-ROM, so there is nowhere to attach a virtio driver
// disc during Setup. The upstream answer is distrobuilder's repack-windows,
// which slipstreams the drivers — and the answer file — into a copy of the
// installer ISO before it is ever booted.
//
// Corral drives that rather than inventing a parallel mechanism, and refuses
// with the exact command when distrobuilder is absent. Inventing one would
// mean reimplementing driver injection into a Windows boot image, which is
// distrobuilder's whole reason to exist.

type Incus struct{}

func (Incus) Requires() Requirements {
	return Requirements{
		Firmware:         Win11(),
		Drivers:          Slipstreamed,
		AnswerFileMedium: "slipstreamed into the installer image by distrobuilder",
		Prerequisites: []string{
			"distrobuilder (for `distrobuilder repack-windows`)",
			"virtio-win.iso",
			"an Incus remote with VM support",
		},
	}
}

// RepackedName is where the slipstreamed installer is written.
func RepackedName(dir, name string) string {
	return filepath.Join(dir, name+"-repacked.iso")
}

func (i Incus) Define(ref types.InstanceRef, spec Spec) (Result, error) {
	if spec.InstallerISO == "" {
		return Result{}, fmt.Errorf("a Windows installation ISO is required")
	}
	if _, err := exec.LookPath("distrobuilder"); err != nil {
		return Result{}, unsupported("incus",
			"an Incus VM gets a single CD-ROM, so the virtio drivers have to be inside the installer image before it boots",
			"install distrobuilder, which repacks the ISO with the drivers: "+
				"`distrobuilder repack-windows <windows.iso> <repacked.iso> --windows-version <version>`")
	}
	if spec.VirtioISO == "" {
		return Result{}, unsupported("incus",
			"repacking needs the virtio-win drivers",
			"pass --virtio-iso, or let Corral download it from "+VirtioWinStableISO)
	}

	workDir := filepath.Dir(spec.InstallerISO)
	repacked := RepackedName(workDir, spec.Name)
	if out, err := runner.Run("distrobuilder", "repack-windows",
		spec.InstallerISO, repacked, "--drivers", spec.VirtioISO); err != nil {
		return Result{}, fmt.Errorf("distrobuilder repack-windows: %s", commandError(out, err))
	}

	remote := ref.Context
	if remote == "" {
		remote = "local"
	}
	target := remote + ":" + spec.Name
	size := spec.DiskSize
	if size == "" {
		size = "64GiB"
	}
	// init, not launch: the caller starts it deliberately, since booting the
	// installer begins wiping the disk.
	args := []string{"init", "--empty", "--vm", target,
		"-c", "limits.cpu=" + fmt.Sprint(orOne(spec.CPU)),
		"-c", "limits.memory=" + orDefault(spec.Memory, "8GiB"),
		"-d", "root,size=" + size,
		// Windows 11's compatibility check wants both.
		"-c", "security.secureboot=true",
	}
	if out, err := runner.Run("incus", args...); err != nil {
		return Result{}, fmt.Errorf("incus init: %s", commandError(out, err))
	}
	if out, err := runner.Run("incus", "config", "device", "add", target, "install", "disk",
		"source="+repacked, "boot.priority=10"); err != nil {
		return Result{}, fmt.Errorf("attaching the installer: %s", commandError(out, err))
	}
	return Result{Definition: strings.Join(args, " ")}, nil
}

// ── helpers ───────────────────────────────────────────────────────

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// memoryMiB normalises 8Gi/8G/8192 into the MiB libvirt wants.
func memoryMiB(mem string) string {
	if mem == "" {
		return "8192"
	}
	value := strings.TrimSpace(mem)
	multiplier := 1
	switch {
	case strings.HasSuffix(strings.ToLower(value), "gi"), strings.HasSuffix(strings.ToLower(value), "gib"):
		value, multiplier = strings.TrimRight(value, "GgIiBb"), 1024
	case strings.HasSuffix(strings.ToUpper(value), "G"):
		value, multiplier = strings.TrimRight(value, "Gg"), 1024
	case strings.HasSuffix(strings.ToLower(value), "mi"), strings.HasSuffix(strings.ToUpper(value), "M"):
		value = strings.TrimRight(value, "MmIiBb")
	}
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n <= 0 {
		return "8192"
	}
	return fmt.Sprint(n * multiplier)
}

func orOne(n int) int {
	if n <= 0 {
		return 4
	}
	return n
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func qemuImg() string {
	if bin, err := runner.LookPath("qemu-img"); err == nil {
		return bin
	}
	return "qemu-img"
}

func commandError(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}
