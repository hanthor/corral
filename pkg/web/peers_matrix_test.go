package web

// Peering across backends and failure modes. peers_test.go covers the happy
// path for a single KubeVirt peer over the legacy /api/vms fallback; this file
// covers the protocol-v1 inventory path, peers whose fleets are not KubeVirt,
// name collisions between peers, credential forwarding, and what the dashboard
// does when a peer misbehaves.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
	"golang.org/x/net/websocket"
)

// peerStub is a fake remote Corral. It answers the protocol-v1 inventory
// endpoint and records every request, so tests can assert both what the local
// server asked for and what credentials it sent.
type peerStub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	vms      []types.VM
	// legacyOnly makes the stub 404 /api/v1/inventory, as a Corral predating
	// the inventory endpoint would, forcing the /api/vms fallback.
	legacyOnly bool
}

func newPeerStub(t *testing.T, vms ...types.VM) *peerStub {
	t.Helper()
	p := &peerStub{vms: vms}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Clone(r.Context()))
		p.mu.Unlock()

		switch {
		case r.URL.Path == "/api/v1/inventory" && !p.legacyOnly:
			json.NewEncoder(w).Encode(map[string]any{"vms": p.vms})
		case r.URL.Path == "/api/v1/inventory":
			http.NotFound(w, r)
		case r.URL.Path == "/api/vms":
			json.NewEncoder(w).Encode(p.vms)
		case strings.HasPrefix(r.URL.Path, "/api/vms/"):
			json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "method": r.Method})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *peerStub) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.requests {
		out = append(out, r.URL.Path)
	}
	return out
}

func (p *peerStub) authFor(path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.requests {
		if r.URL.Path == path {
			return r.Header.Get("Authorization")
		}
	}
	return ""
}

// isolatedPeerHome gives the test its own config file, so peers registered
// here don't leak into other tests (and don't pick up the developer's).
func isolatedPeerHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	t.Setenv("CORRAL_KUBE_CONTEXT", "")
	t.Setenv("CORRAL_INCUS_REMOTE", "")
	t.Setenv("CORRAL_LIBVIRT_URI", "")
	t.Setenv("CORRAL_DEFAULT_BACKEND", "")
}

func listVMs(t *testing.T, fx *TestFixture) []map[string]any {
	t.Helper()
	resp := mustGet(t, fx.Server.URL+"/api/vms")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/vms: status %d", resp.StatusCode)
	}
	var vms []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	return vms
}

// The inventory endpoint is the preferred route — it carries the peer's own
// per-context errors and identities, where /api/vms is a bare list. Only the
// 404 fallback was covered before, so a break here would have gone unnoticed.
func TestPeer_PrefersInventoryEndpoint(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t, types.VM{Name: "desk", Namespace: "corral-vms", Backend: "kubevirt", Running: true})
	if err := config.SetPeer("cluster-a", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	vms := listVMs(t, fx)
	if len(vms) != 1 || vms[0]["name"] != "desk" {
		t.Fatalf("peer fleet not aggregated: %+v", vms)
	}
	for _, p := range peer.paths() {
		if p == "/api/vms" {
			t.Errorf("fell back to /api/vms despite a working inventory endpoint: %v", peer.paths())
		}
	}
}

func TestPeer_FallsBackToLegacyListing(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t, types.VM{Name: "desk", Namespace: "corral-vms", Backend: "kubevirt"})
	peer.legacyOnly = true
	if err := config.SetPeer("old", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	if vms := listVMs(t, fx); len(vms) != 1 {
		t.Fatalf("legacy peer fleet not aggregated: %+v", vms)
	}
	var sawFallback bool
	for _, p := range peer.paths() {
		sawFallback = sawFallback || p == "/api/vms"
	}
	if !sawFallback {
		t.Errorf("never tried /api/vms after the 404: %v", peer.paths())
	}
}

