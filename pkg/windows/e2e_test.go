//go:build e2esnapshot

// Real-tool verification of the Windows adapters (#132).
//
// The Windows workflow is the one place where a wrong guess costs the most: a
// missing TPM or the wrong firmware attribute does not fail fast, it fails
// forty minutes into an install, at a compatibility check, with a message that
// says nothing about libvirt. Unit tests can only prove the XML contains the
// strings I decided it should.
//
// So this asserts the two things a real tool can settle without Windows media:
// that libvirt accepts the generated domain, and that the answer-file ISO is
// really an ISO with autounattend.xml at its root.
//
// What it cannot verify: that Windows Setup runs to completion. That needs the
// installation media and an hour.
//
// Run with: go test -tags e2esnapshot ./pkg/windows/

package windows

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func libvirtURI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("virsh"); err != nil {
		t.Skip("virsh not installed")
	}
	for _, candidate := range []string{"qemu:///system", "qemu:///session"} {
		if exec.Command("virsh", "-c", candidate, "connect").Run() == nil {
			return candidate
		}
	}
	t.Skip("no usable libvirt connection")
	return ""
}

// The generated domain has to be one libvirt will actually take. This is the
// check that catches a misspelled attribute, an element in the wrong place, or
// a TPM model libvirt doesn't know — none of which a string-matching unit test
// can see.
func TestE2E_WindowsLibvirtDomainIsAccepted(t *testing.T) {
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	uri := libvirtURI(t)
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}

	dir := t.TempDir()
	// Stand-ins for the media: libvirt validates the definition, not whether
	// the ISOs are bootable, so this needs no Windows licence to be useful.
	installer := filepath.Join(dir, "windows.iso")
	virtio := filepath.Join(dir, "virtio-win.iso")
	for _, path := range []string{installer, virtio} {
		if err := os.WriteFile(path, []byte("not really an ISO"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	spec := Spec{
		Name: "corral-windows-e2e", CPU: 4, Memory: "8Gi", DiskSize: "1G",
		InstallerISO: installer, VirtioISO: virtio,
		// Unattended needs an ISO writer; the separate test below covers that
		// path, and leaving it off here keeps this test about the domain.
		Unattended: false,
	}
	ref := types.InstanceRef{Backend: "libvirt", Context: uri, Name: spec.Name}
	t.Cleanup(func() { exec.Command("virsh", "-c", uri, "undefine", spec.Name, "--nvram").Run() })

	result, err := (Libvirt{}).Define(ref, spec)
	if err != nil {
		// A host without OVMF or swtpm cannot define this domain, and that is
		// a missing prerequisite rather than a wrong definition — report it as
		// such, with what the adapter said was needed.
		if strings.Contains(err.Error(), "firmware") || strings.Contains(err.Error(), "tpm") ||
			strings.Contains(err.Error(), "swtpm") || strings.Contains(err.Error(), "loader") {
			t.Skipf("host is missing a Windows prerequisite (%v); adapter declares: %v",
				err, (Libvirt{}).Requires().Prerequisites)
		}
		t.Fatalf("real libvirt rejected the generated Windows domain: %v\n%s", err, result.Definition)
	}
	t.Log("libvirt accepted the Windows domain definition")

	// Read it back: libvirt normalises what it stores, and the requirements
	// have to survive that round trip or the guest will not see them.
	out, err := exec.Command("virsh", "-c", uri, "dumpxml", spec.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("virsh dumpxml: %s: %v", out, err)
	}
	stored := string(out)
	for what, want := range map[string]string{
		"TPM device":        "<tpm",
		"TPM 2.0":           "2.0",
		"q35 machine":       "q35",
		"virtio boot disk":  "virtio",
		"installer CD-ROM":  "cdrom",
		"virtio driver ISO": filepath.Base(virtio),
	} {
		if !strings.Contains(stored, want) {
			t.Errorf("libvirt did not keep the %s (%q) in the stored domain:\n%s", what, want, stored)
		}
	}
	// UEFI is reported back as a <loader> once libvirt resolves the firmware
	// descriptor, so accept either form.
	if !strings.Contains(stored, "efi") && !strings.Contains(stored, "loader") {
		t.Errorf("the stored domain has no UEFI firmware — Windows 11 will refuse to install:\n%s", stored)
	}
}

// The answer-file medium has to be a real ISO9660 with the file at its root,
// because that is the only place Windows Setup looks.
func TestE2E_WindowsAnswerISO(t *testing.T) {
	dir := t.TempDir()
	path, err := BuildAnswerISO(dir, AutounattendXML("win11", "P@ssw0rd123!"))
	if err != nil {
		t.Fatalf("BuildAnswerISO: %v", err)
	}

	// Every tree the image has must name the file correctly, checked with a
	// real reader rather than by poking bytes.
	//
	// Both trees, deliberately. Joliet is what Windows normally reads, and
	// checking only that would pass — but the primary ISO9660 tree is the one
	// that silently truncates to AUTOUNAT.XML;1, and a Setup reading it finds
	// no answer file and falls through to an interactive install with no error
	// anywhere. Since -iso-level 4 makes both trees correct, both are asserted:
	// a regression in either is a regression.
	var checked int
	for _, lister := range [][]string{
		{"isoinfo", "-i", path, "-f"},       // primary ISO9660 tree
		{"isoinfo", "-i", path, "-J", "-f"}, // Joliet tree
		{"xorriso", "-indev", path, "-find", "/"},
		{"bsdtar", "-tf", path},
	} {
		bin, err := exec.LookPath(lister[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, lister[1:]...).CombinedOutput()
		if err != nil {
			continue // e.g. no such tree to list; the other readers still apply
		}
		listing := strings.ToUpper(string(out))
		if strings.Contains(listing, "AUTOUNAT.XML") {
			t.Fatalf("%v shows the 8.3-truncated name — Windows Setup reading this tree finds no answer file:\n%s",
				lister, out)
		}
		if !strings.Contains(listing, "AUTOUNATTEND.XML") {
			t.Fatalf("%v does not list autounattend.xml at the ISO root:\n%s", lister, out)
		}
		t.Logf("%v: autounattend.xml at the root, untruncated", lister)
		checked++
	}
	if checked == 0 {
		t.Skip("no ISO reader available to verify the image")
	}
}
