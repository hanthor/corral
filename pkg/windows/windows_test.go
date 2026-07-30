package windows

import (
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
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

func TestFor_SupportedBackends(t *testing.T) {
	for _, backend := range []string{"libvirt", "incus"} {
		if _, err := For(backend); err != nil {
			t.Errorf("no adapter for %q: %v", backend, err)
		}
		if !Supported(backend) {
			t.Errorf("Supported(%q) = false but an adapter exists", backend)
		}
	}
}

// Local QEMU has no UEFI or TPM plumbing, both of which modern Windows
// requires — so it refuses with somewhere to go rather than producing a VM
// that fails Setup's compatibility check forty minutes in.
func TestFor_LocalQemuRefusesWithAnAlternative(t *testing.T) {
	_, err := For("qemu")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(u.Reason, "TPM") || !strings.Contains(u.Remedy, "libvirt") {
		t.Errorf("refusal is not actionable: %v", u)
	}
}

// ── the firmware contract ─────────────────────────────────────────

// This is what unit tests exist to pin: Windows 11 and Server 2022+ refuse to
// install without UEFI, Secure Boot capability, and a TPM 2.0. A backend that
// quietly drops one produces an installer that stops at its compatibility
// check, which is the slowest possible way to learn about it.
func TestRequires_EveryBackendPromisesTheWindows11Firmware(t *testing.T) {
	for _, backend := range []string{"libvirt", "incus"} {
		adapter, err := For(backend)
		if err != nil {
			t.Fatal(err)
		}
		fw := adapter.Requires().Firmware
		if !fw.UEFI || !fw.SecureBoot || !fw.TPM {
			t.Errorf("%s firmware = %+v, want UEFI+SecureBoot+TPM", backend, fw)
		}
		if len(adapter.Requires().Prerequisites) == 0 {
			t.Errorf("%s names no prerequisites, so a missing tool cannot be reported", backend)
		}
	}
}

// The two backends genuinely differ here, and the difference is the whole
// reason Incus needs distrobuilder.
func TestRequires_DriverDeliveryDiffersByBackend(t *testing.T) {
	if got := (Libvirt{}).Requires().Drivers; got != SecondCDROM {
		t.Errorf("libvirt drivers = %q, want %q", got, SecondCDROM)
	}
	if got := (Incus{}).Requires().Drivers; got != Slipstreamed {
		t.Errorf("incus drivers = %q, want %q — an Incus VM has only one CD-ROM", got, Slipstreamed)
	}
}

// ── libvirt domain ────────────────────────────────────────────────

func TestLibvirt_DomainXMLIsWellFormed(t *testing.T) {
	doc := (Libvirt{}).DomainXML(Spec{
		Name: "win11", CPU: 4, Memory: "8Gi", InstallerISO: "/iso/win.iso", VirtioISO: "/iso/virtio.iso",
	}, "/disks/win11.qcow2", "/iso/autounattend.iso")

	var v any
	if err := xml.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("generated domain XML is not well-formed: %v\n%s", err, doc)
	}
}

