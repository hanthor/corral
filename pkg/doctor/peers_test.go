package doctor

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
)

// fakePeer is a stand-in remote Corral whose every response is controllable,
// so peer diagnostics are exercised without a second Corral running anywhere.
type fakePeer struct {
	*httptest.Server
	meta      *peerMeta // nil → /api/v1/meta 404s, as a pre-v1 Corral would
	vms       []types.VM
	token     string // non-empty → reads require this bearer token
	legacy    bool   // 404 /api/v1/inventory, forcing the /api/vms fallback
	metaError int    // non-zero → /api/v1/meta answers this status
}

func newFakePeer(t *testing.T, f *fakePeer) *fakePeer {
	t.Helper()
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/meta":
			if f.metaError != 0 {
				http.Error(w, "nope", f.metaError)
				return
			}
			if f.meta == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(f.meta)
		case "/api/v1/inventory":
			if f.legacy {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"vms": f.vms})
		case "/api/vms":
			json.NewEncoder(w).Encode(f.vms)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func fullMeta() *peerMeta {
	return &peerMeta{Protocol: 1, Features: []string{"inventory", "peer-relay", "service-token"}}
}

func peerCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not reported; got %s", name, checkNames(checks))
	return Check{}
}

func checkNames(checks []Check) string {
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

func TestRunPeers_HealthyPeer(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{
		meta: fullMeta(),
		vms:  []types.VM{{Name: "desk", Backend: "kubevirt"}},
	})

	checks := RunPeers([]config.PeerConfig{{Name: "site-a", URL: peer.URL}})
	for _, name := range []string{"Peer reachable", "Peer API compatibility", "Peer authentication", "Peer console relay"} {
		if c := peerCheck(t, checks, name); !c.OK {
			t.Errorf("%q failed on a healthy peer: %s", name, c.Detail)
		}
	}
	for _, c := range checks {
		if c.Backend != "peer" || c.Context != "site-a" || c.Severity == "" {
			t.Errorf("unscoped result: %+v", c)
		}
	}
}

func TestRunPeers_UnreachablePeerIsRequiredFailure(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{meta: fullMeta()})
	url := peer.URL
	peer.Close()

	checks := RunPeers([]config.PeerConfig{{Name: "down", URL: url}})
	reachable := peerCheck(t, checks, "Peer reachable")
	if reachable.OK {
		t.Error("a peer with nothing listening reported reachable")
	}
	if reachable.Severity != "required" {
		t.Errorf("severity = %q, want required", reachable.Severity)
	}
	// Nothing else is knowable once the peer is silent — don't emit a wall of
	// derived failures.
	if len(checks) != 1 {
		t.Errorf("want a single explanatory row, got %s", checkNames(checks))
	}
}

// A peer that gates reads is diagnosed as an auth problem, not an outage —
// the remedy is completely different.
func TestRunPeers_RejectedToken(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{meta: fullMeta(), token: "right"})

	checks := RunPeers([]config.PeerConfig{{Name: "secure", URL: peer.URL, Token: "wrong"}})
	if c := peerCheck(t, checks, "Peer reachable"); !c.OK {
		t.Errorf("peer is up but reported unreachable: %s", c.Detail)
	}
	auth := peerCheck(t, checks, "Peer authentication")
	if auth.OK {
		t.Error("a rejected token reported as authenticated")
	}
	if !strings.Contains(auth.Detail, "reissue") {
		t.Errorf("no actionable remedy: %q", auth.Detail)
	}
	if strings.Contains(auth.Detail, "wrong") {
		t.Errorf("remedy leaked the token: %q", auth.Detail)
	}
}

func TestRunPeers_MissingTokenIsDistinctFromRejectedOne(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{meta: fullMeta(), token: "required"})

	checks := RunPeers([]config.PeerConfig{{Name: "secure", URL: peer.URL}})
	auth := peerCheck(t, checks, "Peer authentication")
	if auth.OK {
		t.Error("an unauthenticated peer read reported as authenticated")
	}
	if !strings.Contains(auth.Detail, "no token is stored") {
		t.Errorf("remedy does not name the missing credential: %q", auth.Detail)
	}
}

