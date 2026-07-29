package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
)

func TestCorralPeerInventoryAndActionProxy(t *testing.T) {
	action := make(chan string, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/vms":
			json.NewEncoder(w).Encode([]map[string]any{{"name": "desk", "namespace": "corral-vms", "backend": "kubevirt", "running": true}})
		case "/api/vms/corral-vms/desk/start":
			action <- r.Method
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	t.Setenv("HOME", t.TempDir())
	if err := config.SetPeer("cluster-a", remote.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()
	fx.Runner.AddResponse("kubectl get vms -A -o json", `{"items":[]}`, nil)
	fx.Runner.AddResponse("kubectl get vmis -A -o json", `{"items":[]}`, nil)
	resp, err := http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	var vms []map[string]any
	json.NewDecoder(resp.Body).Decode(&vms)
	resp.Body.Close()
	var ns string
	for _, v := range vms {
		if v["peer"] == "cluster-a" {
			ns, _ = v["namespace"].(string)
		}
	}
	if !strings.HasPrefix(ns, "peer-") {
		t.Fatalf("peer VM missing: %+v", vms)
	}
	resp, err = http.Post(fx.Server.URL+"/api/vms/"+ns+"/desk/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy status %d", resp.StatusCode)
	}
	if got := <-action; got != "POST" {
		t.Fatalf("remote got %s", got)
	}
}

func TestCorralPeerWorksWithoutLocalKubeconfig(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"name": "remote-only", "namespace": "vms", "backend": "kubevirt"}})
	}))
	defer remote.Close()
	t.Setenv("HOME", t.TempDir())
	if err := config.SetPeer("remote", remote.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close() // deliberately no kubectl responses
	resp, err := http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("peer-only dashboard status %d, want 200", resp.StatusCode)
	}
}
