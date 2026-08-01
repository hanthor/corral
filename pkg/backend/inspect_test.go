package backend

// Inspection tests: what each backend reports about a guest's firmware, and
// what happens when it cannot say. The firmware answer decides a refusal in
// pkg/move, so a wrong one here is a VM that boots to a blank screen.

import (
	"testing"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func withLibvirtFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	libvirt.SetRunner(fake)
	t.Cleanup(func() { libvirt.SetRunner(shell.Real{}) })
	return fake
}

func withIncusFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	incus.SetRunner(fake)
	t.Cleanup(func() { incus.SetRunner(shell.Real{}) })
	return fake
}

func TestLibvirtInspectFindsEFIInBothSpellings(t *testing.T) {
	for name, xml := range map[string]string{
		"modern attribute": `<domain><os firmware='efi'><type>hvm</type></os></domain>`,
		"double quotes":    `<domain><os firmware="efi"><type>hvm</type></os></domain>`,
		"explicit loader":  `<domain><os><loader readonly='yes'>/usr/share/OVMF/OVMF_CODE.fd</loader></os></domain>`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := withLibvirtFake(t)
			fake.AddPrefixResponse("virsh", xml, nil)
			info, err := (libvirtAdapter{client: libvirt.NewClient("qemu:///system")}).GuestInfo("web")
			if err != nil {
				t.Fatal(err)
			}
			if !info.UEFI {
				t.Fatalf("%s was not recognised as EFI", name)
			}
		})
	}
}

// A BIOS domain must report BIOS. Over-reporting EFI would refuse moves that
// would have worked, which is a quieter failure but still a wrong one.
func TestLibvirtInspectReportsBIOS(t *testing.T) {
	fake := withLibvirtFake(t)
	fake.AddPrefixResponse("virsh", `<domain><os><type arch='x86_64'>hvm</type></os></domain>`, nil)
	info, err := (libvirtAdapter{client: libvirt.NewClient("qemu:///system")}).GuestInfo("web")
	if err != nil {
		t.Fatal(err)
	}
	if info.UEFI {
		t.Fatal("a SeaBIOS domain was reported as EFI")
	}
}

// An Incus VM is always UEFI — Incus boots them under OVMF with no BIOS
// option. It is the one backend where the answer is a property of the backend,
// and it is load-bearing: it makes incus → qemu a refusal rather than a guest
// that imports cleanly and then shows a blank screen.
func TestIncusInspectReportsVMsAsUEFIAndContainersAsUnknown(t *testing.T) {
	fake := withIncusFake(t)
	fake.AddPrefixResponse("incus list", `[
		{"name":"vm-one","type":"virtual-machine","status":"Running"},
		{"name":"ct-one","type":"container","status":"Running"}
	]`, nil)

	adapter := incusAdapter{client: incus.NewClient("local")}
	vm, err := adapter.GuestInfo("vm-one")
	if err != nil {
		t.Fatal(err)
	}
	if !vm.UEFI {
		t.Error("an Incus VM boots under OVMF and must be reported as UEFI")
	}
	ct, err := adapter.GuestInfo("ct-one")
	if err != nil {
		t.Fatal(err)
	}
	if ct.UEFI {
		t.Error("a container has no firmware; reporting UEFI would be nonsense")
	}
}

// Inspect swallows errors on purpose: a config read that failed is not a claim
// that the guest is BIOS, and every caller's next move is the same either way.
func TestInspectReturnsUnknownRatherThanFailing(t *testing.T) {
	fake := withLibvirtFake(t)
	fake.AddPrefixResponse("virsh", "", errString("virsh: failed to connect"))

	got := Inspect(types.InstanceRef{Backend: "libvirt", Name: "web"})
	if got != (GuestInfo{}) {
		t.Fatalf("a failed inspection should be unknown, got %+v", got)
	}
	if got := Inspect(types.InstanceRef{Backend: "vmware", Name: "web"}); got != (GuestInfo{}) {
		t.Fatalf("an unregistered backend should be unknown, got %+v", got)
	}
}

// Which backends can answer at all — the four that record firmware somewhere.
// qemu is the gap: its generated unit has no firmware line to read.
func TestInspectorsAreImplementedWhereTheBackendRecordsFirmware(t *testing.T) {
	for backendName, want := range map[string]bool{
		"kubevirt": true, "libvirt": true, "proxmox": true, "incus": true,
		"qemu": false,
	} {
		adapter, err := probe(backendName)
		if err != nil {
			t.Fatalf("probing %s: %v", backendName, err)
		}
		_, ok := adapter.(Inspector)
		if ok != want {
			t.Errorf("%s implements Inspector = %v, want %v", backendName, ok, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
