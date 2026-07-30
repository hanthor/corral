package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
)

// peerClient is separate from the runner seam the backend checks use: peers
// are diagnosed over HTTP, not by shelling out. The timeout is deliberately
// short — a diagnostic that hangs on an unreachable site is worse than one
// that reports the site as unreachable.
var peerClient = &http.Client{Timeout: 5 * time.Second}

// peerDialTimeout bounds the direct-guest probe. It mirrors the budget
// pkg/web's console bridge gives a guest endpoint before falling back to the
// relay, so what the doctor reports matches what a console session will do.
const peerDialTimeout = 1500 * time.Millisecond

// RunPeers diagnoses every configured Corral peer: whether it answers, whether
// its API is one this Corral can federate with, whether the stored credential
// is accepted, and which of the two console routes — direct to the guest, or
// relayed through the peer — is actually available.
//
// Peers are not compute contexts, so they get their own entry point rather
// than being forced through RunContexts. Nothing here touches the shell runner,
// so it must not be called while RunContexts holds runnerMu.
func RunPeers(peers []config.PeerConfig) []Check {
	var all []Check
	for _, peer := range peers {
		started := time.Now()
		checks := peerChecks(peer)
		latency := time.Since(started).Round(time.Millisecond)
		for i := range checks {
			checks[i].Backend = "peer"
			checks[i].Context = peer.Name
			if checks[i].Severity == "" {
				checks[i].Severity = "required"
			}
			if i == 0 {
				checks[i].Detail += fmt.Sprintf(" (probe %s)", latency)
			}
		}
		all = append(all, checks...)
	}
	return all
}

// peerMeta is the /api/v1/meta payload: the protocol version this Corral
// federates over, and the optional capabilities the peer advertises.
type peerMeta struct {
	Protocol int      `json:"protocol"`
	Features []string `json:"features"`
}

func peerChecks(peer config.PeerConfig) []Check {
	if strings.TrimSpace(peer.URL) == "" {
		return []Check{{Name: "Peer reachable", Detail: "no URL configured; re-add it with `corral peer add`"}}
	}

	meta, status, err := fetchPeerMeta(peer)
	switch {
	case err != nil:
		return []Check{{Name: "Peer reachable", Detail: fmt.Sprintf("%s did not answer: %v; verify the URL, that `corral web` is running there, and tailnet connectivity", peer.URL, err)}}

	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The peer is up and gating reads, so the credential is the problem.
		return []Check{
			{Name: "Peer reachable", OK: true, Detail: "answered at " + peer.URL},
			{Name: "Peer authentication", Detail: authRemedy(peer, status)},
		}

	case status == http.StatusNotFound:
		// Predates /api/v1/meta. peerVMs falls back to /api/vms, so this is
		// degraded rather than broken — worth naming, not worth failing.
		return append([]Check{
			{Name: "Peer reachable", OK: true, Detail: "answered at " + peer.URL},
			{Name: "Peer API compatibility", Severity: "warning", Detail: "no /api/v1/meta; peer predates the v1 API — inventory falls back to the legacy listing and per-context errors are not reported. Upgrade Corral on that host"},
		}, peerAuthAndRoutes(peer, nil)...)

	case status != http.StatusOK:
		return []Check{{Name: "Peer reachable", Detail: fmt.Sprintf("%s answered HTTP %d; check that the URL points at a Corral web API and not a proxy or login page", peer.URL, status)}}
	}

	checks := []Check{{Name: "Peer reachable", OK: true, Detail: "answered at " + peer.URL}}

	compatible := meta.Protocol == types.PeerProtocol
	detail := fmt.Sprintf("protocol %d", meta.Protocol)
	if !compatible {
		detail = fmt.Sprintf("peer speaks protocol %d, this Corral speaks %d; upgrade the older side", meta.Protocol, types.PeerProtocol)
	} else if missing := missingFeatures(meta.Features); len(missing) > 0 {
		detail += fmt.Sprintf(", missing %s; those capabilities degrade to the legacy path", strings.Join(missing, ", "))
	} else {
		detail += ", all federation features advertised"
	}
	checks = append(checks, Check{Name: "Peer API compatibility", OK: compatible, Detail: detail})
	if !compatible {
		return checks
	}
	return append(checks, peerAuthAndRoutes(peer, meta)...)
}

// federationFeatures are the capabilities peering relies on. A peer missing
// one still works through an older path, so their absence is a warning.
var federationFeatures = []string{"inventory", "peer-relay"}

