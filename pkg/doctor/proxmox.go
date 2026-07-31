package doctor

// Proxmox context checks (ADR-0009).
//
// A PVE context has three distinct ways to be wrong, and each has a different
// remedy: the cluster is unreachable, the token is rejected, or the certificate
// is untrusted. Reporting one message for all three is what makes a homelab
// operator start disabling TLS verification out of frustration, so they are
// reported separately.

import (
	"fmt"
	"strings"

	"github.com/tuna-os/corral/pkg/proxmoxbe"
)

func proxmoxChecks(contextName, host string) []Check {
	cfg, err := proxmoxbe.ConfigForContext(contextName)
	if err != nil {
		return []Check{{
			Name:     "API token",
			Detail:   err.Error(),
			Severity: "required",
			Fixable:  false,
		}}
	}

	client, err := proxmoxbe.New(cfg)
	if err != nil {
		return []Check{{Name: "Endpoint configuration", Detail: err.Error(), Severity: "required"}}
	}

	version, err := client.Version()
	if err == nil {
		checks := []Check{{Name: "Cluster reachable", OK: true, Detail: "Proxmox VE " + version}}
		if cfg.Insecure {
			checks = append(checks, Check{
				Name:     "TLS verification",
				Severity: "warning",
				Detail:   "disabled for this context; pin the certificate with proxmox.fingerprint instead",
			})
		} else {
			trust := "system trust store"
			if cfg.Fingerprint != "" {
				trust = "pinned certificate"
			}
			checks = append(checks, Check{Name: "TLS verification", OK: true, Detail: trust})
		}
		checks = append(checks, nodeChecks(client)...)
		return checks
	}

	// Separate the three failures, because the remedies have nothing in common.
	detail := err.Error()
	switch {
	case isUnauthorized(err):
		return []Check{{
			Name:     "API token accepted",
			Severity: "required",
			Detail: "the cluster rejected the token; check it has not been revoked and that its " +
				"role grants VM.Audit and VM.PowerMgmt (pveum acl modify / --privsep 0)",
		}}
	case strings.Contains(detail, "certificate"):
		return []Check{{
			Name:     "TLS trust",
			Severity: "required",
			Detail: "the server certificate is not trusted; pin it with proxmox.fingerprint " +
				"(openssl s_client -connect " + host + " | openssl x509 -fingerprint -sha256 -noout)",
		}}
	default:
		return []Check{{
			Name:     "Cluster reachable",
			Severity: "required",
			Detail:   fmt.Sprintf("%s did not answer: %s", host, detail),
		}}
	}
}

// nodeChecks reports the cluster's own health, which is what decides whether a
// migration has anywhere to go.
func nodeChecks(client *proxmoxbe.Client) []Check {
	nodes, err := client.Nodes()
	if err != nil {
		return []Check{{Name: "Nodes", Severity: "warning", Detail: "could not list nodes: " + err.Error()}}
	}
	online, offline := 0, []string{}
	for _, n := range nodes {
		if n.Ready() {
			online++
			continue
		}
		offline = append(offline, n.Node)
	}
	checks := []Check{{
		Name:   "Nodes online",
		OK:     online > 0,
		Detail: fmt.Sprintf("%d of %d online", online, len(nodes)),
	}}
	if len(offline) > 0 {
		checks = append(checks, Check{
			Name:     "Node availability",
			Severity: "warning",
			Detail:   "offline: " + strings.Join(offline, ", "),
		})
	}
	if online < 2 {
		checks = append(checks, Check{
			Name:     "Migration target",
			Severity: "warning",
			Detail:   "a single online node has nowhere to migrate to",
		})
	}
	return checks
}

func isUnauthorized(err error) bool {
	for err != nil {
		if apiErr, ok := err.(*proxmoxbe.APIError); ok {
			return apiErr.Unauthorized()
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
