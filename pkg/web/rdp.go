package web

import (
	"io"
	"net"
	"net/http"
	"time"

	"github.com/tuna-os/corral/pkg/kubevirt"
	"golang.org/x/net/websocket"
)

// RDP support. Windows VMs run RDP natively; modern Linux desktops expose it
// too (gnome-remote-desktop, xrdp/FreeRDP). Corral detects an open 3389 on
// the VM's pod IP and offers an RDP path for any VM that answers — see
// docs/adr/0002-browser-rdp-via-ironrdp.md for where this is headed
// (in-browser IronRDP client over this bridge).

// rdpDial is the probe dialer — a seam so tests can point it at a local
// listener instead of a pod IP.
var rdpDial = func(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 1500*time.Millisecond)
}

// handleRDPCheck reports whether the VM answers on TCP 3389.
// GET /api/vms/{ns}/{name}/rdp → {"open": bool, "ip": "…"}
func handleRDPCheck(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	info, ok := vmiIndex()[ns+"/"+name]
	if !ok || info.IP == "" {
		jsonResp(w, http.StatusOK, map[string]any{"open": false, "reason": "VM is not running or has no IP"})
		return
	}
	conn, err := rdpDial(net.JoinHostPort(info.IP, "3389"))
	if conn != nil {
		conn.Close()
	}
	jsonResp(w, http.StatusOK, map[string]any{"open": err == nil, "ip": info.IP})
}

// rdpBridge proxies a binary websocket to the VM's RDP console. Two modes:
//   - Raw RDP (default): bridges the TCP stream directly — works with any
//     websocket-capable RDP client or local wsproxy.
//   - RDCleanPath (?rdcleanpath=1): TLS-terminates at the proxy, sends the
//     server cert chain to the browser, then relays the decrypted stream.
//     This is what the IronRDP web component expects for in-browser RDP.
func rdpBridge(ws *websocket.Conn) {
	if ws.Request().URL.Query().Get("rdcleanpath") == "1" {
		rdCleanPathBridge(ws)
		return
	}

	defer ws.Close()
	ns, name := ws.Request().PathValue("ns"), ws.Request().PathValue("name")
	if ns == "" || name == "" {
		return
	}
	if bridgePeerConsole(ws, "rdp") {
		return
	}

	dialer := consoleDialer
	if context := ws.Request().URL.Query().Get("context"); context != "" {
		dialer = kubevirt.RealConsoleDialer{Context: context}
	}
	conn, err := dialer.Dial(ns, name, kubevirt.RDP)
	if err != nil {
		return
	}
	defer conn.Close()

	ws.PayloadType = websocket.BinaryFrame
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, conn); done <- struct{}{} }()
	<-done
}