func missingFeatures(advertised []string) []string {
	have := map[string]bool{}
	for _, f := range advertised {
		have[f] = true
	}
	var missing []string
	for _, want := range federationFeatures {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	return missing
}

// peerAuthAndRoutes checks the credential by actually reading the inventory,
// then reports which console route that inventory makes available.
func peerAuthAndRoutes(peer config.PeerConfig, meta *peerMeta) []Check {
	vms, status, err := fetchPeerInventory(peer)
	if err != nil {
		return []Check{{Name: "Peer authentication", Detail: fmt.Sprintf("inventory request failed: %v", err)}}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return []Check{{Name: "Peer authentication", Detail: authRemedy(peer, status)}}
	}
	if status != http.StatusOK {
		return []Check{{Name: "Peer authentication", Detail: fmt.Sprintf("inventory returned HTTP %d", status)}}
	}

	credential := "peer allows unauthenticated reads"
	if peer.Token != "" {
		credential = "stored token accepted"
	}
	checks := []Check{{Name: "Peer authentication", OK: true, Detail: fmt.Sprintf("%s; %d instance(s) visible", credential, len(vms))}}

	return append(checks, consoleRouteChecks(vms, meta)...)
}

// consoleRouteChecks report the two routes a peer console can take, in the
// order the bridge tries them: straight to the guest, then relayed through the
// peer. Both are warnings — a peer whose guests aren't directly reachable is
// normal (that is what the relay is for), and a peer with no instances yet
// says nothing about either route.
func consoleRouteChecks(vms []types.VM, meta *peerMeta) []Check {
	relay := Check{Name: "Peer console relay", Severity: "warning"}
	switch {
	case meta == nil:
		relay.Detail = "peer predates capability advertisement; relay support unknown until a console is opened"
	case containsFeature(meta.Features, "peer-relay"):
		relay.OK = true
		relay.Detail = "peer advertises relay; consoles work even when guests are unreachable from here"
	default:
		relay.Detail = "peer does not advertise relay; consoles need a directly reachable guest endpoint"
	}

	direct := Check{Name: "Peer guest reachability", Severity: "warning"}
	address, vmName := firstGuestEndpoint(vms)
	switch {
	case len(vms) == 0:
		direct.Detail = "no instances to probe"
	case address == "":
		direct.Detail = "no instance advertises a console endpoint; every session will take the relay"
	default:
		conn, err := net.DialTimeout("tcp", address, peerDialTimeout)
		if err != nil {
			direct.Detail = fmt.Sprintf("%s advertises %s but it is not reachable from here (%v); sessions will take the relay", vmName, address, err)
		} else {
			conn.Close()
			direct.OK = true
			direct.Detail = fmt.Sprintf("%s reachable at %s; consoles connect straight to the guest", vmName, address)
		}
	}
	return []Check{direct, relay}
}

// firstGuestEndpoint picks one advertised console address to probe, preferring
// an explicit endpoint over an IP with an assumed port — the same order the
// console bridge resolves in.
func firstGuestEndpoint(vms []types.VM) (address, name string) {
	for _, vm := range vms {
		for _, kind := range []string{"vnc", "rdp"} {
			if addr := vm.Endpoints[kind]; addr != "" {
				return addr, vm.Name
			}
		}
	}
	for _, vm := range vms {
		if vm.IP != "" && vm.Capabilities.VNC {
			return net.JoinHostPort(vm.IP, "5900"), vm.Name
		}
	}
	return "", ""
}

func containsFeature(features []string, want string) bool {
	for _, f := range features {
		if f == want {
			return true
		}
	}
	return false
}

// authRemedy never echoes the token itself — PeerConfig keeps it out of JSON
// for the same reason.
func authRemedy(peer config.PeerConfig, status int) string {
	if peer.Token == "" {
		return fmt.Sprintf("peer requires authentication (HTTP %d) but no token is stored; re-add it with `corral peer add %s <url> --token ...`", status, peer.Name)
	}
	return fmt.Sprintf("peer rejected the stored token (HTTP %d); reissue it on that host and re-add the peer", status)
}

func fetchPeerMeta(peer config.PeerConfig) (*peerMeta, int, error) {
	body, status, err := peerGet(peer, "/api/v1/meta")
	if err != nil || status != http.StatusOK {
		return nil, status, err
	}
	var meta peerMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, status, fmt.Errorf("unreadable /api/v1/meta payload: %w", err)
	}
	return &meta, status, nil
}

// fetchPeerInventory mirrors pkg/web's aggregation: the v1 inventory first,
// the legacy listing when that is absent, so the doctor exercises whichever
// path a real listing would take against this peer.
func fetchPeerInventory(peer config.PeerConfig) ([]types.VM, int, error) {
	body, status, err := peerGet(peer, "/api/v1/inventory")
	if err != nil {
		return nil, status, err
	}
	if status == http.StatusOK {
		var inventory struct {
			VMs []types.VM `json:"vms"`
		}
		if err := json.Unmarshal(body, &inventory); err != nil {
			return nil, status, fmt.Errorf("unreadable inventory payload: %w", err)
		}
		return inventory.VMs, status, nil
	}
	if status != http.StatusNotFound {
		return nil, status, nil
	}
	body, status, err = peerGet(peer, "/api/vms")
	if err != nil || status != http.StatusOK {
		return nil, status, err
	}
	var vms []types.VM
	if err := json.Unmarshal(body, &vms); err != nil {
		return nil, status, fmt.Errorf("unreadable legacy listing payload: %w", err)
	}
	return vms, status, nil
}

// readLimited caps how much of a peer's response the doctor will ingest. A
// misconfigured URL can point at something that streams indefinitely, and a
// diagnostic must not be the thing that exhausts memory.
func readLimited(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func peerGet(peer config.PeerConfig, path string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(peer.URL, "/")+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if peer.Token != "" {
		req.Header.Set("Authorization", "Bearer "+peer.Token)
	}
	resp, err := peerClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp)
	return body, resp.StatusCode, err
}