// A peer's fleet is whatever that peer runs — it does not have to be KubeVirt.
// Backend and context have to survive aggregation, because the dashboard gates
// per-VM actions on them.
func TestPeer_PreservesEveryBackend(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t,
		types.VM{Name: "cluster-vm", Namespace: "corral-vms", Backend: "kubevirt", Context: "talos"},
		types.VM{Name: "laptop-vm", Namespace: "local", Backend: "qemu"},
		types.VM{Name: "ct-vm", Namespace: "incus", Backend: "incus", Context: "homelab"},
		types.VM{Name: "hv-vm", Namespace: "libvirt", Backend: "libvirt", Context: "qemu+ssh://hv/system"},
	)
	if err := config.SetPeer("branch", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	byName := map[string]map[string]any{}
	for _, vm := range listVMs(t, fx) {
		byName[vm["name"].(string)] = vm
	}
	for _, tc := range []struct{ name, backend, context string }{
		{"cluster-vm", "kubevirt", "talos"},
		{"laptop-vm", "qemu", "branch"}, // no remote context: the peer name stands in
		{"ct-vm", "incus", "homelab"},
		{"hv-vm", "libvirt", "qemu+ssh://hv/system"},
	} {
		vm := byName[tc.name]
		if vm == nil {
			t.Errorf("%s missing from the aggregated fleet", tc.name)
			continue
		}
		if vm["backend"] != tc.backend {
			t.Errorf("%s backend = %v, want %v", tc.name, vm["backend"], tc.backend)
		}
		if vm["context"] != tc.context {
			t.Errorf("%s context = %v, want %v", tc.name, vm["context"], tc.context)
		}
		if vm["peer"] != "branch" {
			t.Errorf("%s peer = %v, want branch", tc.name, vm["peer"])
		}
	}
}

