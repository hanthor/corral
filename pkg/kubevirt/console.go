package kubevirt

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
)

// Console identifies which console protocol to bridge. See ADR-0002 for how
// this fits into the browser-console story (VNC today, RDP today with
// IronRDP planned for phase 2).
type Console int

const (
	VNC Console = iota
	RDP
)

// ConsoleDialer opens a connection to a VM's console. The real adapter shells
// out to virtctl and dials the local port it opens; tests substitute a fake
// that returns an in-memory connection with no subprocess involved.
type ConsoleDialer interface {
	// Dial spawns whatever's needed to reach the VM's console and returns a
	// connection to it. Closing the returned net.Conn also tears down
	// anything Dial started (e.g. the virtctl subprocess).
	Dial(ns, name string, console Console) (net.Conn, error)
}

// RealConsoleDialer is the production ConsoleDialer: it bridges through
// `virtctl vnc --proxy-only` (VNC) or `virtctl port-forward` (RDP).
type RealConsoleDialer struct{ Context string }

func (d RealConsoleDialer) Dial(ns, name string, console Console) (net.Conn, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}

	var cmd *exec.Cmd
	switch console {
	case VNC:
		cmd = shell.CommandForContext(d.Context, "virtctl", "vnc", name, "-n", ns,
			"--proxy-only", "--port", strconv.Itoa(port))
	case RDP:
		cmd = shell.CommandForContext(d.Context, "virtctl", "port-forward", "vm/"+name,
			fmt.Sprintf("%d:3389", port), "-n", ns)
	default:
		return nil, fmt.Errorf("unknown console kind %d", console)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var conn net.Conn
	for i := 0; i < 50; i++ {
		conn, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if conn == nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("console proxy for %s/%s did not open port %d in time", ns, name, port)
	}
	return &consoleConn{Conn: conn, proxy: cmd}, nil
}

// consoleConn wraps the dialed connection so Close() also kills the virtctl
// subprocess backing it — callers just defer Close() once.
type consoleConn struct {
	net.Conn
	proxy *exec.Cmd
}

func (c *consoleConn) Close() error {
	err := c.Conn.Close()
	if c.proxy.Process != nil {
		c.proxy.Process.Kill()
	}
	c.proxy.Wait()
	return err
}

// freePort finds an unused local TCP port for the console proxy to bind to.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
