package web

// End-to-end smoke test of demo mode: EnableDemo + the real mux, exercising
// the same request flow the dashboard uses. This is the CI safety net for
// the whole demo seam — if a handler's kubectl usage drifts away from what
// pkg/demo answers, it fails here instead of on someone's first
// `corral web --demo`.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/registry"
)

func newDemoServer(t *testing.T) *httptest.Server {
	t.Helper()
	// demo.Enable sets CORRAL_INCUS_REMOTE for the whole process, which would
	// otherwise leave every later test in this package with an extra Incus
	// context that nothing stubs. Claiming the variable first makes the test
	// framework put it back afterwards.
	t.Setenv("CORRAL_INCUS_REMOTE", "")
	EnableDemo()
	tmpDir := t.TempDir()
	store = registry.NewStoreAt(tmpDir + "/registry.json")
	t.Cleanup(func() {
		// Restore the seams so later tests in this package start clean.
		config.SetForceKubevirtContext(false)
		qemu.SetStateDirs("", "")
		f := NewTestFixture()
		f.Close()
	})
	mux, err := newMux()
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, srv *httptest.Server, path string, out any) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

func TestDemoMode_EndToEnd(t *testing.T) {
	srv := newDemoServer(t)

	// The fleet lists with real derived state.
	var vms []map[string]any
	getJSON(t, srv, "/api/vms", &vms)
	if len(vms) < 8 {
		t.Fatalf("demo fleet has %d VMs, want >= 8", len(vms))
	}
	byName := map[string]map[string]any{}
	for _, v := range vms {
		byName[v["name"].(string)] = v
	}
	if v := byName["web-prod"]; v == nil || v["running"] != true || v["ip"] != "10.42.1.20" {
		t.Errorf("web-prod not running with its IP: %+v", byName["web-prod"])
	}
	if v := byName["win11-desktop"]; v == nil || !strings.Contains(v["status"].(string), "Paused") {
		t.Errorf("win11-desktop should be paused: %+v", byName["win11-desktop"])
	}

	// CTs and nodes populate.
	var cts []map[string]any
	getJSON(t, srv, "/api/cts", &cts)
	if len(cts) != 2 {
		t.Errorf("demo has %d CTs, want 2", len(cts))
	}
	// Local backend fixture (#91 Phase 4): a fake qemu VM under a "local" node.
	if v := byName["laptop-dev"]; v == nil || v["backend"] != "qemu" || v["namespace"] != "local" {
		t.Errorf("demo local VM missing or misshaped: %+v", byName["laptop-dev"])
	}
	var nodes []map[string]any
	getJSON(t, srv, "/api/nodes", &nodes)
	if len(nodes) != 4 { // 3 cluster nodes + the synthetic local node
		t.Errorf("demo has %d nodes, want 4: %+v", len(nodes), nodes)
	}

	// Cluster checks are all green (local checks depend on the CI host —
	// /dev/kvm and installed CLIs — so only their presence is asserted).
	var checks []map[string]any
	getJSON(t, srv, "/api/doctor", &checks)
	names := map[string]bool{}
	for _, c := range checks {
		names[c["name"].(string)] = true
		if local := map[string]bool{
			"QEMU (local backend)": true, "KVM acceleration": true,
			"Tailscale CLI": true, "virtctl CLI": true,
		}; local[c["name"].(string)] {
			continue
		}
		if c["ok"] != true {
			t.Errorf("doctor check %q not OK in demo: %v", c["name"], c["detail"])
		}
	}
	for _, want := range []string{"KubeVirt installed", "Default StorageClass", "QEMU (local backend)"} {
		if !names[want] {
			t.Errorf("doctor check %q missing", want)
		}
	}

	// Stop is stateful: the VM's derived state flips.
	resp, err := http.Post(srv.URL+"/api/vms/corral-vms/web-prod/stop", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stop web-prod: %v status=%v", err, resp.StatusCode)
	}
	resp.Body.Close()
	getJSON(t, srv, "/api/vms", &vms)
	for _, v := range vms {
		if v["name"] == "web-prod" && v["running"] != false {
			t.Errorf("web-prod still running after stop: %+v", v)
		}
	}

	// Per-VM live metrics flow through the real metrics-server code path.
	var m map[string]string
	getJSON(t, srv, "/api/vms/corral-vms/db-prod/metrics", &m)
	if m["cpu"] == "" {
		t.Errorf("db-prod live cpu empty: %+v", m)
	}
}