// Two peers running the same-named VM is the whole reason instances carry a
// peer-scoped identity. Both have to appear, and they have to be tellable
// apart — otherwise a bare-name selector silently picks one at random.
func TestPeer_CollidingNamesStayDistinct(t *testing.T) {
	isolatedPeerHome(t)
	a := newPeerStub(t, types.VM{Name: "dev", Namespace: "corral-vms", Backend: "kubevirt"})
	b := newPeerStub(t, types.VM{Name: "dev", Namespace: "corral-vms", Backend: "kubevirt"})
	if err := config.SetPeer("site-a", a.URL); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPeer("site-b", b.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	vms := listVMs(t, fx)
	if len(vms) != 2 {
		t.Fatalf("want both peers' dev VMs, got %+v", vms)
	}
	ids := map[string]bool{}
	namespaces := map[string]bool{}
	for _, vm := range vms {
		ids[fmt.Sprint(vm["id"])] = true
		namespaces[fmt.Sprint(vm["namespace"])] = true
	}
	if len(namespaces) != 2 {
		t.Errorf("peer namespaces collided: %v", namespaces)
	}
	if len(ids) != 2 {
		t.Errorf("peer VM identities collided — a bare-name selector cannot tell them apart: %v", ids)
	}
}

// The encoded namespace has to round-trip: an action on a peer VM must reach
// the peer's own namespace for that VM, which differs per backend.
func TestPeer_ActionProxyRestoresRemoteNamespace(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t,
		types.VM{Name: "cluster-vm", Namespace: "corral-vms", Backend: "kubevirt"},
		types.VM{Name: "laptop-vm", Namespace: "local", Backend: "qemu"},
		types.VM{Name: "ct-vm", Namespace: "incus", Backend: "incus"},
	)
	if err := config.SetPeer("branch", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	encoded := map[string]string{}
	for _, vm := range listVMs(t, fx) {
		encoded[vm["name"].(string)] = vm["namespace"].(string)
	}
	for _, tc := range []struct{ name, remoteNS string }{
		{"cluster-vm", "corral-vms"},
		{"laptop-vm", "local"},
		{"ct-vm", "incus"},
	} {
		ns := encoded[tc.name]
		if ns == "" {
			t.Fatalf("%s missing from the fleet", tc.name)
		}
		resp, err := http.Post(fx.Server.URL+"/api/vms/"+ns+"/"+tc.name+"/start", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		want := "/api/vms/" + tc.remoteNS + "/" + tc.name + "/start"
		if got["path"] != want {
			t.Errorf("%s proxied to %q, want %q", tc.name, got["path"], want)
		}
	}
}

// peerGate exists so newly-added VM subresources federate without being listed
// anywhere. Anything under a peer namespace has to reach the peer, not the
// local Kubernetes context.
func TestPeer_ArbitrarySubresourcesFederate(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t, types.VM{Name: "desk", Namespace: "corral-vms", Backend: "kubevirt"})
	if err := config.SetPeer("cluster-a", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	ns := listVMs(t, fx)[0]["namespace"].(string)
	for _, sub := range []string{"snapshots", "metrics", "events", "guestinfo"} {
		resp := mustGet(t, fx.Server.URL+"/api/vms/"+ns+"/desk/"+sub)
		var got map[string]string
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		want := "/api/vms/corral-vms/desk/" + sub
		if got["path"] != want {
			t.Errorf("%s proxied to %q, want %q", sub, got["path"], want)
		}
	}
}

// A peer token is a credential for the whole federated surface, not just the
// initial listing — dropping it on the proxy leg would 401 every action.
func TestPeer_ForwardsTokenOnListingAndProxy(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t, types.VM{Name: "desk", Namespace: "corral-vms", Backend: "kubevirt"})
	if err := config.SetPeerWithToken("secure", peer.URL, "s3cret"); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	ns := listVMs(t, fx)[0]["namespace"].(string)
	resp, err := http.Post(fx.Server.URL+"/api/vms/"+ns+"/desk/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, path := range []string{"/api/v1/inventory", "/api/vms/corral-vms/desk/start"} {
		if got := peer.authFor(path); got != "Bearer s3cret" {
			t.Errorf("%s Authorization = %q, want %q", path, got, "Bearer s3cret")
		}
	}
}

// One unreachable peer must not take the dashboard down with it: the rest of
// the fleet still renders. This is the multi-site failure mode — a peer over a
// flaky link is normal, not exceptional.
func TestPeer_UnreachablePeerDoesNotHideHealthyOnes(t *testing.T) {
	isolatedPeerHome(t)
	healthy := newPeerStub(t, types.VM{Name: "alive", Namespace: "corral-vms", Backend: "kubevirt"})
	dead := newPeerStub(t)
	deadURL := dead.URL
	dead.Close() // nothing listening from here on

	if err := config.SetPeer("up", healthy.URL); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPeer("down", deadURL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	vms := listVMs(t, fx)
	if len(vms) != 1 || vms[0]["name"] != "alive" {
		t.Fatalf("healthy peer's fleet lost to the dead one: %+v", vms)
	}
}

// A peer that answers with an error, rather than not answering, is the same
// situation: skip it, keep serving.
func TestPeer_ErroringPeerIsSkipped(t *testing.T) {
	isolatedPeerHome(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	healthy := newPeerStub(t, types.VM{Name: "alive", Namespace: "corral-vms", Backend: "kubevirt"})

	if err := config.SetPeer("broken", broken.URL); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPeer("up", healthy.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	vms := listVMs(t, fx)
	if len(vms) != 1 || vms[0]["name"] != "alive" {
		t.Fatalf("want only the healthy peer's VM, got %+v", vms)
	}
}

// A VM deleted on the peer has to disappear here too. The cached inventory
// backs console routing, so a stale entry would send a console session at an
// address that no longer belongs to that VM.
func TestPeer_DeletedRemoteVMLeavesTheFleet(t *testing.T) {
	isolatedPeerHome(t)
	peer := newPeerStub(t,
		types.VM{Name: "keeper", Namespace: "corral-vms", Backend: "kubevirt"},
		types.VM{Name: "doomed", Namespace: "corral-vms", Backend: "kubevirt"},
	)
	if err := config.SetPeer("cluster-a", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	if vms := listVMs(t, fx); len(vms) != 2 {
		t.Fatalf("want 2 peer VMs, got %+v", vms)
	}

	peer.mu.Lock()
	peer.vms = peer.vms[:1]
	peer.mu.Unlock()

	vms := listVMs(t, fx)
	if len(vms) != 1 || vms[0]["name"] != "keeper" {
		t.Fatalf("deleted peer VM still listed: %+v", vms)
	}
	if _, stale := peerInventory.Load(vms[0]["namespace"].(string) + "/doomed"); stale {
		t.Error("deleted peer VM left behind in the console-routing cache")
	}
}

// ── Peer consoles ─────────────────────────────────────────────────

// The console bridge prefers the least-indirect route: if the peer advertises
// a guest endpoint this machine can open (the usual case on a shared tailnet),
// the session goes straight there instead of round-tripping through the remote
// Corral.
func TestPeerConsole_PrefersDirectGuestEndpoint(t *testing.T) {
	isolatedPeerHome(t)

	guest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := guest.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	peer := newPeerStub(t, types.VM{
		Name: "desk", Namespace: "corral-vms", Backend: "kubevirt",
		Endpoints: map[string]string{"vnc": guest.Addr().String()},
	})
	if err := config.SetPeer("cluster-a", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	ns := listVMs(t, fx)[0]["namespace"].(string)
	ws, err := websocket.Dial(wsURL(fx.Server.URL)+"/api/vnc/"+ns+"/desk", "", "http://localhost")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	if _, err := ws.Write([]byte("rfb-hello")); err != nil {
		t.Fatalf("ws.Write: %v", err)
	}
	select {
	case conn := <-accepted:
		defer conn.Close()
		buf := make([]byte, 32)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("guest read: %v", err)
		}
		if got := string(buf[:n]); got != "rfb-hello" {
			t.Errorf("guest got %q, want %q", got, "rfb-hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("console never reached the advertised guest endpoint")
	}
}

// With no route to the guest — a peer behind NAT, or a VM with no advertised
// address — the session falls back to relaying through the remote Corral's own
// console websocket, at that peer's namespace for the VM.
func TestPeerConsole_FallsBackToPeerRelay(t *testing.T) {
	isolatedPeerHome(t)

	relayPath := make(chan string, 1)
	relay := websocket.Handler(func(c *websocket.Conn) {
		relayPath <- c.Request().URL.Path
		io.Copy(c, c) // echo, so the round trip is observable
	})
	peer := &peerStub{vms: []types.VM{{Name: "desk", Namespace: "corral-vms", Backend: "kubevirt"}}}
	peer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/vnc/") {
			relay.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/inventory" {
			json.NewEncoder(w).Encode(map[string]any{"vms": peer.vms})
			return
		}
		http.NotFound(w, r)
	}))
	defer peer.Close()

	if err := config.SetPeer("cluster-a", peer.URL); err != nil {
		t.Fatal(err)
	}
	fx := NewTestFixture()
	defer fx.Close()

	ns := listVMs(t, fx)[0]["namespace"].(string)
	ws, err := websocket.Dial(wsURL(fx.Server.URL)+"/api/vnc/"+ns+"/desk", "", "http://localhost")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	if _, err := ws.Write([]byte("rfb-hello")); err != nil {
		t.Fatalf("ws.Write: %v", err)
	}
	select {
	case got := <-relayPath:
		if want := "/api/vnc/corral-vms/desk"; got != want {
			t.Errorf("relayed to %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("console never relayed through the peer")
	}

	buf := make([]byte, 32)
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := ws.Read(buf)
	if err != nil {
		t.Fatalf("ws.Read: %v", err)
	}
	if got := string(buf[:n]); got != "rfb-hello" {
		t.Errorf("relay echoed %q, want %q", got, "rfb-hello")
	}
}
