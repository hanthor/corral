package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/registry"
)

// forceKubevirt makes config.Contexts() offer a kubevirt target for the
// duration of a test, so /api/nodes takes the cluster (kubectl) path
// regardless of whether the test host has a kubeconfig.
func forceKubevirt(t *testing.T) {
	t.Helper()
	config.SetForceKubevirtContext(true)
	t.Cleanup(func() { config.SetForceKubevirtContext(false) })
}

// TestStaticServed verifies the embedded SPA (index.html + assets) is served.
func TestStaticServed(t *testing.T) {
	mux, err := newMux()
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/", "/app.js", "/icons.js", "/style.css", "/alpine.min.js"} {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, r.StatusCode)
		}
	}
}

// TestAllRoutesRegistered hits every API route with its method and asserts the
// route is wired (no 404 / 405). Handlers that shell out to kubectl will fail
// without a cluster (5xx), which is fine — we're verifying the surface exists,
// so the kind of "feature silently missing" regression can't slip through.
func TestAllRoutesRegistered(t *testing.T) {
	mux, err := newMux()
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/readyz"},
		{"GET", "/api/vms"},
		{"POST", "/api/vms"},
		{"GET", "/api/nodes"},
		{"GET", "/api/capabilities"},
		{"GET", "/api/instancetypes"},
		{"GET", "/api/nads"},
		{"GET", "/api/doctor"},
		{"POST", "/api/doctor/fix"},
		{"GET", "/api/plugins"},
		{"GET", "/api/datavolumes"},
		{"POST", "/api/datavolumes"},
		{"DELETE", "/api/datavolumes/ns/name"},
		{"GET", "/api/tasks/abc"},
		{"GET", "/api/vms/ns/name"},
		{"DELETE", "/api/vms/ns/name"},
		{"POST", "/api/vms/ns/name/start"},
		{"POST", "/api/vms/ns/name/scale"},
		{"POST", "/api/vms/ns/name/expand"},
		{"POST", "/api/vms/ns/name/clone"},
		{"POST", "/api/vms/ns/name/template"},
		{"POST", "/api/vms/ns/name/nics"},
		{"GET", "/api/vms/ns/name/guestinfo"},
		{"GET", "/api/vms/ns/name/events"},
		{"GET", "/api/vms/ns/name/metrics"},
		{"GET", "/api/vms/ns/name/export"},
		{"POST", "/api/vms/ns/name/volumes"},
		{"DELETE", "/api/vms/ns/name/volumes/vol"},
		{"GET", "/api/vms/ns/name/snapshots"},
		{"POST", "/api/vms/ns/name/snapshots"},
		{"DELETE", "/api/vms/ns/name/snapshots/snap"},
		{"POST", "/api/vms/ns/name/snapshots/snap/restore"},
	}
	client := &http.Client{}
	for _, rt := range routes {
		req, _ := http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader("{}"))
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", rt.method, rt.path, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed ||
			strings.Contains(string(body), "404 page not found") {
			t.Errorf("%s %s not registered (got %d: %s)", rt.method, rt.path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

// ── handleListVMs ──────────────────────────────────────────────────

func TestHandleListVMs_Empty(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		`{"items": []}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmis", "-A", "-o", "json"},
		`{"items": []}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var vms []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		t.Fatal(err)
	}
	if len(vms) != 0 {
		t.Errorf("expected 0 VMs, got %d", len(vms))
	}
}

func TestHandleListVMs_OneVM(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		vmListJSON("myvm", "tailvm", "Stopped", false), nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmis", "-A", "-o", "json"},
		`{"items": []}`, nil)

	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var vms []map[string]any
	json.NewDecoder(resp.Body).Decode(&vms)
	if len(vms) != 1 {
		t.Fatalf("expected 1 VM, got %d", len(vms))
	}
	vm := vms[0]
	if vm["name"] != "myvm" {
		t.Errorf("name = %v, want myvm", vm["name"])
	}
	if vm["backend"] != "kubevirt" {
		t.Errorf("backend = %v, want kubevirt", vm["backend"])
	}
}

