// Package windows holds what a Windows guest needs, independent of which
// backend runs it (#132).
//
// The answer file was already backend-neutral — it is XML that Windows Setup
// reads, with nothing Kubernetes about it — it just lived in pkg/kubevirt. The
// genuinely per-backend parts are three:
//
//   - **Firmware.** Windows 11 and Server 2025 require UEFI with Secure Boot
//     capability and a TPM 2.0. Get this wrong and Setup refuses at its
//     compatibility check, which is a slow and confusing way to find out.
//   - **Driver delivery.** Windows Setup cannot see a virtio disk without
//     drivers loaded from a second medium. KubeVirt attaches a containerdisk,
//     libvirt attaches the virtio-win ISO from the host, Incus requires the
//     drivers slipstreamed into the installer image beforehand.
//   - **Answer-file delivery.** Setup reads autounattend.xml from the root of
//     any attached removable medium. KubeVirt builds that ISO from a ConfigMap;
//     everywhere else Corral builds a small ISO on the host.
//
// This package models those three so a backend adapter is a description of
// media and firmware rather than a pile of conditionals.
package windows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VirtioWinStableISO is the official virtio-win driver ISO, published by the
// Fedora virt group — the same artifact Red Hat ships with RHEL. Corral does
// not mirror it: pointing at upstream means a user can verify what they are
// downloading, and the "stable" channel corresponds to the drivers shipped in
// the most recent RHEL rather than a pre-release build.
const VirtioWinStableISO = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"

// Firmware is what the guest needs from the platform.
type Firmware struct {
	// UEFI is required for Windows 11 and Server 2022+. BIOS boot is only
	// viable for Windows 10 and older.
	UEFI bool
	// SecureBoot capability — the installer's compatibility check looks for
	// it even when it is not enforced.
	SecureBoot bool
	// TPM 2.0, likewise required by Windows 11's check.
	TPM bool
}

// Win11 is the firmware profile modern Windows requires. Named for the
// version whose compatibility check introduced the requirements; Server 2022
// and 2025 want the same.
func Win11() Firmware { return Firmware{UEFI: true, SecureBoot: true, TPM: true} }

// DriverDelivery says how virtio drivers reach Windows Setup.
type DriverDelivery string

const (
	// SecondCDROM attaches the virtio-win ISO as an extra CD-ROM. Setup's
	// "Load driver" step — or the answer file — picks the drivers up from it.
	SecondCDROM DriverDelivery = "second-cdrom"
	// Slipstreamed means the drivers must already be inside the installer
	// image. Incus needs this: it gives a VM one CD-ROM, so there is nowhere
	// to attach a second one during Setup.
	Slipstreamed DriverDelivery = "slipstreamed"
)

// Spec is everything a backend needs to stand up a Windows install.
type Spec struct {
	Name          string
	CPU           int
	Memory        string
	DiskSize      string
	InstallerISO  string // path or URL to the Windows installation media
	VirtioISO     string // path to virtio-win.iso, when the backend attaches one
	Unattended    bool
	AdminPassword string // generated when empty and Unattended is set
}

// Requirements describes what a backend needs to satisfy for a spec. Adapters
// report this so a caller can be told what is missing before anything is
// created, rather than after a 40-minute install fails its final check.
type Requirements struct {
	Firmware Firmware
	Drivers  DriverDelivery
	// AnswerFileMedium names how autounattend.xml is delivered, for the
	// caller's benefit in errors and docs.
	AnswerFileMedium string
	// Prerequisites are host tools or artifacts the backend needs, named so a
	// missing one is actionable.
	Prerequisites []string
}

// ── answer-file media ─────────────────────────────────────────────

// isoArgs are the options every ISO writer here is given.
//
//	-J            Joliet, which is what Windows normally reads for long names
//	-r            Rock Ridge, so the file is also sane on Linux
//	-iso-level 4  keeps the name untruncated in the *primary* ISO9660 tree
//	-V UNATTEND   a volume label, purely so the medium is identifiable
//
// -iso-level 4 is the load-bearing one. With -J -r alone, inspecting a real
// image shows the primary tree holding the 8.3-mangled "AUTOUNAT.XML;1" while
// only Joliet carries the real name. Windows Setup does normally read Joliet,
// so that arrangement usually works — but "usually" is doing a lot of work for
// a failure whose symptom is an unattended install silently becoming an
// interactive one, with no error anywhere. -iso-level 4 costs nothing and
// puts AUTOUNATTEND.XML;1 in the primary tree too (Setup matches
// case-insensitively), so neither tree can be the one that lets it down.
var isoArgs = []string{"-J", "-r", "-iso-level", "4", "-V", "UNATTEND"}

// isoTools are the ISO writers Corral will use to build the answer-file
// medium, in preference order. Corral shells out rather than hand-rolling
// ISO9660: a subtly malformed image fails inside Windows Setup, where there is
// no useful diagnostic, and every distribution ships at least one of these.
var isoTools = []struct {
	binary string
	args   func(out, dir string) []string
}{
	{"xorriso", func(out, dir string) []string {
		return append(append([]string{"-as", "mkisofs"}, isoArgs...), "-o", out, dir)
	}},
	{"genisoimage", func(out, dir string) []string {
		return append(append([]string{}, isoArgs...), "-o", out, dir)
	}},
	{"mkisofs", func(out, dir string) []string {
		return append(append([]string{}, isoArgs...), "-o", out, dir)
	}},
}

// MissingISOTool reports that no ISO writer is available. Typed so a caller
// can turn it into a capability refusal rather than a generic failure.
type MissingISOTool struct{}

func (MissingISOTool) Error() string {
	return "no ISO writer found (need one of xorriso, genisoimage, or mkisofs) — " +
		"install xorriso to build the unattended-install medium"
}

// BuildAnswerISO writes an ISO containing a single autounattend.xml at its
// root, which is exactly where Windows Setup looks. Returns the path written.
func BuildAnswerISO(dir, xml string) (string, error) {
	staging, err := os.MkdirTemp("", "corral-unattend-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	// The filename is load-bearing: Setup looks for this exact name.
	if err := os.WriteFile(filepath.Join(staging, "autounattend.xml"), []byte(xml), 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "autounattend.iso")
	for _, tool := range isoTools {
		bin, err := exec.LookPath(tool.binary)
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, tool.args(out, staging)...)
		if combined, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("%s: %s", tool.binary, strings.TrimSpace(string(combined)))
		}
		return out, nil
	}
	return "", MissingISOTool{}
}

// EnsureVirtioISO returns a local path to virtio-win.iso, downloading it once
// into cacheDir if it is not already there. Windows Setup cannot see a virtio
// disk without it, so "the install hangs at disk selection" is the failure
// this prevents.
func EnsureVirtioISO(cacheDir string, download func(url, dest string) error) (string, error) {
	path := filepath.Join(cacheDir, "virtio-win.iso")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	if download == nil {
		return "", fmt.Errorf("virtio-win.iso is not cached at %s and no downloader was provided; "+
			"fetch it from %s", path, VirtioWinStableISO)
	}
	if err := download(VirtioWinStableISO, path); err != nil {
		return "", fmt.Errorf("downloading virtio-win.iso: %w", err)
	}
	return path, nil
}

// writeTemp writes content to a temp file and returns its path plus a cleanup.
func writeTemp(content, pattern string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
