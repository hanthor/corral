package doctor

import (
	"errors"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
)

func TestRunContexts_BackendSpecificProbes(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query lab:/1.0", `{}`, nil)
	fake.AddResponse("incus info lab:", `instance_types: [container, virtual-machine]`, nil)
	fake.AddResponse("virsh -c qemu+ssh://hv/system connect", "", nil)
	fake.AddResponse("virsh -c qemu+ssh://hv/system nodeinfo", "CPU model: x86_64", nil)
	fake.AddResponse("virsh -c qemu+ssh://hv/system pool-list --all", "default active", nil)
	fake.AddResponse("virsh -c qemu+ssh://hv/system net-list --all", "default active", nil)

	checks := RunContexts([]config.ContextConfig{
		{Name: "containers", Backend: "incus", Context: "lab"},
		{Name: "hypervisor", Backend: "libvirt", Context: "qemu+ssh://hv/system"},
	})
	for _, c := range checks {
		if !c.OK {
			t.Errorf("%s/%s %q failed: %s", c.Context, c.Backend, c.Name, c.Detail)
		}
		if c.Context == "" || c.Backend == "" || c.Severity == "" {
			t.Errorf("unscoped result: %+v", c)
		}
	}
}

func TestRunContexts_UnreachableContextDoesNotHideNext(t *testing.T) {
	fake := withFake(t)
	fake.AddResponse("incus query broken:/1.0", "connection refused", errors.New("exit 1"))
	fake.AddResponse("virsh -c qemu:///session connect", "", nil)
	fake.AddResponse("virsh -c qemu:///session nodeinfo", "ok", nil)
	fake.AddResponse("virsh -c qemu:///session pool-list --all", "ok", nil)
	fake.AddResponse("virsh -c qemu:///session net-list --all", "ok", nil)

	checks := RunContexts([]config.ContextConfig{
		{Name: "broken", Backend: "incus", Context: "broken"},
		{Name: "working", Backend: "libvirt", Context: "qemu:///session"},
	})
	if c := checkByName(t, checks, "Incus remote reachable"); c.OK || c.Context != "broken" {
		t.Fatalf("unexpected unreachable result: %+v", c)
	}
	foundWorking := false
	for _, c := range checks {
		foundWorking = foundWorking || c.Context == "working" && c.OK
	}
	if !foundWorking {
		t.Fatal("healthy context was not diagnosed after another context failed")
	}
}
