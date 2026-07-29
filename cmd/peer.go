package cmd

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/types"
	"golang.org/x/net/websocket"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

var peerCmd = &cobra.Command{Use: "peer", Short: "Manage remote corral web instances shown in this dashboard"}
var peerToken string
var peerClient = &http.Client{Timeout: 10 * time.Second}

func init() {
	add := &cobra.Command{Use: "add <name> <url>", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		return config.SetPeerWithToken(a[0], a[1], peerToken)
	}}
	add.Flags().StringVar(&peerToken, "token", "", "Scoped service token accepted by the remote Corral")
	peerCmd.AddCommand(
		add,
		&cobra.Command{Use: "remove <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return config.RemovePeer(a[0]) }},
		&cobra.Command{Use: "list", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
			for _, p := range config.Peers() {
				fmt.Printf("%s\t%s\n", p.Name, p.URL)
			}
		}},
	)
	rootCmd.AddCommand(peerCmd)
}

func peerRequest(peer config.PeerConfig, method, target string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, target, body)
	if err == nil && peer.Token != "" {
		req.Header.Set("Authorization", "Bearer "+peer.Token)
	}
	return req, err
}

type peerTarget struct {
	peer config.PeerConfig
	vm   types.VM
}

func resolvePeerTarget(target string) (peerTarget, bool, error) {
	peerName, name, ok := strings.Cut(target, "/")
	if !ok {
		return peerTarget{}, false, nil
	}
	var peer config.PeerConfig
	found := false
	for _, p := range config.Peers() {
		if p.Name == peerName {
			peer = p
			found = true
			break
		}
	}
	if !found {
		return peerTarget{}, true, fmt.Errorf("unknown Corral peer %q", peerName)
	}
	req, err := peerRequest(peer, http.MethodGet, peer.URL+"/api/v1/inventory", nil)
	if err != nil {
		return peerTarget{}, true, err
	}
	resp, err := peerClient.Do(req)
	if err != nil {
		return peerTarget{}, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return peerTarget{}, true, fmt.Errorf("peer %s: HTTP %d", peerName, resp.StatusCode)
	}
	var inventory struct {
		VMs []types.VM `json:"vms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inventory); err != nil {
		return peerTarget{}, true, err
	}
	vms := inventory.VMs
	var matches []types.VM
	for _, vm := range vms {
		if vm.Name == name {
			matches = append(matches, vm)
		}
	}
	if len(matches) == 0 {
		return peerTarget{}, true, fmt.Errorf("%s has no instance %q", peerName, name)
	}
	if len(matches) > 1 {
		return peerTarget{}, true, fmt.Errorf("%s/%s is ambiguous across namespaces", peerName, name)
	}
	return peerTarget{peer: peer, vm: matches[0]}, true, nil
}
func (p peerTarget) action(method, action string) error {
	endpoint := fmt.Sprintf("%s/api/vms/%s/%s", p.peer.URL, url.PathEscape(p.vm.Namespace), url.PathEscape(p.vm.Name))
	if action != "" {
		endpoint += "/" + action
	}
	if p.vm.Context != "" {
		endpoint += "?context=" + url.QueryEscape(p.vm.Context)
	}
	req, err := peerRequest(p.peer, method, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := peerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("peer %s: HTTP %d: %s", p.peer.Name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if method == "GET" {
		_, _ = io.Copy(os.Stdout, resp.Body)
	}
	return nil
}
func (p peerTarget) ssh(user, command string) error {
	sshAddress := p.vm.Endpoints["ssh"]
	if sshAddress == "" {
		sshAddress = p.vm.IP
	}
	if sshAddress != "" {
		host, port, splitErr := net.SplitHostPort(sshAddress)
		if splitErr != nil {
			host = sshAddress
		}
		args := []string{}
		if port != "" {
			args = append(args, "-p", port)
		}
		args = append(args, user+"@"+host)
		if command != "" {
			args = append(args, command)
		}
		cmd := exec.Command("ssh", args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	u, _ := url.Parse(p.peer.URL)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = fmt.Sprintf("/api/tty/%s/%s", url.PathEscape(p.vm.Namespace), url.PathEscape(p.vm.Name))
	if p.vm.Context != "" {
		u.RawQuery = "context=" + url.QueryEscape(p.vm.Context)
	}
	cfg, err := websocket.NewConfig(u.String(), p.peer.URL)
	if err != nil {
		return err
	}
	if p.peer.Token != "" {
		cfg.Header.Set("Authorization", "Bearer "+p.peer.Token)
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return fmt.Errorf("direct SSH and peer TTY fallback failed: %w", err)
	}
	defer ws.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(ws, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, ws); done <- struct{}{} }()
	<-done
	return nil
}
