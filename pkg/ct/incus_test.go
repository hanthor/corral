package ct

// The Incus CT path. It used to call exec.Command directly, which made it
// untestable, invisible to demo mode, and hard-wired to the local daemon — so
// this file exists as much to prove the seam is there as to check the mapping.

import (
	"testing"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/shell"
)

const incusRemoteJSON = `[
 {"name":"web-ct","type":"container","status":"Running","status_code":103,"location":"node-a",
  "config":{"limits.cpu":"2","limits.memory":"1GiB","security.privileged":"true"}},
 {"name":"builder-vm","type":"virtual-machine","status":"Running","status_code":103,"location":"node-b",
  "config":{"limits.cpu":"4","limits.memory":"8GiB"}}
]`

func fakeIncus(t *testing.T, listJSON string) *shell.Fake {
	t.Helper()
	f := shell.NewFake()
	f.AddPrefixResponse("incus list", listJSON, nil)
	f.AddPrefixResponse("incus", "", nil)
	incus.SetRunner(f)
	t.Cleanup(func() { incus.SetRunner(shell.Real{}) })
	return f
}

func TestListIncusCTs_ContainersOnly(t *testing.T) {
	fakeIncus(t, incusRemoteJSON)

	cts := listIncusCTs()
	if len(cts) != 1 {
		t.Fatalf("listIncusCTs returned %d, want only the container: %+v", len(cts), cts)
	}
	ct := cts[0]
	if ct.Name != "web-ct" {
		t.Errorf("CT name = %q; an Incus virtual machine is a VM, not a CT", ct.Name)
	}
	if ct.Backend != "incus" {
		t.Errorf("CT backend = %q, want incus", ct.Backend)
	}
	if ct.CPU != 2 || ct.Mem != "1GiB" {
		t.Errorf("CT shape = %d CPU / %s", ct.CPU, ct.Mem)
	}
	if ct.Node != "node-a" {
		t.Errorf("CT node = %q", ct.Node)
	}
	if !ct.Ready || ct.Phase != "Running" {
		t.Errorf("CT state = %q ready=%v", ct.Phase, ct.Ready)
	}
	// Privilege is the CT concept ADR-0005 models; it must survive the mapping.
	if !ct.Privileged {
		t.Error("a privileged container was reported as unprivileged")
	}
}

func TestListIncusCTs_NoDaemonIsEmptyNotAPanic(t *testing.T) {
	f := shell.NewFake() // nothing registered: every call fails
	incus.SetRunner(f)
	t.Cleanup(func() { incus.SetRunner(shell.Real{}) })

	if cts := listIncusCTs(); len(cts) != 0 {
		t.Errorf("listIncusCTs with no daemon = %+v, want empty", cts)
	}
}

// An Incus host with no Kubernetes cluster is a supported deployment. Now that
// a container is a CT and not a VM, ListCTs is the only place it appears, so a
// kubectl failure must not take the Incus containers down with it.
func TestListCTs_IncusSurvivesNoCluster(t *testing.T) {
	fakeIncus(t, incusRemoteJSON)

	kube := shell.NewFake() // no kubectl responses registered: no cluster
	SetRunner(kube)
	t.Cleanup(func() { SetRunner(shell.DefaultKubectl) })

	cts, err := ListCTs()
	if err != nil {
		t.Fatalf("ListCTs with Incus but no cluster: %v", err)
	}
	if len(cts) != 1 || cts[0].Name != "web-ct" {
		t.Fatalf("ListCTs = %+v, want the Incus container", cts)
	}
}

// With neither a cluster nor Incus, the cluster error is the useful answer —
// an empty list would claim the host has no CTs when it cannot tell.
func TestListCTs_NoClusterNoIncusReportsTheClusterError(t *testing.T) {
	f := shell.NewFake()
	incus.SetRunner(f)
	t.Cleanup(func() { incus.SetRunner(shell.Real{}) })

	kube := shell.NewFake()
	SetRunner(kube)
	t.Cleanup(func() { SetRunner(shell.DefaultKubectl) })

	if _, err := ListCTs(); err == nil {
		t.Error("ListCTs with no cluster and no Incus returned no error")
	}
}

// The lifecycle helpers must go through the seam, or a CT on a remote can be
// listed and never started.
func TestIncusCTLifecycleUsesTheSeam(t *testing.T) {
	f := fakeIncus(t, incusRemoteJSON)

	if !incusExists("web-ct") {
		t.Error("incusExists should report a reachable instance as present")
	}
	if err := incusStart("web-ct"); err != nil {
		t.Errorf("incusStart: %v", err)
	}
	if err := incusStop("web-ct"); err != nil {
		t.Errorf("incusStop: %v", err)
	}
	if err := incusDelete("web-ct"); err != nil {
		t.Errorf("incusDelete: %v", err)
	}

	verbs := map[string]bool{}
	for _, call := range f.Calls() {
		if call.Name != "incus" {
			t.Errorf("CT path shelled out to %q, want incus through the seam", call.Name)
		}
		if len(call.Args) > 0 {
			verbs[call.Args[0]] = true
		}
	}
	for _, verb := range []string{"info", "start", "stop", "delete"} {
		if !verbs[verb] {
			t.Errorf("no incus %s call recorded — the operation bypassed the runner", verb)
		}
	}
}