// Every one of these is load-bearing. Losing the TPM or the firmware line
// fails Setup's compatibility check; losing the virtio driver disc leaves
// Setup unable to see the disk at all.
func TestLibvirt_DomainXMLCarriesTheWindowsRequirements(t *testing.T) {
	doc := (Libvirt{}).DomainXML(Spec{
		Name: "win11", CPU: 4, Memory: "8Gi", InstallerISO: "/iso/win.iso", VirtioISO: "/iso/virtio.iso",
	}, "/disks/win11.qcow2", "/iso/autounattend.iso")

	for what, want := range map[string]string{
		"UEFI firmware":      `firmware='efi'`,
		"q35 machine type":   `machine='q35'`,
		"TPM device":         `<tpm model='tpm-crb'>`,
		"TPM 2.0":            `version='2.0'`,
		"virtio system disk": `bus='virtio'`,
		"installer ISO":      `/iso/win.iso`,
		"virtio driver ISO":  `/iso/virtio.iso`,
		"answer file ISO":    `/iso/autounattend.iso`,
		"hyperv":             `<hyperv mode='custom'>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("domain XML is missing the %s (%q)", what, want)
		}
	}
}

// An interactive install has no answer file, and attaching an empty CD-ROM
// would be a device that points at nothing.
func TestLibvirt_DomainXMLOmitsAbsentMedia(t *testing.T) {
	doc := (Libvirt{}).DomainXML(Spec{Name: "win11", InstallerISO: "/iso/win.iso"}, "/disks/d.qcow2", "")
	if strings.Contains(doc, "autounattend") {
		t.Error("attached an answer-file CD-ROM for an interactive install")
	}
	if strings.Count(doc, "device='cdrom'") != 1 {
		t.Errorf("want exactly the installer CD-ROM, got %d", strings.Count(doc, "device='cdrom'"))
	}
}

func TestLibvirt_DefineRequiresTheVirtioDrivers(t *testing.T) {
	withFake(t)
	_, err := (Libvirt{}).Define(types.InstanceRef{Backend: "libvirt", Name: "win11"},
		Spec{Name: "win11", InstallerISO: "/iso/win.iso"})
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(u.Remedy, VirtioWinStableISO) {
		t.Errorf("refusal does not say where to get the drivers: %q", u.Remedy)
	}
}

func TestLibvirt_DefineNeedsAnInstaller(t *testing.T) {
	withFake(t)
	if _, err := (Libvirt{}).Define(types.InstanceRef{Backend: "libvirt", Name: "win11"}, Spec{Name: "win11"}); err == nil {
		t.Fatal("defined a Windows VM with no installation media")
	}
}

func TestMemoryMiB(t *testing.T) {
	for input, want := range map[string]string{
		"8Gi":   "8192",
		"8G":    "8192",
		"4096":  "4096",
		"4096M": "4096",
		"":      "8192",
		"junk":  "8192",
	} {
		if got := memoryMiB(input); got != want {
			t.Errorf("memoryMiB(%q) = %q, want %q", input, got, want)
		}
	}
}

// ── Incus ─────────────────────────────────────────────────────────

// distrobuilder is not something Corral can substitute for: slipstreaming
// drivers into a Windows boot image is its entire reason to exist. The
// refusal has to name it and the command.
func TestIncus_RefusesWithoutDistrobuilder(t *testing.T) {
	withFake(t)
	if _, err := exec.LookPath("distrobuilder"); err == nil {
		t.Skip("distrobuilder is installed here, so the refusal cannot trigger")
	}
	_, err := (Incus{}).Define(types.InstanceRef{Backend: "incus", Name: "win11"},
		Spec{Name: "win11", InstallerISO: "/iso/win.iso", VirtioISO: "/iso/virtio.iso"})
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(u.Remedy, "repack-windows") {
		t.Errorf("refusal does not name the command that fixes it: %q", u.Remedy)
	}
	if !strings.Contains(u.Reason, "CD-ROM") {
		t.Errorf("refusal does not explain why Incus is different: %q", u.Reason)
	}
}

// ── answer-file medium ────────────────────────────────────────────

// The filename is load-bearing — Windows Setup looks for this exact name at
// the medium's root and ignores anything else.
func TestBuildAnswerISO_WritesAutounattendAtTheRoot(t *testing.T) {
	dir := t.TempDir()
	path, err := BuildAnswerISO(dir, AutounattendXML("win11", "P@ssw0rd123!"))
	if err != nil {
		var missing MissingISOTool
		if errors.As(err, &missing) {
			t.Skipf("no ISO writer on this host: %v", err)
		}
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("produced an empty ISO")
	}
	// ISO9660's identifier sits at offset 32769; a file that lacks it is not
	// an ISO whatever the extension says.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 32774 || string(data[32769:32774]) != "CD001" {
		t.Error("the produced file is not an ISO9660 image")
	}
	if !strings.Contains(string(data), "autounattend.xml") &&
		!strings.Contains(strings.ToUpper(string(data)), "AUTOUNATTEND.XML") {
		t.Error("the ISO does not contain a file named autounattend.xml")
	}
}

// A host with no ISO writer must get a typed refusal naming the packages, not
// a generic failure.
func TestBuildAnswerISO_MissingToolIsTyped(t *testing.T) {
	err := MissingISOTool{}
	if !strings.Contains(err.Error(), "xorriso") {
		t.Errorf("refusal does not name a package to install: %v", err)
	}
}

func TestEnsureVirtioISO_UsesTheCacheAndOnlyDownloadsOnce(t *testing.T) {
	dir := t.TempDir()
	cached := filepath.Join(dir, "virtio-win.iso")
	if err := os.WriteFile(cached, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	var downloads int
	path, err := EnsureVirtioISO(dir, func(string, string) error { downloads++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if path != cached {
		t.Errorf("path = %q, want the cached copy", path)
	}
	if downloads != 0 {
		t.Error("re-downloaded an ISO that was already cached")
	}
}

func TestEnsureVirtioISO_ExplainsItselfWithNoDownloader(t *testing.T) {
	_, err := EnsureVirtioISO(t.TempDir(), nil)
	if err == nil {
		t.Fatal("want an error when the ISO is absent and nothing can fetch it")
	}
	if !strings.Contains(err.Error(), VirtioWinStableISO) {
		t.Errorf("error does not say where to get it: %v", err)
	}
}
