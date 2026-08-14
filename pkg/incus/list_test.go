package incus

// Tests for the Incus list/parse surface that had zero coverage:
// Client.ListInstances, Client.Containers, Client.List, and the
// container-vs-VM split that keeps one instance from being both.
// Uses the shell.Fake runner pattern from lxc_test.go.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
)

func TestListInstances_ParsesJSON(t *testing.T) {
	f := fakeRemote(t, `[
  {"name":"web-ct","type":"container","status":"Running"},
  {"name":"builder-vm","type":"virtual-machine","status":"Running"}
]`)

	insts, err := NewClient("").ListInstances()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("ListInstances returned %d instances, want 2", len(insts))
	}
	if insts[0].Name != "web-ct" || insts[1].Name != "builder-vm" {
		t.Errorf("unexpected instance order: %+v", insts)
	}
	_ = f
}

func TestListInstances_ErrorPropagates(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus list", "", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	_, err := NewClient("").ListInstances()
	if err == nil {
		t.Fatal("ListInstances: expected error when incus list fails")
	}
	if !strings.Contains(err.Error(), "incus list failed") {
		t.Errorf("error = %v, want 'incus list failed'", err)
	}
}

func TestListInstances_BadJSON(t *testing.T) {
	fakeRemote(t, `{not json`)
	_, err := NewClient("").ListInstances()
	if err == nil {
		t.Fatal("ListInstances: expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("error = %v, want parse error", err)
	}
}

func TestContainers_OnlyContainers(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	cts, err := NewClient("").Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(cts) != 2 {
		t.Fatalf("Containers returned %d, want 2 (web-ct + stopped-ct)", len(cts))
	}
	for _, c := range cts {
		if !c.IsContainer() {
			t.Errorf("Containers returned a VM: %+v", c)
		}
	}
}

func TestList_OnlyVMsWithIdentity(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	vms, err := NewClient("").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("List returned %d VMs, want 1 (builder-vm)", len(vms))
	}
	vm := vms[0]
	if vm.Name != "builder-vm" {
		t.Errorf("vm.Name = %q, want builder-vm", vm.Name)
	}
	if vm.Backend != "incus" {
		t.Errorf("vm.Backend = %q, want incus", vm.Backend)
	}
	if !vm.Running || !vm.Ready {
		t.Errorf("builder-vm should be running+ready: %+v", vm)
	}
	if vm.IP != "10.1.1.9" {
		t.Errorf("vm.IP = %q, want 10.1.1.9 (first global IPv4)", vm.IP)
	}
	if vm.CPU != 4 {
		t.Errorf("vm.CPU = %d, want 4", vm.CPU)
	}
	if vm.Mem != "8GiB" {
		t.Errorf("vm.Mem = %q, want 8GiB", vm.Mem)
	}
	if vm.ID == "" {
		t.Error("vm.ID empty — SetIdentity must run")
	}
}

func TestList_RemoteQualifiesContext(t *testing.T) {
	fakeRemote(t, `[{"name":"vm","type":"virtual-machine","status":"Running"}]`)

	vms, err := NewClient("lab").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("List returned %d VMs", len(vms))
	}
	if vms[0].Context != "lab" {
		t.Errorf("vm.Context = %q, want lab", vms[0].Context)
	}
}

func TestList_StoppedVMNotReady(t *testing.T) {
	fakeRemote(t, `[{"name":"down-vm","type":"virtual-machine","status":"Stopped"}]`)

	vms, err := NewClient("").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("List returned %d VMs", len(vms))
	}
	if vms[0].Running || vms[0].Ready {
		t.Errorf("stopped VM must not be running/ready: %+v", vms[0])
	}
}

// ── package-level wrappers ────────────────────────────────────────────────

func TestPackageWrappers_DefaultRemote(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	// List() / Containers() / ListInstances() use the default remote.
	vms, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(vms) != 1 || vms[0].Name != "builder-vm" {
		t.Errorf("List() = %+v, want builder-vm only", vms)
	}

	cts, err := Containers()
	if err != nil {
		t.Fatalf("Containers(): %v", err)
	}
	if len(cts) != 2 {
		t.Errorf("Containers() = %d, want 2", len(cts))
	}

	insts, err := ListInstances()
	if err != nil {
		t.Fatalf("ListInstances(): %v", err)
	}
	if len(insts) != 3 {
		t.Errorf("ListInstances() = %d, want 3", len(insts))
	}
}

func TestList_ContainerWithVMNameSuffixIsNotAVM(t *testing.T) {
	// A container whose name ends in -vm must still be a container — the
	// split is on the type field, never the name.
	fakeRemote(t, `[{"name":"not-a-vm","type":"container","status":"Running"}]`)

	vms, err := NewClient("").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 0 {
		t.Errorf("List returned %d VMs for a container, want 0", len(vms))
	}
}
