package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
	"golang.org/x/net/websocket"
)

var peerHTTPClient = &http.Client{Timeout: 5 * time.Second}
var peerInventory sync.Map // encoded namespace/name -> types.VM

func newPeerRequest(ctx context.Context, peer config.PeerConfig, method, target string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err == nil && peer.Token != "" {
		req.Header.Set("Authorization", "Bearer "+peer.Token)
	}
	return req, err
}

func peerVMs() []types.VM {
	var all []types.VM
	for _, peer := range config.Peers() {
		// Clear stale inventory for this peer before publishing the latest view.
		peerInventory.Range(func(key, value any) bool {
			if vm, ok := value.(types.VM); ok && vm.Peer == peer.Name {
				peerInventory.Delete(key)
			}
			return true
		})
		req, err := newPeerRequest(context.Background(), peer, http.MethodGet, peer.URL+"/api/v1/inventory", nil)
		if err != nil {
			continue
		}
		resp, err := peerHTTPClient.Do(req)
		if err != nil {
			continue
		}
		var inventory struct {
			VMs []types.VM `json:"vms"`
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			req, err = newPeerRequest(context.Background(), peer, http.MethodGet, peer.URL+"/api/vms", nil)
			if err != nil {
				continue
			}
			resp, err = peerHTTPClient.Do(req)
			if err != nil {
				continue
			}
			err = json.NewDecoder(resp.Body).Decode(&inventory.VMs)
		} else {
			err = json.NewDecoder(resp.Body).Decode(&inventory)
		}
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		vms := inventory.VMs
		for i := range vms {
			vms[i].Peer = peer.Name
			if vms[i].Context == "" {
				vms[i].Context = peer.Name
			}
			payload := peer.Name + "\x00" + vms[i].Namespace
			vms[i].Namespace = "peer-" + base64.RawURLEncoding.EncodeToString([]byte(payload))
			peerInventory.Store(vms[i].Namespace+"/"+vms[i].Name, vms[i])
		}
		all = append(all, vms...)
	}
	return all
}

// bridgePeerConsole uses the least-indirect route: advertised guest IP first
// for protocols with a real TCP port, then the remote Corral websocket.
func bridgePeerConsole(ws *websocket.Conn, kind string) bool {
	ns, name := ws.Request().PathValue("ns"), ws.Request().PathValue("name")
	peer, remoteNS, ok := parsePeerNamespace(ns)
	if !ok {
		return false
	}
	if value, found := peerInventory.Load(ns + "/" + name); found {
		vm := value.(types.VM)
		port := ""
		if kind == "vnc" {
			port = "5900"
		} else if kind == "rdp" {
			port = "3389"
		}
		address := vm.Endpoints[kind]
		if address == "" && vm.IP != "" && port != "" {
			address = net.JoinHostPort(vm.IP, port)
		}
		if address != "" {
			if conn, err := net.DialTimeout("tcp", address, 1500*time.Millisecond); err == nil {
				defer conn.Close()
				copyWebsocket(ws, conn)
				return true
			}
		}
	}
	u, err := url.Parse(peer.URL)
	if err != nil {
		return true
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = fmt.Sprintf("/api/%s/%s/%s", kind, url.PathEscape(remoteNS), url.PathEscape(name))
	cfg, err := websocket.NewConfig(u.String(), peer.URL)
	if err != nil {
		return true
	}
	if peer.Token != "" {
		cfg.Header.Set("Authorization", "Bearer "+peer.Token)
	}
	upstream, err := websocket.DialConfig(cfg)
	if err != nil {
		return true
	}
	defer upstream.Close()
	copyWebsocketPair(ws, upstream)
	return true
}

func copyWebsocket(ws *websocket.Conn, conn io.ReadWriteCloser) {
	ws.PayloadType = websocket.BinaryFrame
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, conn); done <- struct{}{} }()
	<-done
}
func copyWebsocketPair(a, b *websocket.Conn) {
	a.PayloadType = websocket.BinaryFrame
	b.PayloadType = websocket.BinaryFrame
	done := make(chan struct{}, 2)
	go func() { io.Copy(b, a); done <- struct{}{} }()
	go func() { io.Copy(a, b); done <- struct{}{} }()
	<-done
}

func parsePeerNamespace(ns string) (config.PeerConfig, string, bool) {
	if !strings.HasPrefix(ns, "peer-") {
		return config.PeerConfig{}, "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ns, "peer-"))
	if err != nil {
		return config.PeerConfig{}, "", false
	}
	parts := strings.SplitN(string(b), "\x00", 2)
	if len(parts) != 2 {
		return config.PeerConfig{}, "", false
	}
	for _, p := range config.Peers() {
		if p.Name == parts[0] {
			return p, parts[1], true
		}
	}
	return config.PeerConfig{}, "", false
}

func proxyPeerVM(w http.ResponseWriter, r *http.Request, peer config.PeerConfig, remoteNS string) {
	encodedPrefix := "/api/vms/" + url.PathEscape(r.PathValue("ns")) + "/" + url.PathEscape(r.PathValue("name"))
	remotePrefix := "/api/vms/" + url.PathEscape(remoteNS) + "/" + url.PathEscape(r.PathValue("name"))
	path := strings.Replace(r.URL.EscapedPath(), encodedPrefix, remotePrefix, 1)
	target := peer.URL + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := newPeerRequest(r.Context(), peer, r.Method, target, r.Body)
	if err != nil {
		errResp(w, 500, err)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	resp, err := peerHTTPClient.Do(req)
	if err != nil {
		errResp(w, 502, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// peerGate makes every VM subresource federatable, not just lifecycle and
// info. It runs before the route table so newly-added capability endpoints do
// not accidentally fall through to the local Kubernetes context.
func peerGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/vms/peer-") {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/vms/"), "/")
		if len(parts) < 2 {
			next.ServeHTTP(w, r)
			return
		}
		r.SetPathValue("ns", parts[0])
		r.SetPathValue("name", parts[1])
		peer, remoteNS, ok := parsePeerNamespace(parts[0])
		if !ok {
			errResp(w, http.StatusBadRequest, fmt.Errorf("invalid Corral peer target"))
			return
		}
		proxyPeerVM(w, r, peer, remoteNS)
	})
}
