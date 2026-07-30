package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCluster gives the test an empty config plus a kubeconfig, so
// config.Contexts offers a kubevirt target and the default backend stays qemu.
func withCluster(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CORRAL_INCUS_REMOTE", "")
	t.Setenv("CORRAL_LIBVIRT_URI", "")
	t.Setenv("CORRAL_KUBE_CONTEXT", "")
	t.Setenv("CORRAL_DEFAULT_BACKEND", "")
	kubeconfig := filepath.Join(home, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
}

// "cluster" is the legacy alias, and the target the dashboard hardcodes when
// /api/contexts is unreachable. It has to stay KubeVirt even though the
// default backend for a host with no config is qemu — resolving it locally
// would silently create the wrong kind of VM.
func TestResolveCreateTarget_ClusterIsNeverLocal(t *testing.T) {
	withCluster(t)

	target, err := resolveCreateTarget("cluster", createRequest{Name: "vm"})
	if err != nil {
		t.Fatalf("resolveCreateTarget(cluster): %v", err)
	}
	if target.Backend != "kubevirt" {
		t.Errorf("backend = %q, want kubevirt", target.Backend)
	}
}

// With no cluster to resolve to, "cluster" must say so rather than quietly
// falling back to the local backend.
func TestResolveCreateTarget_ClusterWithoutAClusterErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	t.Setenv("CORRAL_KUBE_CONTEXT", "")
	t.Setenv("CORRAL_DEFAULT_BACKEND", "")

	if target, err := resolveCreateTarget("cluster", createRequest{Name: "vm"}); err == nil {
		t.Errorf("want an error, got target %+v", target)
	}
}

// An unqualified create carrying KubeVirt-only fields belongs on the cluster:
// createLocalVM reads only ISO and Import, so routing it to qemu would drop
// the request's actual source and fail with an unrelated "needs an ISO".
func TestResolveCreateTarget_KubevirtShapedRequestsGoToTheCluster(t *testing.T) {
	withCluster(t)

	for _, tc := range []struct {
		name string
		req  createRequest
	}{
		{"containerDisk", createRequest{ContainerDisk: "quay.io/containerdisks/fedora:42"}},
		{"catalog image", createRequest{Image: "fedora"}},
		{"existing PVC", createRequest{PVC: "data-disk"}},
		{"bootc", createRequest{Bootc: "quay.io/centos-bootc:stream9"}},
		{"windows", createRequest{Windows: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := resolveCreateTarget("", tc.req)
			if err != nil {
				t.Fatalf("resolveCreateTarget: %v", err)
			}
			if target.Backend != "kubevirt" {
				t.Errorf("backend = %q, want kubevirt", target.Backend)
			}
		})
	}
}

// The shapes local QEMU actually serves still follow the default context, so a
// qemu-only or Incus-only deployment keeps working without a cluster (#136).
func TestResolveCreateTarget_LocallyServableRequestsUseTheDefault(t *testing.T) {
	withCluster(t)

	for _, tc := range []struct {
		name string
		req  createRequest
	}{
		{"import URL", createRequest{Import: "https://example.com/jammy.qcow2"}},
		{"installer ISO", createRequest{ISO: "https://example.com/debian.iso"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := resolveCreateTarget("", tc.req)
			if err != nil {
				t.Fatalf("resolveCreateTarget: %v", err)
			}
			if target.Backend != "qemu" {
				t.Errorf("backend = %q, want qemu (the default)", target.Backend)
			}
		})
	}
}

// "local" is unconditional — it means the local backend even when the request
// looks like a cluster one, so the caller gets a real error about the source
// rather than a VM on the wrong backend.
func TestResolveCreateTarget_LocalStaysLocal(t *testing.T) {
	withCluster(t)

	target, err := resolveCreateTarget("local", createRequest{ContainerDisk: "quay.io/containerdisks/fedora:42"})
	if err != nil {
		t.Fatalf("resolveCreateTarget(local): %v", err)
	}
	if target.Backend != "qemu" {
		t.Errorf("backend = %q, want qemu", target.Backend)
	}
}

// A qemu-only deployment has no remote context at all, so an empty fleet is
// just an empty fleet — it must not surface as the dashboard's "No cluster
// connected" error page (#136).
func TestHandleListVMs_QemuOnlyDeploymentIsNotAnError(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	t.Setenv("CORRAL_KUBE_CONTEXT", "")
	t.Setenv("CORRAL_INCUS_REMOTE", "")
	t.Setenv("CORRAL_LIBVIRT_URI", "")
	t.Setenv("CORRAL_DEFAULT_BACKEND", "")

	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200 — body: %s", resp.StatusCode, string(body))
	}
}

// Snapshots used to be KubeVirt-only: the three mutating routes were behind
// noLocal and answered a flat "not supported for local QEMU VMs". They now
// dispatch through the backend adapters (#134), so a local VM gets a real
// answer — and when the backend genuinely can't, the refusal says why.
func TestHandleSnapshots_LocalVMGetsABackendAnswer(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp := mustGet(t, fx.Server.URL+"/api/vms/local/nosuchvm/snapshots")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "not supported for local QEMU VMs") {
			t.Fatalf("still behind the blanket local refusal: %s", body)
		}
	}
	// The VM doesn't exist, so this is an error either way — what matters is
	// that it came from the local backend rather than from a route guard.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a nonexistent local VM reported snapshots")
	}
}
