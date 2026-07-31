package incus

// Incus LXC coverage: containers and virtual machines are different things, and
// Corral models them as different things. These tests pin the split, the remote
// targeting, and the address parsing — the three places Incus support was
// quietly incomplete (see docs/backend-parity.md).

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
)

// A remote holding one container and one VM, the mixed case that used to be
// mishandled. The VM carries an address, the container does not yet — both
// happen on a real remote.
const mixedRemoteJSON = `[
 {"name":"web-ct","type":"container","status":"Running","status_code":103,"location":"node-a",
  "config":{"limits.cpu":"2","limits.memory":"1GiB","security.privileged":"true"},
  "state":{"network":{"lo":{"addresses":[{"family":"inet","address":"127.0.0.1","scope":"local"}]},
                      "eth0":{"addresses":[{"family":"inet","address":"10.1.1.5","scope":"global"},
                                           {"family":"inet6","address":"fe80::1","scope":"link"}]}}}},
 {"name":"builder-vm","type":"virtual-machine","status":"Running","status_code":103,"location":"node-b",
  "config":{"limits.cpu":"4","limits.memory":"8GiB"},
  "state":{"network":{"enp5s0":{"addresses":[{"family":"inet","address":"10.1.1.9","scope":"global"}]}}}},
 {"name":"stopped-ct","type":"container","status":"Stopped","status_code":102,"location":"node-a",
  "config":{"limits.cpu":"1","limits.memory":"512MiB"}}
]`

func fakeRemote(t *testing.T, json string) *shell.Fake {
	t.Helper()
	f := shell.NewFake()
	f.AddPrefixResponse("incus list", json, nil)
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })
	return f
}

// The headline fix: a container is not a VM. Listing used to return every
// instance as a VM while pkg/ct returned the same instances as CTs, so every
// Incus instance appeared in the fleet twice.
func TestList_ReturnsVirtualMachinesOnly(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	vms, err := NewClient("lab").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("List returned %d instances, want only the virtual machine: %+v", len(vms), vms)
	}
	vm := vms[0]
	if vm.Name != "builder-vm" {
		t.Errorf("List returned %q, want builder-vm", vm.Name)
	}
	if vm.Backend != "incus" || vm.Context != "lab" {
		t.Errorf("VM identity = backend %q context %q, want incus/lab", vm.Backend, vm.Context)
	}
	if vm.CPU != 4 || vm.Mem != "8GiB" {
		t.Errorf("VM shape = %d CPU / %s", vm.CPU, vm.Mem)
	}
	if vm.Node != "node-b" {
		t.Errorf("VM node = %q, want node-b", vm.Node)
	}
	if !vm.Running || !vm.Ready {
		t.Error("a Running instance should read as running and ready")
	}
	if vm.ID == "" {
		t.Error("VM has no canonical identity")
	}
}

func TestContainers_ReturnsContainersOnly(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	cts, err := NewClient("lab").Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(cts) != 2 {
		t.Fatalf("Containers returned %d, want the two containers: %+v", len(cts), cts)
	}
	for _, ct := range cts {
		if !ct.IsContainer() {
			t.Errorf("%q is not a container", ct.Name)
		}
		if ct.Name == "builder-vm" {
			t.Error("Containers returned the virtual machine")
		}
	}
	// Privilege has to survive: it is the CT concept ADR-0005 models, and PVE's
	// own unprivileged default.
	if cts[0].Config["security.privileged"] != "true" {
		t.Error("privileged container lost its security.privileged config")
	}
}

// Nothing may appear in both listings — the invariant the double-listing bug
// broke.
func TestListAndContainersDoNotOverlap(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	client := NewClient("lab")
	vms, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	cts, err := client.Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}

	seen := map[string]bool{}
	for _, vm := range vms {
		seen[vm.Name] = true
	}
	for _, ct := range cts {
		if seen[ct.Name] {
			t.Errorf("%q is listed as both a VM and a container", ct.Name)
		}
	}
	if len(vms)+len(cts) != 3 {
		t.Errorf("%d VMs + %d containers, want the remote's 3 instances exactly once each",
			len(vms), len(cts))
	}
}

// An instance Corral cannot classify is treated as a container: it loses a
// graphical console rather than being offered one that cannot work.
func TestUnclassifiedInstanceIsAContainer(t *testing.T) {
	fakeRemote(t, `[{"name":"mystery","status":"Running","config":{}}]`)

	vms, _ := NewClient("").List()
	if len(vms) != 0 {
		t.Errorf("an instance with no type was listed as a VM: %+v", vms)
	}
	cts, _ := NewClient("").Containers()
	if len(cts) != 1 {
		t.Errorf("an instance with no type should be treated as a container, got %+v", cts)
	}
}

func TestAddress(t *testing.T) {
	fakeRemote(t, mixedRemoteJSON)

	vms, _ := NewClient("lab").List()
	if got := vms[0].IP; got != "10.1.1.9" {
		t.Errorf("VM address = %q, want 10.1.1.9 — without it the address column is blank "+
			"and the SSH/RDP probes have nothing to aim at", got)
	}

	cts, _ := NewClient("lab").Containers()
	byName := map[string]Instance{}
	for _, ct := range cts {
		byName[ct.Name] = ct
	}
	// Loopback and link-local are not the instance's address.
	if got := byName["web-ct"].Address(); got != "10.1.1.5" {
		t.Errorf("container address = %q, want 10.1.1.5", got)
	}
	// An instance with no state yet reports no address rather than guessing.
	if got := byName["stopped-ct"].Address(); got != "" {
		t.Errorf("stopped container address = %q, want empty", got)
	}
}

// Every operation must target the configured remote. This is what the CT path
// lost by calling exec.Command: it always hit the local daemon, so a CT on a
// remote could be listed and never started.
func TestOperationsTargetTheConfiguredRemote(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus", "", nil)
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	client := NewClient("lab")
	client.Start("web-ct")
	client.Stop("web-ct")
	client.Delete("web-ct")
	client.Exists("web-ct")

	verbs := map[string]bool{}
	for _, call := range f.Calls() {
		line := call.Name + " " + strings.Join(call.Args, " ")
		if len(call.Args) > 0 {
			verbs[call.Args[0]] = true
		}
		if !strings.Contains(line, "lab:web-ct") {
			t.Errorf("call %q does not target the configured remote", line)
		}
	}
	for _, verb := range []string{"start", "stop", "delete", "info"} {
		if !verbs[verb] {
			t.Errorf("no incus %s call was made", verb)
		}
	}
}

func TestListSurfacesDaemonErrors(t *testing.T) {
	f := shell.NewFake() // no responses registered: every call fails
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if _, err := NewClient("lab").List(); err == nil {
		t.Error("List should surface a daemon failure")
	}
	if _, err := NewClient("lab").Containers(); err == nil {
		t.Error("Containers should surface a daemon failure")
	}
}

func TestListRejectsGarbage(t *testing.T) {
	fakeRemote(t, `not json`)
	if _, err := NewClient("").List(); err == nil {
		t.Error("List should reject unparseable output rather than returning an empty fleet")
	}
}