func TestRunPeers_AcceptedToken(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{
		meta: fullMeta(),
		vms:  []types.VM{{Name: "desk", Backend: "kubevirt"}},
		// The stub also proves the token reaches the inventory leg, not just meta.
		token: "s3cret",
	})

	checks := RunPeers([]config.PeerConfig{{Name: "secure", URL: peer.URL, Token: "s3cret"}})
	auth := peerCheck(t, checks, "Peer authentication")
	if !auth.OK {
		t.Fatalf("accepted token reported as failing: %s", auth.Detail)
	}
	if !strings.Contains(auth.Detail, "stored token accepted") {
		t.Errorf("detail does not distinguish an authenticated peer: %q", auth.Detail)
	}
}

// A pre-v1 peer still federates through the legacy listing, so it is a warning
// with an upgrade hint rather than a required failure.
func TestRunPeers_PreV1PeerDegradesToWarning(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{
		legacy: true, // no /api/v1/meta, no /api/v1/inventory
		vms:    []types.VM{{Name: "old", Backend: "kubevirt"}},
	})

	checks := RunPeers([]config.PeerConfig{{Name: "old", URL: peer.URL}})
	compat := peerCheck(t, checks, "Peer API compatibility")
	if compat.Severity != "warning" {
		t.Errorf("severity = %q, want warning — the legacy path still works", compat.Severity)
	}
	if !strings.Contains(compat.Detail, "Upgrade") {
		t.Errorf("no upgrade hint: %q", compat.Detail)
	}
	// The legacy listing is still readable, so auth is still diagnosable.
	if c := peerCheck(t, checks, "Peer authentication"); !c.OK {
		t.Errorf("legacy listing not consulted: %s", c.Detail)
	}
	if c := peerCheck(t, checks, "Peer console relay"); c.OK {
		t.Errorf("relay claimed for a peer that advertises nothing: %s", c.Detail)
	}
}

func TestRunPeers_ProtocolMismatchStopsEarly(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{meta: &peerMeta{Protocol: 99, Features: []string{"inventory"}}})

	checks := RunPeers([]config.PeerConfig{{Name: "future", URL: peer.URL}})
	compat := peerCheck(t, checks, "Peer API compatibility")
	if compat.OK {
		t.Error("an unfederatable protocol reported as compatible")
	}
	if !strings.Contains(compat.Detail, "99") {
		t.Errorf("detail does not name the peer's protocol: %q", compat.Detail)
	}
	for _, c := range checks {
		if c.Name == "Peer authentication" {
			t.Error("kept probing a peer this Corral cannot federate with")
		}
	}
}

func TestRunPeers_MissingFeatureIsNamed(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{meta: &peerMeta{Protocol: 1, Features: []string{"inventory"}}})

	checks := RunPeers([]config.PeerConfig{{Name: "partial", URL: peer.URL}})
	if c := peerCheck(t, checks, "Peer API compatibility"); !strings.Contains(c.Detail, "peer-relay") {
		t.Errorf("missing feature not named: %q", c.Detail)
	}
	if c := peerCheck(t, checks, "Peer console relay"); c.OK {
		t.Errorf("relay reported available without the feature: %s", c.Detail)
	}
}

