package lifecycle

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/types"
)

func TestDoRejectsAnUnknownAction(t *testing.T) {
	err := Do(types.InstanceRef{Backend: "qemu", Name: "dev"}, Action("explode"))
	if err == nil || !strings.Contains(err.Error(), "unknown lifecycle action") {
		t.Fatalf("want an unknown-action error, got %v", err)
	}
}

func TestDoRequiresAFullyScopedReference(t *testing.T) {
	if err := Do(types.InstanceRef{Name: "dev"}, Start); err == nil {
		t.Fatal("a reference with no backend was accepted")
	}
	if err := Do(types.InstanceRef{Backend: "qemu"}, Start); err == nil {
		t.Fatal("a reference with no name was accepted")
	}
}

// A scheduler pointed at "qemu in context prod" is misconfigured. Powering the
// local VM of that name would be the wrong recovery — it is a different
// machine than the one the entry meant.
func TestDoRefusesAContextOnTheLocalBackend(t *testing.T) {
	err := Do(types.InstanceRef{Backend: "qemu", Context: "prod", Name: "dev"}, Stop)
	if err == nil || !strings.Contains(err.Error(), "no contexts") {
		t.Fatalf("want a refusal naming the problem, got %v", err)
	}
}

func TestDoRejectsAnUnknownBackend(t *testing.T) {
	err := Do(types.InstanceRef{Backend: "vmware", Name: "dev"}, Start)
	if err == nil {
		t.Fatal("an unregistered backend was accepted")
	}
}

// Supported is asked of the operation contract rather than a second table, so
// it cannot claim more than Do can deliver. Proxmox is the test that matters:
// it powers guests through the contract and was invisible to this package
// while the switch was hand-written here.
func TestSupportedMatchesTheContract(t *testing.T) {
	for _, name := range []string{"kubevirt", "qemu", "incus", "libvirt", "proxmox"} {
		if !Supported(name) {
			t.Errorf("%s implements Power and should be schedulable", name)
		}
	}
	if Supported("vmware") {
		t.Error("an unimplemented backend reported as schedulable")
	}
}
