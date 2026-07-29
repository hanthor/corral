package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/shell"
)

// RunContexts diagnoses every configured compute context. It intentionally
// runs sequentially: diagnostics should be gentle on remote control planes,
// and the existing KubeVirt repair closures are bound to one runner at a time.
func RunContexts(contexts []config.ContextConfig) []Check {
	runnerMu.Lock()
	defer runnerMu.Unlock()

	base := runner
	var all []Check
	for _, target := range contexts {
		if !target.IsEnabled() {
			continue
		}
		started := time.Now()
		var checks []Check
		switch target.Backend {
		case "qemu":
			runner = base
			checks = localChecks()
		case "kubevirt":
			runner = shell.WithKubeContext(base, target.Context)
			if !ok("kubectl", "get", "--raw", "/livez", "--request-timeout=3s") {
				checks = []Check{{Name: "Cluster reachable", OK: false, Severity: "required", Detail: "Kubernetes API did not answer; verify kubeconfig, context, VPN, and credentials"}}
			} else {
				checks = clusterChecks()
				if _, err := runner.LookPath("virtctl"); err != nil {
					checks = append(checks, Check{Name: "virtctl CLI", Severity: "warning", Detail: "not installed; console and SSH commands are unavailable"})
				}
			}
		case "incus":
			runner = base
			checks = incusChecks(target.Context)
		case "libvirt":
			runner = base
			checks = libvirtChecks(target.Context)
		default:
			checks = []Check{{Name: "Backend supported", Detail: fmt.Sprintf("unknown backend %q", target.Backend), Severity: "required"}}
		}
		latency := time.Since(started).Round(time.Millisecond)
		for i := range checks {
			checks[i].Backend = target.Backend
			checks[i].Context = target.Name
			if checks[i].Severity == "" {
				checks[i].Severity = "required"
			}
			if checks[i].Name == "Tailscale CLI" || checks[i].Name == "Nested virtualization" || checks[i].Name == "KVM acceleration" || checks[i].Name == "virtctl CLI" {
				checks[i].Severity = "warning"
			}
			if i == 0 {
				checks[i].Detail += fmt.Sprintf(" (probe %s)", latency)
			}
		}
		all = append(all, checks...)
	}
	runner = base
	return all
}

func incusChecks(remote string) []Check {
	if remote == "" {
		remote = "local"
	}
	if _, err := runner.LookPath("incus"); err != nil {
		return []Check{{Name: "Incus CLI", Detail: "incus is not installed", Severity: "required"}}
	}
	out, err := runner.Run("incus", "query", remote+":/1.0")
	reachable := err == nil
	checks := []Check{{Name: "Incus remote reachable", OK: reachable, Severity: "required", Detail: detailIf(reachable, "remote API and trust are valid", commandFailure(out, err, "verify `incus remote list` and trust credentials"))}}
	if !reachable {
		return checks
	}
	out, err = runner.Run("incus", "info", remote+":")
	info := strings.ToLower(string(out))
	vm := err == nil && (strings.Contains(info, "virtual-machine") || strings.Contains(info, "qemu") || strings.Contains(info, "instance_types"))
	checks = append(checks,
		Check{Name: "Incus VM support", OK: vm, Severity: "required", Detail: detailIf(vm, "virtual-machine instances supported", "server did not advertise VM/QEMU support")},
		Check{Name: "Incus storage and network", OK: err == nil, Severity: "required", Detail: detailIf(err == nil, "server configuration readable", commandFailure(out, err, "inspect the remote storage pools and managed networks"))},
	)
	return checks
}

func libvirtChecks(uri string) []Check {
	if uri == "" {
		uri = "qemu:///system"
	}
	if _, err := runner.LookPath("virsh"); err != nil {
		return []Check{{Name: "virsh CLI", Detail: "virsh is not installed", Severity: "required"}}
	}
	out, err := runner.Run("virsh", "-c", uri, "connect")
	reachable := err == nil
	checks := []Check{{Name: "libvirt reachable", OK: reachable, Severity: "required", Detail: detailIf(reachable, "connection and permissions are valid", commandFailure(out, err, "verify the URI, SSH transport, libvirt daemon, and group membership"))}}
	if !reachable {
		return checks
	}
	for _, probe := range []struct{ name, command, good, remedy string }{
		{"libvirt hypervisor", "nodeinfo", "host virtualization details available", "verify hypervisor permissions"},
		{"libvirt storage", "pool-list", "storage pools queryable", "define or start a storage pool"},
		{"libvirt networking", "net-list", "virtual networks queryable", "define or start a virtual network"},
	} {
		args := []string{"-c", uri, probe.command}
		if probe.command != "nodeinfo" {
			args = append(args, "--all")
		}
		out, err = runner.Run("virsh", args...)
		checks = append(checks, Check{Name: probe.name, OK: err == nil, Severity: "required", Detail: detailIf(err == nil, probe.good, commandFailure(out, err, probe.remedy))})
	}
	_, viewerErr := runner.LookPath("virt-viewer")
	checks = append(checks, Check{Name: "libvirt console viewer", OK: viewerErr == nil, Severity: "warning", Detail: detailIf(viewerErr == nil, "virt-viewer found", "optional virt-viewer missing; web VNC can still work when the domain advertises VNC")})
	return checks
}

func commandFailure(out []byte, err error, remedy string) string {
	message := strings.TrimSpace(string(out))
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		return remedy
	}
	return message + "; " + remedy
}