// A failing NON-default backend (here KubeVirt, on a qemu-default host with a
// stray kubeconfig) must not produce the full-page offline error: the default
// backend (qemu) listed fine, just empty. The dashboard renders an empty fleet
// and the KubeVirt failure is a silent partial (#91).
func TestHandleListVMs_NonDefaultBackendError_NotOffline(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		"", fmt.Errorf("kubectl: connection refused"))

	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200 (qemu is the default backend and listed fine)", resp.StatusCode)
	}
	if resp.Header.Get("X-Corral-Partial") != "true" {
		t.Error("expected X-Corral-Partial header flagging the KubeVirt failure")
	}
	var vms []map[string]any
	json.NewDecoder(resp.Body).Decode(&vms)
	if len(vms) != 0 {
		t.Errorf("expected empty fleet, got %d", len(vms))
	}
}

// When the user's PRIMARY backend is the one that's down and there's nothing to
// show, the offline (502) state is still correct — that's the real "connect a
// cluster" case for a KubeVirt-default deployment.
func TestHandleListVMs_DefaultBackendDown_Offline(t *testing.T) {
	t.Setenv("CORRAL_DEFAULT_BACKEND", "kubevirt")
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		"", fmt.Errorf("kubectl: connection refused"))

	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] == "" {
		t.Error("expected error message in response")
	}
}

// ── handleCreateVM ─────────────────────────────────────────────────

func TestHandleCreateVM_MissingName(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp := mustPost(t, fx.Server.URL+"/api/vms", `{"cpu": 2, "mem": "4G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] == "" {
		t.Error("expected error message")
	}
}

func TestHandleCreateVM_CatalogImage(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Create a fake SSH key file so LoadSSHPublicKey finds it
	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	// Set up responses for kubectl apply (PVC + VM + registry store)
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		`{"name":"myvm","image":"fedora","cpu":2,"mem":"4G","disk":"20G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 201 — body: %s", resp.StatusCode, string(body))
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["name"] != "myvm" {
		t.Errorf("name = %v, want myvm", body["name"])
	}
}

// Catalog entries sourced from the distros' own mirrors (URL kind) must route
// through the CDI import path, and installer-ISO entries through the ISO path.
func TestHandleCreateVM_CatalogOfficialSource(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	for _, image := range []string{"debian-12-official", "turnkey-core"} {
		fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
		resp := mustPost(t, fx.Server.URL+"/api/vms",
			fmt.Sprintf(`{"name":"myvm-%s","image":%q,"cpu":1,"mem":"2G","disk":"15G"}`, image, image))
		code := resp.StatusCode
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if code != http.StatusCreated {
			t.Fatalf("%s: got %d, want 201 — body: %s", image, code, string(body))
		}
	}
}

func TestHandleCreateVM_ContainerDisk(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		`{"name":"myvm","containerDisk":"quay.io/containerdisks/ubuntu:24.04","cpu":2,"mem":"4G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 201 — body: %s", resp.StatusCode, string(body))
	}
}

func TestHandleCreateVM_ImportURL(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		// An import URL is one of the two shapes the local backend also serves,
		// so this asks for the cluster explicitly to exercise the CDI path.
		`{"name":"myvm","target":"cluster","import":"https://example.com/jammy.qcow2","cpu":2,"mem":"4G","disk":"10G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 201 — body: %s", resp.StatusCode, string(body))
	}
}

