//go:build e2esnapshot

// Real-tool verification of the device adapters (#129).
//
// This one matters more than the other e2e suites, because the passthrough
// adapters were written without hardware to try them on: the command syntax
// and the output shapes are researched, not observed. The unit tests can only
// prove the parsers agree with fixtures *I* wrote — which is exactly the
// failure mode this file exists to close.
//
// What it can verify without a passthrough-capable GPU:
//
//   - discovery parses what real `virsh nodedev-dumpxml` and real
//     `incus query /1.0/resources` actually emit, on a host with neither
//     curated nor convenient devices;
//   - the generated <hostdev> XML is accepted by a real libvirt, which
//     validates it at attach-config time;
//   - the Incus device syntax is accepted by a real Incus, or rejected for a
//     reason that is about the device rather than about the keys.
//
// What it cannot verify, and does not pretend to: that a guest actually sees
// the device after a start. That needs a spare GPU.
//
// Run with: go test -tags e2esnapshot ./pkg/device/

package device

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func realRunner(t *testing.T) {
	t.Helper()
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
}

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

// Discovery against a real host's PCI bus. Any machine has PCI devices, so
// this exercises the real nodedev-dumpxml schema — the part most likely to be
// wrong, since the parser was written from documentation.
func TestE2E_LibvirtDeviceDiscovery(t *testing.T) {
	realRunner(t)
	uri := libvirtURI(t)

	devices, err := (Libvirt{}).List(uri)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("no PCI devices discovered on a real host — the nodedev parse is wrong")
	}
	t.Logf("discovered %d PCI devices", len(devices))

	// Every device must come back with a parseable address and some identity,
	// or the parse silently produced empty structs.
	var withVendor int
	for _, d := range devices {
		if d.Address == "" {
			t.Errorf("device has no address: %+v", d)
		}
		if d.Attachable {
			if _, err := hostdevXML(d, "probe"); err != nil {
				t.Errorf("discovered device %q produced an address hostdevXML rejects: %v", d.Address, err)
			}
		}
		if d.Vendor != "" {
			withVendor++
		}
	}
	if withVendor == 0 {
		t.Error("not one device reported a vendor ID — the XML field names are wrong")
	}
	for i, d := range devices {
		if i >= 5 {
			break
		}
		t.Logf("  %s  %-8s %s:%s  driver=%-12s class=%s",
			d.Address, d.Class, d.Vendor, d.Product, d.Driver, d.Description)
	}
}

