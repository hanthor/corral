package fleet

// Table-driven tests for Resolve — the pure selector logic that maps a
// user-supplied ID/name onto exactly one VM. The subtle cases (ID colliding
// with another VM's name, duplicate names across contexts) are the ones that
// matter and were previously only spot-covered.

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/types"
)

func TestResolveRequiresScopedIDForDuplicateNames(t *testing.T) {
	vms := []types.VM{{Name: "dev", Backend: "qemu", Namespace: "local"}, {Name: "dev", Backend: "kubevirt", Context: "lab", Namespace: "vms"}}
	for i := range vms {
		vms[i].SetIdentity()
	}
	if _, err := Resolve(vms, "dev"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	got, err := Resolve(vms, vms[1].ID)
	if err != nil || got.Context != "lab" {
		t.Fatalf("scoped resolve: %+v %v", got, err)
	}
}

func TestResolve_ExactID(t *testing.T) {
	vms := []types.VM{
		{ID: "qemu.local.dev", Name: "dev"},
		{ID: "kubevirt.lab.web", Name: "web"},
	}
	got, err := Resolve(vms, "qemu.local.dev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "dev" {
		t.Errorf("got %+v, want the dev VM", got)
	}
}

func TestResolve_UniqueName(t *testing.T) {
	vms := []types.VM{
		{ID: "qemu.local.dev", Name: "dev"},
		{ID: "kubevirt.lab.web", Name: "web"},
	}
	got, err := Resolve(vms, "web")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "kubevirt.lab.web" {
		t.Errorf("got %+v, want the web VM", got)
	}
}

func TestResolve_EmptyInventory(t *testing.T) {
	_, err := Resolve(nil, "anything")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("empty inventory: err = %v, want does-not-exist", err)
	}
}

func TestResolve_UnknownSelector(t *testing.T) {
	vms := []types.VM{{ID: "qemu.local.dev", Name: "dev"}}
	_, err := Resolve(vms, "nope")
	if err == nil || !strings.Contains(err.Error(), `"nope" does not exist`) {
		t.Fatalf("err = %v, want does-not-exist naming the selector", err)
	}
}

func TestResolve_DuplicateNameIsAmbiguous(t *testing.T) {
	vms := []types.VM{
		{ID: "qemu.local.dev", Name: "dev"},
		{ID: "kubevirt.lab.dev", Name: "dev"},
	}
	_, err := Resolve(vms, "dev")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous", err)
	}
}

func TestResolve_IDCollidingWithAnotherNameIsAmbiguous(t *testing.T) {
	// The selector matches VM 1's ID *and* VM 2's name — a bare selector is
	// only valid when it resolves to exactly one instance.
	vms := []types.VM{
		{ID: "web", Name: "webserver"},
		{ID: "qemu.local.other", Name: "web"},
	}
	_, err := Resolve(vms, "web")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous when an ID collides with a name", err)
	}
}

func TestResolve_PrefersFullyScopedID(t *testing.T) {
	// Same string is a *different* VM's name: the scoped ID wins for the
	// caller who quotes the full identifier... but the contract says the
	// selector must be unique, so this stays ambiguous. Assert the returned
	// behavior is deterministic (ambiguity), documenting the contract.
	vms := []types.VM{
		{ID: "qemu.local.web", Name: "web"},
		{ID: "web", Name: "other"},
	}
	_, err := Resolve(vms, "web")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous", err)
	}
}

func TestResolve_MultipleMatchesSameVM(t *testing.T) {
	// Selector equals the VM's own ID and its own name: still exactly one
	// VM — must NOT be ambiguous.
	vms := []types.VM{{ID: "dev", Name: "dev"}}
	got, err := Resolve(vms, "dev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "dev" {
		t.Errorf("got %+v, want the dev VM", got)
	}
}