func TestHandleCreateVM_BootcUnavailable(t *testing.T) {
	if kubevirt.BootcAvailable() {
		t.Skip("bootc pipeline compiled in (-tags bootc) — the 400 guard can't trigger")
	}
	fx := NewTestFixture()
	defer fx.Close()

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		`{"name":"myvm","bootc":"quay.io/centos-bootc:stream9"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

func TestHandleCreateVM_UnknownCatalogImage(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		`{"name":"myvm","image":"nonexistent-os","cpu":2,"mem":"4G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

// ── handleVMAction ─────────────────────────────────────────────────

func TestHandleVMAction_Start(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// StartVM calls ensureVirtctl → LookPath → returns /fake/bin/virtctl
	// then calls Run("/fake/bin/virtctl", "start", "myvm", "-n", "tailvm")
	fx.Runner.AddResponseKV("/fake/bin/virtctl",
		[]string{"start", "myvm", "-n", "tailvm"}, "", nil)

	resp := mustPost(t, fx.Server.URL+"/api/vms/tailvm/myvm/start", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200 — body: %s", resp.StatusCode, string(body))
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}

func TestHandleVMAction_StartError(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl",
		[]string{"start", "myvm", "-n", "tailvm"}, "", fmt.Errorf("VM not found"))

	resp := mustPost(t, fx.Server.URL+"/api/vms/tailvm/myvm/start", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", resp.StatusCode)
	}
}

func TestHandleVMAction_Stop(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl",
		[]string{"stop", "myvm", "-n", "tailvm"}, "", nil)

	resp := mustPost(t, fx.Server.URL+"/api/vms/tailvm/myvm/stop", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
}

func TestHandleVMAction_UnknownAction(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp := mustPost(t, fx.Server.URL+"/api/vms/tailvm/myvm/bogus", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

// ── handleDeleteVM ─────────────────────────────────────────────────

func TestHandleDeleteVM_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// DeleteVM runs several commands; stub them all
	fx.Runner.AddResponseKV("/fake/bin/virtctl",
		[]string{"stop", "myvm", "-n", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl",
		[]string{"delete", "vm", "myvm", "-n", "tailvm", "--ignore-not-found"}, "", nil)
	// DeleteVM also deletes DataVolumes and PVCs for each suffix
	for _, suffix := range []string{"disk", "data", "iso", "bootc-disk"} {
		pvc := "myvm-" + suffix
		fx.Runner.AddResponseKV("kubectl",
			[]string{"delete", "datavolume", pvc, "-n", "tailvm", "--ignore-not-found", "--wait=false"}, "", nil)
		fx.Runner.AddResponseKV("kubectl",
			[]string{"delete", "pvc", pvc, "-n", "tailvm", "--ignore-not-found", "--wait=false"}, "", nil)
	}
	fx.Runner.AddResponseKV("kubectl",
		[]string{"delete", "pvc", "-n", "tailvm", "-l", "corral.dev/vm=myvm", "--ignore-not-found", "--wait=false"}, "", nil)
	fx.Runner.AddResponseKV("kubectl",
		[]string{"delete", "vmsnapshot", "-n", "tailvm", "-l", "corral.dev/vm=myvm", "--ignore-not-found", "--wait=false"}, "", nil)

	resp := mustDelete(t, fx.Server.URL+"/api/vms/tailvm/myvm")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200 — body: %s", resp.StatusCode, string(body))
	}
}

// ── handleVMInfo ───────────────────────────────────────────────────

func TestHandleVMInfo_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl",
		[]string{"get", "vm", "myvm", "-n", "tailvm", "-o", "json"},
		`{"kind":"VirtualMachine","metadata":{"name":"myvm"}}`, nil)

	resp := mustGet(t, fx.Server.URL+"/api/vms/tailvm/myvm")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["metadata"].(map[string]any)["name"] != "myvm" {
		t.Error("unexpected VM name")
	}
}

// ── handleNodes ────────────────────────────────────────────────────

func TestHandleNodes_Success(t *testing.T) {
	forceKubevirt(t)
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"},
		nodeListJSON("bihar", true, "control-plane,master", "v1.36.1", "amd64"), nil)

	resp := mustGet(t, fx.Server.URL+"/api/nodes")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var nodes []map[string]any
	json.NewDecoder(resp.Body).Decode(&nodes)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0]["name"] != "bihar" {
		t.Errorf("name = %v, want bihar", nodes[0]["name"])
	}
}

func TestHandleNodes_Error(t *testing.T) {
	forceKubevirt(t)
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"},
		"", fmt.Errorf("kubectl: connection refused"))

	resp := mustGet(t, fx.Server.URL+"/api/nodes")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", resp.StatusCode)
	}
}

