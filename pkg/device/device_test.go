package device

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

// fakeBootVGA redirects the boot-display check at a temp sysfs, so the refusal
// can be tested without a machine that actually has one.
func fakeBootVGA(t *testing.T, bootAddress string) {
	t.Helper()
	dir := t.TempDir()
	if bootAddress != "" {
		devDir := filepath.Join(dir, bootAddress)
		if err := os.MkdirAll(devDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(devDir, "boot_vga"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	original := bootVGAPath
	bootVGAPath = func(address string) string { return filepath.Join(dir, address, "boot_vga") }
	t.Cleanup(func() { bootVGAPath = original })
}

// ── contract ──────────────────────────────────────────────────────

func TestFor_ImplementedBackends(t *testing.T) {
	for _, backend := range []string{"kubevirt", "libvirt", "incus"} {
		if _, err := For(backend); err != nil {
			t.Errorf("no adapter for %q: %v", backend, err)
		}
		if !Supported(backend) {
			t.Errorf("Supported(%q) = false but an adapter exists", backend)
		}
	}
}

// The local QEMU backend builds its own command line with no vfio wiring.
// Emitting a device it cannot honour would be worse than saying so.
func TestFor_LocalQemuRefusesWithAReason(t *testing.T) {
	_, err := For("qemu")
	var u *Unsupported
	if !errors.As(err, &u) {
		t.Fatalf("want *Unsupported, got %T: %v", err, err)
	}
	if !strings.Contains(u.Remedy, "libvirt") {
		t.Errorf("refusal offers no alternative: %q", u.Remedy)
	}
	if Supported("qemu") {
		t.Error("Supported reported a backend that has no adapter")
	}
}

// ── the safety gate ───────────────────────────────────────────────

// This is the single most important test in the package. Passing through the
// device driving the host's console leaves the machine dark, recoverable only
// with physical access.
func TestAttach_RefusesTheHostBootDisplay(t *testing.T) {
	fake := withFake(t)
	fakeBootVGA(t, "0000:00:02.0")

	boot := Device{ID: "0000:00:02.0", Address: "0000:00:02.0", Class: GPU,
		Description: "Intel Arc Graphics", BootDisplay: isBootDisplay("0000:00:02.0")}
	if !boot.BootDisplay {
		t.Fatal("fixture did not mark the device as the boot display")
	}

	for name, adapter := range map[string]Adapter{
		"libvirt":  Libvirt{},
		"incus":    Incus{},
		"kubevirt": KubeVirt{},
	} {
		t.Run(name, func(t *testing.T) {
			err := adapter.Attach(types.InstanceRef{Backend: name, Name: "vm"}, boot, "gpu0")
			var refused *Refused
			if !errors.As(err, &refused) {
				t.Fatalf("want *Refused, got %T: %v", err, err)
			}
			if !strings.Contains(refused.Error(), "console") {
				t.Errorf("refusal does not explain the consequence: %v", refused)
			}
		})
	}
	for _, call := range fake.Calls() {
		t.Errorf("a refused attachment still ran a command: %s %v", call.Name, call.Args)
	}
}

// A second GPU on the same host is not the boot display and must remain
// attachable — the safety gate has to be narrow or it is useless.
func TestAttach_AllowsANonBootGPU(t *testing.T) {
	fake := withFake(t)
	fakeBootVGA(t, "0000:00:02.0")
	fake.AddPrefixResponse("virsh", "", nil)

	second := Device{ID: "0000:01:00.0", Address: "0000:01:00.0", Class: GPU,
		BootDisplay: isBootDisplay("0000:01:00.0")}
	if second.BootDisplay {
		t.Fatal("a second GPU was wrongly marked as the boot display")
	}
	if err := (Libvirt{}).Attach(types.InstanceRef{Backend: "libvirt", Name: "vm"}, second, "gpu0"); err != nil {
		t.Fatalf("attaching a non-boot GPU failed: %v", err)
	}
}

// Consequences must reach the caller before mutation — the issue asks for
// restart/migration/security implications to be surfaced, not logged after.
func TestConsequences_CoverRestartMigrationAndDMA(t *testing.T) {
	dev := Device{ID: "0000:01:00.0", Address: "0000:01:00.0", Class: GPU, Driver: "amdgpu"}
	got := (Libvirt{}).Consequences(dev)

	want := map[Consequence]bool{
		RequiresRestart: false, BlocksMigration: false,
		GuestGetsDMA: false, HostLosesDevice: false,
	}
	for _, c := range got {
		want[c] = true
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("consequence not surfaced: %s", c)
		}
	}
}

// A device already bound to vfio-pci is not in use by the host, so the "host
// loses it" warning would be noise.
func TestConsequences_OmitHostLossWhenAlreadyOnVfio(t *testing.T) {
	dev := Device{ID: "0000:01:00.0", Address: "0000:01:00.0", Class: GPU, Driver: "vfio-pci"}
	for _, c := range (Libvirt{}).Consequences(dev) {
		if c == HostLosesDevice {
			t.Error("warned about host loss for a device already on vfio-pci")
		}
	}
}

// On a cluster the device plugin already owns the device, so nothing is taken
// away from a host that was using it.
func TestConsequences_KubeVirtDoesNotClaimHostLoss(t *testing.T) {
	for _, c := range (KubeVirt{}).Consequences(Device{ID: "amd.com/gpu", Driver: "amdgpu"}) {
		if c == HostLosesDevice {
			t.Error("KubeVirt warned about host device loss, which it cannot know")
		}
	}
}

// ── libvirt ───────────────────────────────────────────────────────

func TestLibvirt_ParsesNodedevXML(t *testing.T) {
	fake := withFake(t)
	fakeBootVGA(t, "")
	fake.AddResponse("virsh -c qemu:///system nodedev-list --cap pci", "pci_0000_01_00_0\n", nil)
	fake.AddResponse("virsh -c qemu:///system nodedev-dumpxml pci_0000_01_00_0", `<device>
  <name>pci_0000_01_00_0</name>
  <driver><name>vfio-pci</name></driver>
  <capability type='pci'>
    <domain>0</domain><bus>1</bus><slot>0</slot><function>0</function>
    <product id='0x2204'>GA102 [GeForce RTX 3090]</product>
    <vendor id='0x10de'>NVIDIA Corporation</vendor>
  </capability>
</device>`, nil)

	devices, err := (Libvirt{}).List("qemu:///system")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	d := devices[0]
	if d.Address != "0000:01:00.0" {
		t.Errorf("address = %q, want 0000:01:00.0", d.Address)
	}
	if d.Vendor != "10de" || d.Product != "2204" {
		t.Errorf("vendor/product = %s/%s, want 10de/2204", d.Vendor, d.Product)
	}
	if d.Class != GPU {
		t.Errorf("class = %q, want gpu for a GeForce", d.Class)
	}
	if !d.Ready {
		t.Error("a device already on vfio-pci reported as not ready")
	}
}

// One unreadable device must not hide a host's other twenty.
func TestLibvirt_SkipsUnreadableDevices(t *testing.T) {
	fake := withFake(t)
	fakeBootVGA(t, "")
	fake.AddResponse("virsh -c qemu:///system nodedev-list --cap pci",
		"pci_0000_01_00_0\npci_0000_02_00_0\n", nil)
	fake.AddResponse("virsh -c qemu:///system nodedev-dumpxml pci_0000_01_00_0",
		"", errors.New("device vanished"))
	fake.AddResponse("virsh -c qemu:///system nodedev-dumpxml pci_0000_02_00_0", `<device>
  <name>pci_0000_02_00_0</name><driver><name>igb</name></driver>
  <capability type='pci'>
    <domain>0</domain><bus>2</bus><slot>0</slot><function>0</function>
    <product id='0x1533'>I210 Gigabit Network Connection</product>
    <vendor id='0x8086'>Intel Corporation</vendor>
  </capability>
</device>`, nil)

	devices, err := (Libvirt{}).List("qemu:///system")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Class != Network {
		t.Fatalf("got %+v, want just the readable network device", devices)
	}
}

func TestHostdevXML_RendersAValidSourceAddress(t *testing.T) {
	doc, err := hostdevXML(Device{Address: "0000:01:00.0"}, "gpu0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"managed='yes'",
		"domain='0x0000'", "bus='0x01'", "slot='0x00'", "function='0x0'",
		"ua-gpu0",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("hostdev XML missing %q:\n%s", want, doc)
		}
	}
}

func TestHostdevXML_RejectsAMalformedAddress(t *testing.T) {
	if _, err := hostdevXML(Device{Address: "not-an-address"}, "gpu0"); err == nil {
		t.Fatal("accepted an address that is not a PCI address")
	}
}

func TestLibvirt_AttachedRoundTripsTheAlias(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("virsh -c qemu:///system dumpxml vm", `<domain>
  <devices>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <source><address domain='0x0000' bus='0x01' slot='0x00' function='0x0'/></source>
      <alias name='ua-gpu0'/>
    </hostdev>
  </devices>
</domain>`, nil)

	attached, err := (Libvirt{}).Attached(types.InstanceRef{Backend: "libvirt", Context: "qemu:///system", Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != 1 {
		t.Fatalf("got %+v, want one attachment", attached)
	}
	if attached[0].Name != "gpu0" {
		t.Errorf("name = %q, want gpu0 (the ua- prefix should be stripped)", attached[0].Name)
	}
	if attached[0].Device != "0000:01:00.0" {
		t.Errorf("device = %q, want 0000:01:00.0", attached[0].Device)
	}
}

// ── Incus ─────────────────────────────────────────────────────────

func TestIncus_ParsesResources(t *testing.T) {
	fake := withFake(t)
	fakeBootVGA(t, "")
	fake.AddResponse("incus query lab:/1.0/resources", `{
	  "gpu": {"cards": [
	    {"driver":"nvidia","pci_address":"0000:01:00.0","vendor":"NVIDIA","vendor_id":"10de","product":"RTX 3090","product_id":"2204"}
	  ]},
	  "pci": {"devices": [
	    {"driver":"nvidia","pci_address":"0000:01:00.0","vendor":"NVIDIA","vendor_id":"10de","product":"RTX 3090","product_id":"2204"},
	    {"driver":"igb","pci_address":"0000:02:00.0","vendor":"Intel","vendor_id":"8086","product":"I210 Ethernet","product_id":"1533"}
	  ]}
	}`, nil)

	devices, err := (Incus{}).List("lab")
	if err != nil {
		t.Fatal(err)
	}
	// The GPU appears in both lists; it must be reported once, as a GPU.
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2 (the GPU deduplicated): %+v", len(devices), devices)
	}
	if devices[0].Class != GPU {
		t.Errorf("GPUs should sort first, got %+v", devices[0])
	}
	if devices[1].Class != Network {
		t.Errorf("second device class = %q, want network", devices[1].Class)
	}
}

func TestIncus_AttachUsesTheRightDeviceTypePerClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class Class
		want  string
	}{
		{"gpu", GPU, "incus config device add lab:vm dev0 gpu gputype=physical pci=0000:01:00.0"},
		{"other", Network, "incus config device add lab:vm dev0 pci address=0000:02:00.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := withFake(t)
			fake.AddPrefixResponse("incus", "", nil)
			address := "0000:01:00.0"
			if tc.class != GPU {
				address = "0000:02:00.0"
			}
			dev := Device{ID: address, Address: address, Class: tc.class}
			ref := types.InstanceRef{Backend: "incus", Context: "lab", Name: "vm"}
			if err := (Incus{}).Attach(ref, dev, "dev0"); err != nil {
				t.Fatal(err)
			}
			last := fake.LastCall()
			got := last.Name + " " + strings.Join(last.Args, " ")
			if got != tc.want {
				t.Errorf("ran %q,\n want %q", got, tc.want)
			}
		})
	}
}