// The generated <hostdev> has to be something libvirt will accept. libvirt
// validates the XML when it is attached to a domain's config, so this proves
// the element is well-formed and schema-valid without ever starting the domain
// — nothing is bound to vfio, and the host keeps its devices.
func TestE2E_LibvirtHostdevXMLIsAccepted(t *testing.T) {
	realRunner(t)
	uri := libvirtURI(t)

	devices, err := (Libvirt{}).List(uri)
	if err != nil || len(devices) == 0 {
		t.Skipf("no devices to build a hostdev from: %v", err)
	}
	dev, reason := attachableDevice(devices)
	if reason != "" {
		t.Skip(reason)
	}

	dir := t.TempDir()
	const domain = "corral-device-e2e"
	xmlPath := filepath.Join(dir, "domain.xml")
	os.WriteFile(xmlPath, []byte(`<domain type='qemu'>
  <name>`+domain+`</name><memory unit='MiB'>128</memory><vcpu>1</vcpu>
  <os><type arch='x86_64'>hvm</type></os>
</domain>`), 0o644)
	if out, err := exec.Command("virsh", "-c", uri, "define", xmlPath).CombinedOutput(); err != nil {
		t.Skipf("cannot define a domain here: %s: %v", out, err)
	}
	t.Cleanup(func() { exec.Command("virsh", "-c", uri, "undefine", domain).Run() })

	ref := types.InstanceRef{Backend: "libvirt", Context: uri, Name: domain}
	if err := (Libvirt{}).Attach(ref, dev, "probe0"); err != nil {
		t.Fatalf("real libvirt rejected the generated hostdev XML for %s: %v", dev.Address, err)
	}
	t.Logf("libvirt accepted a hostdev for %s (%s)", dev.Address, dev.Description)

	// It has to read back, or Detach can never find it again.
	attached, err := (Libvirt{}).Attached(ref)
	if err != nil {
		t.Fatalf("Attached: %v", err)
	}
	var found bool
	for _, a := range attached {
		if a.Name == "probe0" {
			found = true
			if a.Device != dev.Address {
				t.Errorf("read back address %q, want %q", a.Device, dev.Address)
			}
		}
	}
	if !found {
		t.Fatalf("the attachment did not read back: %+v", attached)
	}

	if err := (Libvirt{}).Detach(ref, "probe0"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	after, err := (Libvirt{}).Attached(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range after {
		if a.Name == "probe0" {
			t.Error("the device is still attached after Detach")
		}
	}
}

// Discovery against a real Incus. The resources payload's field names were
// taken from documentation and forum output, never observed — so this asserts
// they are right, and logs the raw shape when they are not.
func TestE2E_IncusDeviceDiscovery(t *testing.T) {
	realRunner(t)
	if _, err := exec.LookPath("incus"); err != nil {
		t.Skip("incus not installed")
	}
	if exec.Command("incus", "query", "local:/1.0").Run() != nil {
		t.Skip("no reachable local Incus remote")
	}

	devices, err := (Incus{}).List("local")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) == 0 {
		// Dump what Incus really returned, so a field-name mismatch is
		// diagnosable from the CI log rather than by guessing again.
		raw, _ := exec.Command("incus", "query", "local:/1.0/resources").Output()
		var pretty map[string]json.RawMessage
		json.Unmarshal(raw, &pretty)
		keys := make([]string, 0, len(pretty))
		for k := range pretty {
			keys = append(keys, k)
		}
		t.Fatalf("no PCI devices parsed from a real Incus. Top-level keys were %v.\nFirst 800 bytes:\n%s",
			keys, truncate(string(raw), 800))
	}
	t.Logf("discovered %d devices via Incus", len(devices))
	var withVendor int
	for _, d := range devices {
		// A device with no PCI address is legitimate — a cloud VM's synthetic
		// GPU is a VMBus device — but it must be marked unattachable with a
		// reason, not offered as something that can be passed through.
		if d.Address == "" {
			if d.Attachable {
				t.Errorf("device with no PCI address reported as attachable: %+v", d)
			}
			if d.Unattachable == "" {
				t.Errorf("device with no PCI address gives no reason: %+v", d)
			}
			if d.ID == "" {
				t.Errorf("device with no PCI address has no identifier at all: %+v", d)
			}
		} else if !d.Attachable {
			t.Errorf("device with a PCI address reported as unattachable: %+v", d)
		}
		if d.Vendor != "" {
			withVendor++
		}
	}
	if withVendor == 0 {
		t.Error("not one device reported a vendor ID — the resources field names are wrong")
	}
	for i, d := range devices {
		if i >= 5 {
			break
		}
		t.Logf("  %s  %-8s %s:%s  driver=%-12s %s",
			d.Address, d.Class, d.Vendor, d.Product, d.Driver, d.Description)
	}
}

// The Incus device syntax has to be one Incus recognises. A wrong key name
// fails with "unknown device option"; a valid key naming an unavailable device
// fails with something about the device. Both are informative, and only the
// first means the adapter is wrong.
func TestE2E_IncusDeviceSyntaxIsRecognised(t *testing.T) {
	realRunner(t)
	if _, err := exec.LookPath("incus"); err != nil {
		t.Skip("incus not installed")
	}
	if exec.Command("incus", "query", "local:/1.0").Run() != nil {
		t.Skip("no reachable local Incus remote")
	}

	devices, err := (Incus{}).List("local")
	if err != nil || len(devices) == 0 {
		t.Skipf("no devices to try: %v", err)
	}
	dev, reason := attachableDevice(devices)
	if reason != "" {
		t.Skip(reason)
	}

	const instance = "corral-device-e2e"
	// init, not launch: the instance is never started, so nothing is ever
	// bound away from the host.
	if out, err := exec.Command("incus", "init", "images:ubuntu/22.04", instance).CombinedOutput(); err != nil {
		t.Skipf("cannot init an Incus instance here: %s: %v", out, err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", instance).Run() })

	ref := types.InstanceRef{Backend: "incus", Context: "local", Name: instance}
	err = (Incus{}).Attach(ref, dev, "probe0")
	if err == nil {
		t.Logf("Incus accepted the device config for %s (%s)", dev.Address, dev.Class)
		attached, listErr := (Incus{}).Attached(ref)
		if listErr != nil {
			t.Fatalf("Attached: %v", listErr)
		}
		var found bool
		for _, a := range attached {
			if a.Name == "probe0" {
				found = true
				if a.Device != dev.Address {
					t.Errorf("read back address %q, want %q", a.Device, dev.Address)
				}
			}
		}
		if !found {
			t.Errorf("the device did not read back: %+v", attached)
		}
		if err := (Incus{}).Detach(ref, "probe0"); err != nil {
			t.Errorf("Detach: %v", err)
		}
		return
	}

	// Rejected — the question is whether Incus objected to the *keys* (adapter
	// bug) or to the device (expected on a runner with no passthrough setup).
	msg := strings.ToLower(err.Error())
	for _, keyError := range []string{
		"unknown device option", "invalid device option",
		"invalid device type", "unknown device type",
	} {
		if strings.Contains(msg, keyError) {
			t.Fatalf("Incus rejected the adapter's device syntax, not the device: %v", err)
		}
	}
	t.Logf("Incus rejected the device itself, which is expected without passthrough set up: %v", err)
}

// attachableDevice picks something safe to probe with, and says precisely why
// when there is nothing. The distinction matters: "all boot display" and "none
// has a PCI address" are different facts, and conflating them hid a real bug
// where a synthetic GPU came back with no address at all.
func attachableDevice(devices []Device) (Device, string) {
	var boot, unattachable int
	for _, d := range devices {
		switch {
		case d.BootDisplay:
			boot++
		case !d.Attachable:
			unattachable++
		default:
			return d, ""
		}
	}
	return Device{}, fmt.Sprintf(
		"no device here can be safely probed: %d of %d are the host boot display, %d report no PCI address",
		boot, len(devices), unattachable)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