// A host with no KubeVirt context (only local QEMU/Incus/libvirt) must not
// surface a "no cluster connected" error just because kubectl fails — it has
// no cluster by design. /api/nodes returns the synthetic local host instead,
// so the dashboard renders normally rather than nagging about KubeVirt (#91).
func TestHandleNodes_LocalOnlyNoCluster(t *testing.T) {
	config.SetForceKubevirtContext(false)
	t.Setenv("CORRAL_KUBE_CONTEXT", "")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir()) // no ~/.kube/config → kubevirt is not configured
	fx := NewTestFixture()
	defer fx.Close()

	// kubectl would fail here, but the local-only path must never call it.
	fx.Runner.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"},
		"", fmt.Errorf("kubectl: connection refused"))

	resp := mustGet(t, fx.Server.URL+"/api/nodes")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var nodes []map[string]any
	json.NewDecoder(resp.Body).Decode(&nodes)
	if len(nodes) != 1 || nodes[0]["name"] != "local" {
		t.Fatalf("expected a single synthetic local node, got %v", nodes)
	}
}

// ── Helpers ────────────────────────────────────────────────────────

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ── JSON fixtures ──────────────────────────────────────────────────

func vmListJSON(name, ns, status string, ready bool) string {
	return fmt.Sprintf(`{
  "items": [
    {
      "metadata": {"name": %q, "namespace": %q, "labels": {}},
      "spec": {
        "running": false,
        "template": {
          "spec": {
            "domain": {
              "cpu": {"cores": 1, "sockets": 2, "threads": 1},
              "memory": {"guest": "4Gi"}
            },
            "nodeSelector": {"kubernetes.io/hostname": "bihar"}
          }
        }
      },
      "status": {"ready": %t, "printableStatus": %q}
    }
  ]
}`, name, ns, ready, status)
}

func vmiListJSON(name, ns, ip, nodeName string) string {
	return fmt.Sprintf(`{
  "items": [
    {
      "metadata": {"name": %q, "namespace": %q},
      "status": {
        "nodeName": %q,
        "interfaces": [{"ipAddress": %q}]
      }
    }
  ]
}`, name, ns, nodeName, ip)
}

func nodeWithRolesJSON(name string, ready bool, labels map[string]string, kubelet, arch string) string {
	labelPairs := ""
	for k, v := range labels {
		if labelPairs != "" {
			labelPairs += ","
		}
		labelPairs += fmt.Sprintf("%q:%q", k, v)
	}
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	return fmt.Sprintf(`{
  "items": [
    {
      "metadata": {"name": %q, "labels": {%s}},
      "status": {
        "conditions": [{"type": "Ready", "status": %q}],
        "nodeInfo": {"kubeletVersion": %q, "architecture": %q}
      }
    }
  ]
}`, name, labelPairs, readyStatus, kubelet, arch)
}

func nodeListJSON(name string, ready bool, roles, kubelet, arch string) string {
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	return fmt.Sprintf(`{
  "items": [
    {
      "metadata": {
        "name": %q,
        "labels": {"kubernetes.io/role": %q}
      },
      "status": {
        "conditions": [{"type": "Ready", "status": %q}],
        "nodeInfo": {"kubeletVersion": %q, "architecture": %q}
      }
    }
  ]
}`, name, roles, readyStatus, kubelet, arch)
}

// ── handleListVMs VMI merge ──────────────────────────────────────

func TestHandleListVMs_WithRunningVMI(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// VM list: one VM in tailvm namespace
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		vmListJSON("myvm", "tailvm", "Running", true), nil)
	// VMI list: matching VMI with IP and nodeName
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmis", "-A", "-o", "json"},
		vmiListJSON("myvm", "tailvm", "10.42.0.15", "bihar"), nil)

	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var vms []map[string]any
	json.NewDecoder(resp.Body).Decode(&vms)
	if len(vms) != 1 {
		t.Fatalf("expected 1 VM, got %d", len(vms))
	}
	vm := vms[0]
	// IP and Node should be populated from the VMI merge
	if ip, _ := vm["ip"].(string); ip != "10.42.0.15" {
		t.Errorf("ip = %q, want '10.42.0.15'", ip)
	}
	if node, _ := vm["node"].(string); node != "bihar" {
		t.Errorf("node = %q, want 'bihar'", node)
	}
}

// ── handleNodes roles ────────────────────────────────────────────

