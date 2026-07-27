package web

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"

	"golang.org/x/net/websocket"

	"github.com/tuna-os/corral/pkg/kubevirt"
)

// rdCleanPathBridge proxies a websocket to a VM's RDP console using
// Devolutions' RDCleanPath protocol — the proxy handles TLS termination,
// sends the server cert chain to the browser, then relays the decrypted
// RDP stream. This lets IronRDP's web component render RDP in the browser
// without the browser needing raw TCP access or handling TLS itself.
//
// See docs/adr/0002-browser-rdp-via-ironrdp.md for the full design.

// rdpTLSBridge replaces the raw rdpBridge when IronRDP is vendored.
// It's registered at the same route; the browser client indicates
// RDCleanPath by including a query parameter or the IronRDP web component
// handles the handshake transparently.
func rdCleanPathBridge(ws *websocket.Conn) {
	defer ws.Close()
	ns, name := ws.Request().PathValue("ns"), ws.Request().PathValue("name")
	if ns == "" || name == "" {
		return
	}

	// Dial the VM's RDP port via the console dialer (virtctl port-forward).
	conn, err := consoleDialer.Dial(ns, name, kubevirt.RDP)
	if err != nil {
		return
	}
	defer conn.Close()

	// Perform TLS handshake with the RDP server. RDP servers use self-signed
	// certs — skip verification and capture the chain for the browser.
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	// Send RDCleanPath PDU: 4-byte big-endian length + DER-encoded cert chain.
	certs := tlsConn.ConnectionState().PeerCertificates
	pdu := encodeRdCleanPath(certs)
	ws.PayloadType = websocket.BinaryFrame
	if _, err := ws.Write(pdu); err != nil {
		return
	}

	// Relay: browser ↔ decrypted RDP stream.
	done := make(chan struct{}, 2)
	go func() { io.Copy(tlsConn, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, tlsConn); done <- struct{}{} }()
	<-done
}

// encodeRdCleanPath builds the RDCleanPath certificate chain PDU:
// [4-byte BE total-length] [4-byte BE cert1-len] [cert1-DER] [4-byte BE cert2-len] [cert2-DER] …
// total-length covers everything after itself (all cert entries).
func encodeRdCleanPath(certs []*x509.Certificate) []byte {
	// Calculate payload size.
	payloadSize := 0
	for _, c := range certs {
		payloadSize += 4 + len(c.Raw) // length prefix + DER
	}

	buf := make([]byte, 4+payloadSize)
	binary.BigEndian.PutUint32(buf[0:4], uint32(payloadSize))
	off := 4
	for _, c := range certs {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(len(c.Raw)))
		off += 4
		copy(buf[off:], c.Raw)
		off += len(c.Raw)
	}
	return buf
}

// rdCleanPathDialer wraps the ConsoleDialer so the RDCleanPath bridge can
// get a raw net.Conn (the TLS layer is added by rdCleanPathBridge itself,
// not by the dialer).
type rdCleanPathDialer struct {
	inner kubevirt.ConsoleDialer
	ns    string
	name  string
}

func (d *rdCleanPathDialer) dial() (net.Conn, error) {
	return d.inner.Dial(d.ns, d.name, kubevirt.RDP)
}