func TestIncus_DetachOfAMissingDeviceIsNotAnError(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus config device remove lab:vm gone",
		"Error: Device doesn't exist", errors.New("exit 1"))
	if err := (Incus{}).Detach(types.InstanceRef{Backend: "incus", Context: "lab", Name: "vm"}, "gone"); err != nil {
		t.Errorf("removing an already-gone device failed: %v", err)
	}
}

func TestIncus_AttachedReadsBothDeviceTypes(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0/instances/vm", `{"devices":{
	  "gpu0": {"type":"gpu","gputype":"physical","pci":"0000:01:00.0"},
	  "nic0": {"type":"pci","address":"0000:02:00.0"},
	  "root": {"type":"disk","path":"/"}
	}}`, nil)

	attached, err := (Incus{}).Attached(types.InstanceRef{Backend: "incus", Context: "lab", Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != 2 {
		t.Fatalf("got %+v, want the two passthrough devices and not the disk", attached)
	}
	byName := map[string]string{}
	for _, a := range attached {
		byName[a.Name] = a.Device
	}
	if byName["gpu0"] != "0000:01:00.0" || byName["nic0"] != "0000:02:00.0" {
		t.Errorf("addresses did not round-trip: %+v", byName)
	}
}

// ── KubeVirt ──────────────────────────────────────────────────────

func TestKubeVirt_ListsOnlyPassthroughResources(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("kubectl get nodes -o json", `{"items":[{
	  "metadata":{"name":"node-1"},
	  "status":{"allocatable":{
	    "cpu":"8","memory":"32Gi","pods":"110",
	    "devices.kubevirt.io/kvm":"1000",
	    "nvidia.com/gpu":"2"
	  }}
	}]}`, nil)

	devices, err := (KubeVirt{}).List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %+v, want only nvidia.com/gpu", devices)
	}
	if devices[0].ID != "nvidia.com/gpu" {
		t.Errorf("id = %q", devices[0].ID)
	}
	if devices[0].Node != "node-1" {
		t.Errorf("node = %q, want node-1 — a cluster device is only usable where it is", devices[0].Node)
	}
	if devices[0].Class != GPU {
		t.Errorf("class = %q, want gpu", devices[0].Class)
	}
}

func TestClassify(t *testing.T) {
	for input, want := range map[string]Class{
		"NVIDIA Corporation GA102 [GeForce RTX 3090]": GPU,
		"Intel Corporation VGA compatible controller": GPU,
		"nvidia.com/gpu":                         GPU,
		"Intel Corporation I210 Gigabit Network": Network,
		"Intel HD Audio Controller":              Audio,
		"Samsung NVMe SSD Controller":            Storage,
		"Some Unknown Bridge":                    Other,
	} {
		if got := classify(input); got != want {
			t.Errorf("classify(%q) = %q, want %q", input, got, want)
		}
	}
}