func TestHandleNodes_WithRoles(t *testing.T) {
	forceKubevirt(t)
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"},
		nodeWithRolesJSON("bihar", true, map[string]string{
			"node-role.kubernetes.io/control-plane": "",
			"node-role.kubernetes.io/master":        "",
		}, "v1.36.1", "amd64"), nil)

	resp := mustGet(t, fx.Server.URL+"/api/nodes")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var nodes []map[string]any
	json.NewDecoder(resp.Body).Decode(&nodes)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	roles, _ := nodes[0]["roles"].(string)
	// Roles should contain both control-plane and master
	if !strings.Contains(roles, "control-plane") || !strings.Contains(roles, "master") {
		t.Errorf("roles = %q, expected to contain 'control-plane' and 'master'", roles)
	}
}

// ── handleTaskStatus existing task ───────────────────────────────

func TestHandleTaskStatus_Existing(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Insert a completed build task directly into the package-level tasks map
	task := newBuildTask()
	task.finish(nil) // status = "done"
	tasks.Store("test-task-1", task)
	defer tasks.Delete("test-task-1")

	resp := mustGet(t, fx.Server.URL+"/api/tasks/test-task-1")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "done" {
		t.Errorf("status = %q, want 'done'", body["status"])
	}
}

// ── Edge cases ───────────────────────────────────────────────────

