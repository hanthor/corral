package fleet

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
