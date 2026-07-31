package proxmoxbe

// Context configuration.
//
// A Proxmox context is a cluster endpoint plus the credentials to reach it. The
// token is a secret, so it is read from the environment first: a homelab config
// file is often in a dotfiles repo, and a token that ends up committed is a
// cluster somebody else can drive.

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tuna-os/corral/pkg/config"
)

// TokenEnv holds an API token for every Proxmox context, and overrides the
// config file. CORRAL_PROXMOX_TOKEN_<CONTEXT> (uppercased, non-alphanumerics as
// underscores) overrides it per context, for an operator with two clusters.
const TokenEnv = "CORRAL_PROXMOX_TOKEN"

var (
	clientsMu sync.Mutex
	clients   = map[string]*Client{}
)

// ClientForContext resolves the client for a configured context. Clients are
// cached because building one parses TLS trust, and a fleet listing resolves the
// same context once per operation otherwise.
func ClientForContext(context string) (*Client, error) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if client, ok := clients[context]; ok {
		return client, nil
	}
	cfg, err := ConfigForContext(context)
	if err != nil {
		return nil, err
	}
	client, err := New(cfg)
	if err != nil {
		return nil, err
	}
	clients[context] = client
	return client, nil
}

// SetClientForContext installs a client for a context, bypassing configuration.
// Tests use it to point a context at an httptest server.
func SetClientForContext(context string, client *Client) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clients[context] = client
}

// ResetClients drops the cache, so a configuration change takes effect without a
// restart (and so tests do not leak a client into one another).
func ResetClients() {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clients = map[string]*Client{}
}

// ConfigForContext builds a Config from the named context. The context's own
// Context field is the host, matching how every other backend uses it — a
// kubeconfig context, an Incus remote, a libvirt URI, a PVE endpoint.
func ConfigForContext(context string) (Config, error) {
	for _, target := range config.Contexts() {
		if target.Backend != "proxmox" {
			continue
		}
		if target.Context != context && target.Name != context {
			continue
		}
		cfg := Config{Host: target.Context}
		if target.Proxmox != nil {
			cfg.Token = target.Proxmox.Token
			cfg.Fingerprint = target.Proxmox.Fingerprint
			cfg.Insecure = target.Proxmox.Insecure
			cfg.Node = target.Proxmox.Node
		}
		if token := tokenFromEnv(target.Name); token != "" {
			cfg.Token = token
		}
		if cfg.Token == "" {
			return Config{}, fmt.Errorf("proxmox: context %q has no API token; set %s or add "+
				"proxmox.token to the context in the config file", target.Name, TokenEnv)
		}
		return cfg, nil
	}
	return Config{}, fmt.Errorf("proxmox: no configured context matches %q", context)
}

func tokenFromEnv(contextName string) string {
	if token := os.Getenv(TokenEnv + "_" + envSuffix(contextName)); token != "" {
		return token
	}
	return os.Getenv(TokenEnv)
}

func envSuffix(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