// The direct-guest probe is what decides whether a console session goes
// straight to the VM or round-trips through the peer, so it has to reflect
// actual reachability from this machine.
func TestRunPeers_ReachableGuestEndpoint(t *testing.T) {
	guest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	go func() {
		for {
			conn, err := guest.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	peer := newFakePeer(t, &fakePeer{
		meta: fullMeta(),
		vms: []types.VM{{
			Name: "desk", Backend: "kubevirt",
			Endpoints: map[string]string{"vnc": guest.Addr().String()},
		}},
	})

	checks := RunPeers([]config.PeerConfig{{Name: "site-a", URL: peer.URL}})
	direct := peerCheck(t, checks, "Peer guest reachability")
	if !direct.OK {
		t.Errorf("advertised, listening endpoint reported unreachable: %s", direct.Detail)
	}
	if !strings.Contains(direct.Detail, "desk") {
		t.Errorf("detail does not name the probed instance: %q", direct.Detail)
	}
}

func TestRunPeers_UnreachableGuestFallsBackToRelay(t *testing.T) {
	// Port 1 on loopback: reserved, and nothing will be listening.
	peer := newFakePeer(t, &fakePeer{
		meta: fullMeta(),
		vms: []types.VM{{
			Name: "desk", Backend: "kubevirt",
			Endpoints: map[string]string{"vnc": "127.0.0.1:1"},
		}},
	})

	checks := RunPeers([]config.PeerConfig{{Name: "site-a", URL: peer.URL}})
	direct := peerCheck(t, checks, "Peer guest reachability")
	if direct.OK {
		t.Error("nothing is listening, yet the guest reported reachable")
	}
	if direct.Severity != "warning" {
		t.Errorf("severity = %q, want warning — the relay covers this", direct.Severity)
	}
	if !strings.Contains(direct.Detail, "relay") {
		t.Errorf("detail does not explain the fallback: %q", direct.Detail)
	}
	if c := peerCheck(t, checks, "Peer console relay"); !c.OK {
		t.Errorf("relay unavailable too — consoles would not work at all: %s", c.Detail)
	}
}

func TestRunPeers_NoAdvertisedEndpointIsNotAFailure(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{
		meta: fullMeta(),
		vms:  []types.VM{{Name: "headless", Backend: "kubevirt"}},
	})

	checks := RunPeers([]config.PeerConfig{{Name: "site-a", URL: peer.URL}})
	direct := peerCheck(t, checks, "Peer guest reachability")
	if direct.Severity != "warning" {
		t.Errorf("severity = %q, want warning", direct.Severity)
	}
	if !strings.Contains(direct.Detail, "relay") {
		t.Errorf("detail does not say what happens instead: %q", direct.Detail)
	}
}

// A URL pointing at something that isn't Corral — a proxy, an SSO login — is
// its own diagnosis, not a generic outage.
func TestRunPeers_NonCorralEndpoint(t *testing.T) {
	peer := newFakePeer(t, &fakePeer{metaError: http.StatusBadGateway})

	checks := RunPeers([]config.PeerConfig{{Name: "proxy", URL: peer.URL}})
	reachable := peerCheck(t, checks, "Peer reachable")
	if reachable.OK {
		t.Error("a 502 reported as reachable")
	}
	if !strings.Contains(reachable.Detail, "502") {
		t.Errorf("detail does not name the status: %q", reachable.Detail)
	}
}

func TestRunPeers_OnePeerDoesNotHideAnother(t *testing.T) {
	dead := newFakePeer(t, &fakePeer{meta: fullMeta()})
	deadURL := dead.URL
	dead.Close()
	healthy := newFakePeer(t, &fakePeer{meta: fullMeta(), vms: []types.VM{{Name: "desk"}}})

	checks := RunPeers([]config.PeerConfig{
		{Name: "down", URL: deadURL},
		{Name: "up", URL: healthy.URL},
	})
	var sawHealthy bool
	for _, c := range checks {
		if c.Context == "up" && c.Name == "Peer authentication" && c.OK {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Fatalf("healthy peer not diagnosed after a dead one: %s", checkNames(checks))
	}
}

func TestRunPeers_NoPeersConfigured(t *testing.T) {
	if checks := RunPeers(nil); len(checks) != 0 {
		t.Errorf("want no rows without peers, got %s", checkNames(checks))
	}
}