func TestHandleCreateVM_MalformedJSON(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json",
		strings.NewReader(`{not json}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for malformed JSON, got %d", resp.StatusCode)
	}
}

func TestHandleCreateVM_EmptyBody(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json",
		strings.NewReader(``))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for empty body, got %d", resp.StatusCode)
	}
}

func TestHandleCreateVM_InvalidName(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	body := strings.NewReader(`{"name":"UPPERCASE"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// CreateVM itself doesn't validate name format, but k8s will reject
	// The handler should at least not panic
	t.Logf("uppercase name returned %d", resp.StatusCode)
}

func TestHandleListVMs_CORSHeaders(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		`{"items":[]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmis", "-A", "-o", "json"},
		`{"items":[]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json Content-Type, got %q", ct)
	}
}

func TestHandleCreateDelete_RegistryRoundtrip(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Create VM — should store in registry
	fx.Runner.AddResponseKV("kubectl", []string{"create", "ns", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"label", "ns", "tailvm",
		"pod-security.kubernetes.io/enforce=privileged", "--overwrite"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vm", "web", "-n", "tailvm", "-o", "name"},
		"", errSimulated) // VM doesn't exist yet
	fx.Runner.AddResponseKV("kubectl", []string{"get", "sc", "-o", "json"},
		`{"items":[{"metadata":{"name":"longhorn"}}]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "created", nil)

	body := strings.NewReader(`{"name":"web","containerDisk":"quay.io/containerdisks/fedora:42"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	// Now delete — should remove from registry
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"stop", "web", "-n", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vm", "web", "-n", "tailvm", "--ignore-not-found"}, "", nil)
	fx.Runner.AddPrefixResponse("kubectl delete pvc web-", "", nil)
	fx.Runner.AddPrefixResponse("kubectl delete datavolume web-", "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "pvc", "-n", "tailvm", "-l", "corral.dev/vm=web", "--ignore-not-found"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vmsnapshot", "-n", "tailvm", "-l", "corral.dev/vm=web", "--ignore-not-found"}, "", nil)

	req, _ := http.NewRequest("DELETE", fx.Server.URL+"/api/vms/tailvm/web", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	// Verify registry was cleaned — try deleting again, should still succeed
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"stop", "web", "-n", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vm", "web", "-n", "tailvm", "--ignore-not-found"}, "", nil)
	fx.Runner.AddPrefixResponse("kubectl delete pvc web-", "", nil)
	fx.Runner.AddPrefixResponse("kubectl delete datavolume web-", "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "pvc", "-n", "tailvm", "-l", "corral.dev/vm=web", "--ignore-not-found"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vmsnapshot", "-n", "tailvm", "-l", "corral.dev/vm=web", "--ignore-not-found"}, "", nil)

	req, _ = http.NewRequest("DELETE", fx.Server.URL+"/api/vms/tailvm/web", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Second delete should also succeed (idempotent)
	if resp.StatusCode != 200 {
		t.Errorf("idempotent delete: expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleCreateVM_ISOSource(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	// ISO path: EnsureNamespace + PreferredStorageClass + DataVolume apply + PVC apply + VM apply
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "sc", "-o", "json"},
		`{"items":[{"metadata":{"name":"longhorn"}}]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vm", "isovm", "-n", "tailvm", "-o", "name"},
		"", errSimulated) // VM doesn't exist yet

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		// As with the import URL above: an ISO is servable locally, so name the
		// cluster to keep the CDI/blank-disk path under test.
		`{"name":"isovm","target":"cluster","iso":"https://example.com/debian.iso","cpu":2,"mem":"4G","disk":"20G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 201 — body: %s", resp.StatusCode, string(body))
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["name"] != "isovm" {
		t.Errorf("name = %v, want isovm", body["name"])
	}
}

func TestHandleCreateVM_PVCSource(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	tmpHome := t.TempDir()
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAtest"), 0600)
	t.Setenv("HOME", tmpHome)

	// PVC source skips PVC creation, just creates the VM manifest
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "sc", "-o", "json"},
		`{"items":[{"metadata":{"name":"longhorn"}}]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vm", "pvcvm", "-n", "tailvm", "-o", "name"},
		"", errSimulated)

	resp := mustPost(t, fx.Server.URL+"/api/vms",
		`{"name":"pvcvm","pvc":"existing-data-disk","cpu":2,"mem":"4G"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 201 — body: %s", resp.StatusCode, string(body))
	}
}

func TestHandleDeleteVM_RemovesLabeledPVCs(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// All DeleteVM commands
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"stop", "labeled", "-n", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vm", "labeled", "-n", "tailvm", "--ignore-not-found"}, "", nil)
	for _, suffix := range []string{"disk", "data", "iso", "bootc-disk"} {
		pvc := "labeled-" + suffix
		fx.Runner.AddResponseKV("kubectl", []string{"delete", "datavolume", pvc, "-n", "tailvm", "--ignore-not-found", "--wait=false"}, "", nil)
		fx.Runner.AddResponseKV("kubectl", []string{"delete", "pvc", pvc, "-n", "tailvm", "--ignore-not-found", "--wait=false"}, "", nil)
	}
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "pvc", "-n", "tailvm", "-l", "corral.dev/vm=labeled", "--ignore-not-found", "--wait=false"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"delete", "vmsnapshot", "-n", "tailvm", "-l", "corral.dev/vm=labeled", "--ignore-not-found"}, "", nil)

	resp := mustDelete(t, fx.Server.URL+"/api/vms/tailvm/labeled")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	// Verify the labeled PVC delete command was called
	found := false
	for _, call := range fx.Runner.Calls() {
		if call.Name == "kubectl" && len(call.Args) >= 6 &&
			call.Args[0] == "delete" && call.Args[1] == "pvc" &&
			call.Args[4] == "-l" && call.Args[5] == "corral.dev/vm=labeled" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected kubectl delete pvc -l corral.dev/vm=labeled call, but it was not found in recorded calls")
	}
}

func TestHandleExport_StoppedVM(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// VM is stopped — kubectl get vmi returns error
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmi", "stoppedvm", "-n", "tailvm"},
		"", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/stoppedvm/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should NOT return 409 (that's only when VM is running)
	if resp.StatusCode == 409 {
		t.Errorf("got 409, expected non-409 (VM is stopped, export should proceed)")
	}
	// Export will fail later (virtctl not available) but the handler shouldn't return 409
	t.Logf("export stopped VM returned %d", resp.StatusCode)
}

func TestHealthz(t *testing.T) {
	mux, err := newMux()
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok\n" {
		t.Errorf("GET /healthz body = %q, want %q", string(body), "ok\n")
	}
}

func TestReadyz(t *testing.T) {
	mux, err := newMux()
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. When store is nil
	origStore := store
	store = nil
	defer func() { store = origStore }()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz (nil store) = %d, want 503", resp.StatusCode)
	}

	// 2. When store is initialized
	tmpDir := t.TempDir()
	store = registry.NewStoreAt(filepath.Join(tmpDir, "registry.json"))

	resp2, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz (initialized store) = %d, want 200", resp2.StatusCode)
	}
	var res map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res["status"] != "ready" {
		t.Errorf("status = %v, want ready", res["status"])
	}
}
