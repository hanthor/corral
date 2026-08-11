package kubevirt

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// LastPassword holds the cloud-init password from the most recent GenerateVM call.
var LastPassword string

// AlpineImage is the base for the VM port-proxy's init/main containers —
// digest-pinned (see #66) so a supply-chain push to the mutable :latest tag
// can't silently swap what runs with the proxy's in-cluster credentials.
const AlpineImage = "alpine:latest@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"

// mergeCloudInit combines the generated #cloud-config user-data with the
// user-supplied extra YAML. A raw string append produces duplicate top-level
// keys (e.g. two ssh_authorized_keys:) — invalid cloud-config that cloud-init
// mishandles silently. Lists merge by concatenation (generated entries first),
// maps merge per key, scalars are replaced by the extra's value. Extra that
// isn't parseable YAML falls back to the old append.
func mergeCloudInit(base, extra string) string {
	var b, e map[string]any
	if yaml.Unmarshal([]byte(base), &b) != nil || b == nil {
		return base + extra
	}
	if yaml.Unmarshal([]byte(extra), &e) != nil || e == nil {
		return base + extra
	}
	for k, v := range e {
		if bl, ok := b[k].([]any); ok {
			if el, ok := v.([]any); ok {
				b[k] = append(bl, el...)
				continue
			}
		}
		if bm, ok := b[k].(map[string]any); ok {
			if em, ok := v.(map[string]any); ok {
				for mk, mv := range em {
					bm[mk] = mv
				}
				continue
			}
		}
		b[k] = v
	}
	out, err := yaml.Marshal(b)
	if err != nil {
		return base + extra
	}
	return "#cloud-config\n" + string(out)
}

// LoadSSHPublicKey reads the first available SSH public key from ~/.ssh/.
func LoadSSHPublicKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub", "id_ecdsa.pub"} {
		key, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err == nil && len(key) > 0 {
			return strings.TrimSpace(string(key))
		}
	}
	return ""
}

// Client interacts with the Kubernetes cluster via kubectl.
type Client struct {
	Namespace string
	Context   string
	Runner    shell.Runner // injected for tests; defaults to shell.Real
}

func (c *Client) runner() shell.Runner {
	if c.Runner != nil {
		return c.Runner
	}
	if defaultClientRunner != nil {
		return defaultClientRunner
	}
	return shell.WithKubeContext(shell.DefaultKubectl, c.Context)
}

// defaultClientRunner is a package-level runner override for tests.
// Set it to intercept all client calls without modifying each NewClient call site.
var defaultClientRunner shell.Runner
var contextScopeMu sync.Mutex

// SetDefaultRunner overrides the runner for all kubevirt clients (for unit tests).
func SetDefaultRunner(r shell.Runner) { defaultClientRunner = r }

// WithContext scopes package-level create helpers (Apply, image import,
// proxy creation) to one kubeconfig context. Client lifecycle methods already
// carry their own runner; this bridge keeps the older manifest pipeline safe
// until all package-level helpers become methods.
func WithContext(context string, fn func() error) error {
	if context == "" {
		return fn()
	}
	contextScopeMu.Lock()
	defer contextScopeMu.Unlock()
	runnerMu.Lock()
	oldApply, oldPackage := applyRunner, defaultPackageRunner
	bound := shell.WithKubeContext(shell.DefaultKubectl, context)
	applyRunner, defaultPackageRunner = bound, bound
	runnerMu.Unlock()
	defer func() {
		runnerMu.Lock()
		applyRunner, defaultPackageRunner = oldApply, oldPackage
		runnerMu.Unlock()
	}()
	return fn()
}

func CreateVMInContext(opts types.CreateOpts, context string) (password string, err error) {
	err = WithContext(context, func() error {
		if inner := CreateVM(opts); inner != nil {
			return inner
		}
		password = LastPassword
		return nil
	})
	return password, err
}

// DefaultNamespace is the default namespace for KubeVirt VMs. Override with
// CORRAL_NAMESPACE (fallback for existing deployments that predate the rename).
var DefaultNamespace = defaultNamespace()

func defaultNamespace() string {
	if ns := os.Getenv("CORRAL_NAMESPACE"); ns != "" {
		return ns
	}
	return "corral-vms"
}

// Reachable reports whether a KubeVirt-capable cluster is usable right now:
// the kubeconfig points somewhere the API answers AND KubeVirt is installed.
// It's the signal for auto-selecting the backend — KubeVirt when this is true,
// local QEMU otherwise — so one command works in both places. Cheap enough to
// call inline (a single cached kubectl get with a short timeout).
func Reachable() bool {
	_, err := getPackageRunner().Run("kubectl", "get", "kubevirt",
		"-A", "--request-timeout=5s", "-o", "name")
	return err == nil
}

// EnsureNamespace creates the namespace if it doesn't exist and labels it
// for privileged pods (needed by the bootc builder Job).
func EnsureNamespace(ns string) {
	if ns == "" {
		ns = DefaultNamespace
	}
	runPkg("kubectl", "create", "ns", ns) // no-op if it exists
	runPkg("kubectl", "label", "ns", ns,
		"pod-security.kubernetes.io/enforce=privileged", "--overwrite")
}

// NewClient creates a KubeVirt client using the real os/exec runner.
func NewClient(ns string) *Client {
	if ns == "" {
		ns = DefaultNamespace
	}
	return &Client{Namespace: ns}
}

// NewClientForContext binds all client operations to one kubeconfig context.
// It does not mutate kubectl's current-context or process-global environment.
func NewClientForContext(ns, context string) *Client {
	c := NewClient(ns)
	c.Context = context
	return c
}

// NewClientWithRunner creates a KubeVirt client with a custom Runner (for tests).
func NewClientWithRunner(ns string, r shell.Runner) *Client {
	c := NewClient(ns)
	c.Runner = r
	return c
}

// VMExists checks if a VirtualMachine exists in the cluster.
func (c *Client) VMExists(name string) bool {
	_, err := c.runner().Run("kubectl", "get", "vm", name, "-n", c.Namespace, "-o", "name")
	return err == nil
}

// DataVolumeStatus returns the import progress for a VM's ISO DataVolume.
func DataVolumeStatus(name, ns string) string {
	return dataVolumeStatus(getPackageRunner(), name, ns)
}
func dataVolumeStatus(r shell.Runner, name, ns string) string {
	out, err := r.Run("kubectl", "get", "datavolume", name+"-iso", "-n", ns, "-o", "json")
	if err != nil {
		return ""
	}
	var dv struct {
		Status struct {
			Phase    string `json:"phase"`
			Progress string `json:"progress"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &dv) != nil {
		return ""
	}
	switch dv.Status.Phase {
	case "Succeeded":
		return "✓ ready"
	case "ImportInProgress", "ImportScheduled":
		if dv.Status.Progress != "" {
			return "↓ " + dv.Status.Progress
		}
		return "↓ importing"
	case "Pending", "PVCBound", "WaitForFirstConsumer":
		return "↓ queued"
	default:
		return "↓ " + dv.Status.Phase
	}
}

// ListVMs returns all KubeVirt VMs with status information.
func (c *Client) ListVMs() ([]types.VM, error) {
	out, err := c.runner().Run("kubectl", "get", "vms", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	runner := c.runner()
	li := launcherRunningIndexWithRunner(runner)
	launcherRunning := func(name, ns string) bool { return li[ns+"/"+name] }
	isoStatus := func(name, ns string) string { return dataVolumeStatus(runner, name, ns) }
	vms, err := parseVMList(out, vmiStatusIndexWithRunner(runner), nodeVendorsWithRunner(runner), c.proxyStatus, isoStatus, launcherRunning)
	for i := range vms {
		vms[i].Context = c.Context
		vms[i].SetIdentity()
	}
	return vms, err
}

// parseVMList turns `kubectl get vms -o json` output into []types.VM. Pure
// except for the injected helpers (proxy status, ISO import status, launcher
// running), so the state-derivation logic is unit-testable. Keep it free of
// exec/IO.
//
// launcherRunningFn reports whether a VM's virt-launcher pod is Running. It's
// the truth source for kernel-boot (bootc) VMs, whose VMI status — phase
// included — freezes on KubeVirt versions where the kernelBootStatus checksum
// (a uint32) trips the CRD's int32 validation, so printableStatus is stuck.
// See docs note in HANDOFF; pass a func returning false to disable.
func parseVMList(out []byte, vmis map[string]vmiStatus, vendors map[string]string,
	proxyStatusFn, isoStatusFn func(name, ns string) string,
	launcherRunningFn func(name, ns string) bool) ([]types.VM, error) {
	var result struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Running  *bool `json:"running"`
				Template struct {
					Spec struct {
						Domain struct {
							CPU struct {
								Cores   int `json:"cores"`
								Sockets int `json:"sockets"`
								Threads int `json:"threads"`
							} `json:"cpu"`
							Memory   struct{ Guest string } `json:"memory"`
							Firmware struct {
								KernelBoot *json.RawMessage `json:"kernelBoot"`
							} `json:"firmware"`
						} `json:"domain"`
						NodeSelector map[string]string `json:"nodeSelector"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				Ready           bool   `json:"ready"`
				PrintableStatus string `json:"printableStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}

	var vms []types.VM
	for _, vm := range result.Items {
		name := vm.Metadata.Name
		ns := vm.Metadata.Namespace
		// Derive state from KubeVirt's authoritative printableStatus — spec.running
		// is empty on VMs that use spec.runStrategy (the newer field).
		ps := vm.Status.PrintableStatus
		running := ps == "Running" || ps == "Paused" || ps == "Migrating" || ps == "Stopping"
		node := "—"
		if n, ok := vm.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; ok {
			node = n
		}

		// Kernel-boot (bootc) rescue: a frozen VMI status leaves printableStatus
		// stuck at a transitional value even though the guest is up. Trust the
		// launcher pod instead. Only overrides transitional states — never a
		// genuine Stopped (the VM controller cleans up the pod on stop).
		kernelBoot := vm.Spec.Template.Spec.Domain.Firmware.KernelBoot != nil
		transitional := ps == "Starting" || ps == "Scheduling" || ps == "Scheduled" || ps == "Provisioning" || ps == ""
		if !running && kernelBoot && transitional && launcherRunningFn != nil && launcherRunningFn(name, ns) {
			running = true
			ps = "Running"
		}

		status := statusLabel(ps)
		if !running && !vm.Status.Ready {
			if iso := isoStatusFn(name, ns); iso != "" && iso != "✓ ready" {
				status = iso
			}
		}

		cpu := vm.Spec.Template.Spec.Domain.CPU
		v := types.VM{
			Name:       name,
			Backend:    "kubevirt",
			Namespace:  ns,
			Status:     status,
			Ready:      vm.Status.Ready,
			Running:    running,
			CPU:        totalVCPU(cpu.Sockets, cpu.Cores, cpu.Threads),
			Mem:        vm.Spec.Template.Spec.Domain.Memory.Guest,
			Node:       node,
			VNC:        proxyStatusFn(name, ns),
			IsTemplate: vm.Metadata.Labels["corral.dev/template"] == "true",
			Bootc:      kernelBoot,
			Tags:       tagsFromLabels(vm.Metadata.Labels),
			Ephemeral:  vm.Metadata.Labels["corral.dev/ephemeral"] == "true",
			ExpiresAt:  vm.Metadata.Annotations["corral.dev/expires-at"],
			StoppedAt:  vm.Metadata.Annotations["corral.dev/gc-stopped-at"],
		}
		// Overlay live VMI facts (actual node, IP, migratability, agent).
		// LiveMigratable reflects REAL viability: KubeVirt's condition AND a
		// same-CPU-vendor target node (live migration can't cross Intel/AMD).
		if vmi, ok := vmis[ns+"/"+name]; ok {
			if vmi.Node != "" {
				v.Node = vmi.Node
			}
			v.IP = vmi.IP
			v.LiveMigratable = vmi.LiveMigratable && hasMigrationTarget(vmi.Node, vendors)
			v.AgentConnected = vmi.AgentConnected
		}
		vms = append(vms, v)
	}
	return vms, nil
}

// statusLabel maps KubeVirt's printableStatus to a Corral status string.
func statusLabel(ps string) string {
	switch ps {
	case "Running":
		return "● Running"
	case "Paused":
		return "⏸ Paused"
	case "Migrating":
		return "⇄ Migrating"
	case "Starting", "Provisioning", "WaitingForVolumeBinding":
		return "◐ " + ps
	case "Stopping", "Terminating":
		return "◌ " + ps
	case "Stopped", "":
		return "○ Stopped"
	default:
		return "○ " + ps
	}
}

func totalVCPU(sockets, cores, threads int) int {
	if sockets == 0 {
		sockets = 1
	}
	if cores == 0 {
		cores = 1
	}
	if threads == 0 {
		threads = 1
	}
	return sockets * cores * threads
}

type vmiStatus struct {
	Node           string
	IP             string
	LiveMigratable bool
	AgentConnected bool
}

// vmiStatusIndex returns live per-VMI facts keyed by "namespace/name".
// launcherRunningIndex maps "ns/vm" → true when the VM's virt-launcher pod is
// Running with its compute container ready. Used to rescue kernel-boot VMs
// whose VMI status is frozen (see parseVMList). One list call per refresh.
func launcherRunningIndex() map[string]bool {
	return launcherRunningIndexWithRunner(getPackageRunner())
}
func launcherRunningIndexWithRunner(r shell.Runner) map[string]bool {
	out, err := r.Run("kubectl", "get", "pods", "-A",
		"-l", "kubevirt.io=virt-launcher", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Name  string `json:"name"`
					Ready bool   `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	idx := map[string]bool{}
	for _, p := range res.Items {
		vm := p.Metadata.Labels["vm.kubevirt.io/name"]
		if vm == "" || p.Status.Phase != "Running" {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "compute" && cs.Ready {
				idx[p.Metadata.Namespace+"/"+vm] = true
			}
		}
	}
	return idx
}

func vmiStatusIndex() map[string]vmiStatus {
	return vmiStatusIndexWithRunner(getPackageRunner())
}
func vmiStatusIndexWithRunner(r shell.Runner) map[string]vmiStatus {
	out, err := r.Run("kubectl", "get", "vmis", "-A", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				NodeName   string `json:"nodeName"`
				Interfaces []struct {
					IPAddress string `json:"ipAddress"`
				} `json:"interfaces"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	idx := make(map[string]vmiStatus, len(res.Items))
	for _, it := range res.Items {
		s := vmiStatus{Node: it.Status.NodeName}
		if len(it.Status.Interfaces) > 0 {
			s.IP = it.Status.Interfaces[0].IPAddress
		}
		for _, c := range it.Status.Conditions {
			switch c.Type {
			case "LiveMigratable":
				s.LiveMigratable = c.Status == "True"
			case "AgentConnected":
				s.AgentConnected = c.Status == "True"
			}
		}
		idx[it.Metadata.Namespace+"/"+it.Metadata.Name] = s
	}
	return idx
}

func (c *Client) proxyStatus(name, ns string) string {
	out, err := c.runner().Run("kubectl", "get", "deploy", name+"-proxy", "-n", ns,
		"-o", "jsonpath={.status.readyReplicas}")
	if err != nil || len(out) == 0 {
		return "off"
	}
	if strings.TrimSpace(string(out)) != "0" {
		return "on"
	}
	return "pending"
}

// StartVM starts a KubeVirt VirtualMachine.
func (c *Client) StartVM(name string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}
	_, err = c.runner().Run(virtctl, "start", name, "-n", c.Namespace)
	return err
}

// StopVM stops a KubeVirt VirtualMachine.
func (c *Client) StopVM(name string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}
	_, err = c.runner().Run(virtctl, "stop", name, "-n", c.Namespace)
	return err
}

// DeleteVM deletes a KubeVirt VM and its PVCs/DataVolumes/proxy resources.
func (c *Client) DeleteVM(name string) error {
	// Stop first
	virtctl, _ := c.ensureVirtctl()
	if virtctl != "" {
		c.runner().Run(virtctl, "stop", name, "-n", c.Namespace)
	}

	// Delete VM
	c.runner().Run("kubectl", "delete", "vm", name, "-n", c.Namespace, "--ignore-not-found")

	// Delete DataVolumes and PVCs. The DataVolume goes first: while a CDI
	// importer pod still has the PVC mounted, deleting the PVC blocks on the
	// pvc-protection finalizer until that pod exits, so a VM deleted mid-import
	// used to hang this call for as long as the download took. Removing the
	// DataVolume tears the importer down and garbage-collects its PVC.
	// --wait=false keeps the call from blocking on finalizers either way —
	// deletion still completes, we just don't sit on the request while it does.
	for _, suffix := range []string{"disk", "data", "iso", "bootc-disk"} {
		pvc := name + "-" + suffix
		c.runner().Run("kubectl", "delete", "datavolume", pvc, "-n", c.Namespace, "--ignore-not-found", "--wait=false")
		c.runner().Run("kubectl", "delete", "pvc", pvc, "-n", c.Namespace, "--ignore-not-found", "--wait=false")
	}

	// Delete hotplug disks and snapshots labeled for this VM
	c.runner().Run("kubectl", "delete", "pvc", "-n", c.Namespace,
		"-l", "corral.dev/vm="+name, "--ignore-not-found", "--wait=false")
	c.runner().Run("kubectl", "delete", "vmsnapshot", "-n", c.Namespace,
		"-l", "corral.dev/vm="+name, "--ignore-not-found")

	// Delete proxy resources if any
	DeleteProxy(name, c.Namespace)
	return nil
}

// Logs tails the virt-launcher pod logs for a VM.
func (c *Client) Logs(name string) error {
	cmd := shell.CommandForContext(c.Context, "kubectl", "logs", "-n", c.Namespace,
		"-l", "vm.kubevirt.io/name="+name, "-c", "compute", "--tail=100", "-f")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// VMInfo returns JSON info about a VM.
func (c *Client) VMInfo(name string) ([]byte, error) {
	return c.runner().Run("kubectl", "get", "vm", name, "-n", c.Namespace, "-o", "json")
}

// SSH opens an SSH session to a VM via virtctl ssh. localForwards are raw
// ssh -L specs ([bind_address:]port:host:hostport), passed through to
// virtctl's underlying local ssh client via --local-ssh-opts.
func (c *Client) SSH(name, username, identityFile, command string, port int, password string, localForwards []string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}

	args := virtctlSSHArgs(c.Namespace, username, identityFile, command, port, localForwards, name)

	if password != "" {
		return shell.RunWithSSHPass(password, virtctl, args...)
	}

	cmd := shell.CommandForContext(c.Context, virtctl, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// WaitSSH starts name (if not already running), waits for the VMI to reach
// Running, then polls a non-interactive SSH probe until it succeeds or timeout
// elapses. It's the KubeVirt twin of qemu.WaitSSH, so `corral create --bootc
// <img> --wait-ssh` is a backend-agnostic boot gate: nonzero return ⇒ fail the
// pipeline. identityFile "" lets virtctl pick the default key.
func (c *Client) WaitSSH(name, username, identityFile string, timeout time.Duration) error {
	if err := c.StartVM(name); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)

	// Phase 1: wait for the VMI to be Running (scheduled + booting). Until then
	// there's no guest to SSH into, so probing is pointless.
	for time.Now().Before(deadline) {
		out, _ := c.runner().Run("kubectl", "get", "vmi", name, "-n", c.Namespace,
			"-o", "jsonpath={.status.phase}")
		if strings.TrimSpace(string(out)) == "Running" {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Phase 2: poll SSH. `true` is the lightest possible probe; a nonzero exit
	// (connection refused, auth not ready) just means "not yet" until the
	// deadline. StrictHostKeyChecking off so the changing host key never blocks.
	args := []string{"ssh", "--namespace=" + c.Namespace, "--username=" + username,
		"--command=true",
		"--local-ssh-opts=-o StrictHostKeyChecking=no",
		"--local-ssh-opts=-o UserKnownHostsFile=/dev/null",
		"--local-ssh-opts=-o ConnectTimeout=5",
	}
	if identityFile != "" {
		args = append(args, "--identity-file="+identityFile)
	}
	args = append(args, "vm/"+name)

	var lastErr error
	for time.Now().Before(deadline) {
		cmd := shell.CommandForContext(c.Context, virtctl, args...)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			lastErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout after %s waiting for SSH on %q (last probe: %v)", timeout, name, lastErr)
}

// virtctlSSHArgs builds the virtctl ssh argv, factored out for unit testing
// — the exec itself is a real interactive process, not mockable.
func virtctlSSHArgs(namespace, username, identityFile, command string, port int, localForwards []string, name string) []string {
	args := []string{"ssh", "--namespace=" + namespace, "--username=" + username}
	if identityFile != "" {
		args = append(args, "--identity-file="+identityFile)
	}
	if command != "" {
		args = append(args, "--command="+command)
	}
	if port != 22 && port != 0 {
		args = append(args, fmt.Sprintf("--port=%d", port))
	}
	args = append(args,
		"--local-ssh-opts=-o StrictHostKeyChecking=no",
		"--local-ssh-opts=-o UserKnownHostsFile=/dev/null",
	)
	for _, fwd := range localForwards {
		args = append(args, "--local-ssh-opts=-L "+fwd)
	}
	args = append(args, "vm/"+name)
	return args
}

// Viewer launches VNC viewer using virtctl proxy + xdg-open.
func (c *Client) Viewer(name string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}

	xdg, _ := exec.LookPath("xdg-open")
	if xdg != "" {
		// Find free port
		port := c.findFreePort()
		proxy := shell.CommandForContext(c.Context, virtctl, "vnc", name, "-n", c.Namespace, "--proxy-only", fmt.Sprintf("--port=%d", port))
		proxy.Stdout = os.Stderr
		proxy.Stderr = os.Stderr
		if err := proxy.Start(); err != nil {
			return err
		}
		// Launch xdg-open
		exec.Command(xdg, fmt.Sprintf("vnc://localhost:%d", port)).Start()
		fmt.Fprintf(os.Stderr, "VNC: vnc://localhost:%d (proxy PID: %d)\n", port, proxy.Process.Pid)
		return proxy.Wait()
	}

	// Fallback: virtctl vnc directly
	cmd := shell.CommandForContext(c.Context, virtctl, "vnc", name, "-n", c.Namespace)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *Client) findFreePort() int {
	for p := 5901; p < 5910; p++ {
		// Simple check — in production use net.Dial
		if !c.portInUse(p) {
			return p
		}
	}
	return 5901
}

func (c *Client) portInUse(port int) bool {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("ss -tln | grep -q ':%d '", port))
	return cmd.Run() == nil
}

// Virtctl resolves the virtctl binary path (exported for plugins that shell to
// virtctl directly, e.g. corral-backup's CDI image-upload restore).
func (c *Client) Virtctl() (string, error) { return c.ensureVirtctl() }

func (c *Client) ensureVirtctl() (string, error) {
	path, err := c.runner().LookPath("virtctl")
	if err != nil {
		return "", fmt.Errorf("virtctl not found — install: brew install virtctl")
	}
	return path, nil
}

// Apply marshals a manifest object and pipes it to kubectl apply.
func Apply(obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return applyManifest(string(data))
}

// CreateVM provisions the namespace, disks, and VirtualMachine described by opts.
// Used by both the CLI create command and the web UI.
func CreateVM(opts types.CreateOpts) error {
	ns := opts.Namespace
	if ns == "" {
		ns = DefaultNamespace
		opts.Namespace = ns
	}
	EnsureNamespace(ns)

	name := opts.Name
	hasISO := opts.ISO != ""
	hasContainer := opts.ContainerDisk != ""
	hasImport := opts.ImportURL != ""
	hasPVC := opts.PVC != ""
	diskSize := opts.Disk
	if diskSize == "" {
		diskSize = "20G"
	}
	sc := opts.StorageClass
	if sc == "" {
		sc = PreferredStorageClass()
	}

	if hasISO {
		if err := Apply(GenerateDataVolume(name+"-iso", ns, opts.ISO, DetectISOSize(opts.ISO))); err != nil {
			return fmt.Errorf("creating ISO DataVolume: %w", err)
		}
		fmt.Fprintf(os.Stderr, "ISO DataVolume: %s-iso (importing from %s)\n", name, opts.ISO)
		if err := Apply(GeneratePVCWithClass(name+"-disk", ns, diskSize, sc)); err != nil {
			return fmt.Errorf("creating boot PVC: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Boot PVC: %s-disk (%s)\n", name, diskSize)
	} else if hasImport {
		// CDI imports the qcow2/raw image straight into the boot disk PVC.
		if err := Apply(GenerateBootDataVolume(name+"-disk", ns, opts.ImportURL, diskSize, sc)); err != nil {
			return fmt.Errorf("creating import DataVolume: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Importing %s → %s-disk (%s)\n", opts.ImportURL, name, diskSize)
	} else if hasContainer && opts.Disk != "" {
		if err := Apply(GeneratePVCWithClass(name+"-data", ns, opts.Disk, sc)); err != nil {
			return fmt.Errorf("creating data PVC: %w", err)
		}
	} else if !hasPVC && !hasContainer {
		if err := Apply(GeneratePVCWithClass(name+"-disk", ns, diskSize, sc)); err != nil {
			return fmt.Errorf("creating boot PVC: %w", err)
		}
	}

	vm := GenerateVM(opts)

	// KubeVirt limits inline cloud-init userData to 2048 bytes.  When
	// provisioning scripts (from Lima --file) exceed that, store the
	// userData in a Secret and reference it via userDataSecretRef.
	if userData, ok := extractUserData(volumesFromVM(vm)); ok && len(userData) > 2048 {
		secretName := name + "-cloudinit"
		if err := createCloudInitSecret(secretName, ns, userData); err != nil {
			return fmt.Errorf("creating cloud-init Secret: %w", err)
		}
		replaceUserDataWithSecret(volumesFromVM(vm), secretName)
	}

	if err := Apply(vm); err != nil {
		return fmt.Errorf("creating VM: %w", err)
	}
	return nil
}

// volumesFromVM extracts the volumes slice from a GenerateVM manifest.
func volumesFromVM(vm map[string]any) []map[string]any {
	spec, _ := vm["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	tmplSpec, _ := tmpl["spec"].(map[string]any)
	vols, _ := tmplSpec["volumes"].([]map[string]any)
	return vols
}

// extractUserData returns the inline userData string from the cloudInitNoCloud
// volume, if present.
func extractUserData(volumes []map[string]any) (string, bool) {
	for _, v := range volumes {
		ci, ok := v["cloudInitNoCloud"].(map[string]any)
		if !ok {
			continue
		}
		if ud, ok := ci["userData"].(string); ok && ud != "" {
			return ud, true
		}
	}
	return "", false
}

// replaceUserDataWithSecret swaps cloudInitNoCloud.userData for
// secretRef to avoid KubeVirt's 2048-byte inline limit.
func replaceUserDataWithSecret(volumes []map[string]any, secretName string) {
	for _, v := range volumes {
		ci, ok := v["cloudInitNoCloud"].(map[string]any)
		if !ok {
			continue
		}
		delete(ci, "userData")
		ci["secretRef"] = map[string]any{"name": secretName}
		return
	}
}

// createCloudInitSecret creates a Secret containing the cloud-init userData.
func createCloudInitSecret(name, ns, userData string) error {
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"data": map[string]any{
			"userData": base64.StdEncoding.EncodeToString([]byte(userData)),
		},
	}
	return Apply(secret)
}

// applyManifest pipes a YAML manifest string to kubectl apply.
func applyManifest(yaml string) error {
	out, err := getApplyRunner().RunStdin(yaml, "kubectl", "apply", "-f", "-")
	if err != nil {
		// RunStdin returns combined stdout+stderr; surface it so callers don't
		// just see "exit status 1".
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

// The package shell-out runners are swapped by tests (SetApplyRunner /
// SetPackageRunner) while background goroutines (e.g. async ApplyProxy from a
// create handler) read them, so guard access with a mutex — otherwise `go test
// -race` flags the read/write.
var (
	runnerMu             sync.RWMutex
	applyRunner          shell.Runner = shell.DefaultKubectl // runner used by Apply
	defaultPackageRunner shell.Runner = shell.DefaultKubectl // runner for package-level kubectl
)

func getApplyRunner() shell.Runner { runnerMu.RLock(); defer runnerMu.RUnlock(); return applyRunner }
func getPackageRunner() shell.Runner {
	runnerMu.RLock()
	defer runnerMu.RUnlock()
	return defaultPackageRunner
}

// SetApplyRunner overrides the runner for Apply (for unit tests).
func SetApplyRunner(r shell.Runner) { runnerMu.Lock(); defer runnerMu.Unlock(); applyRunner = r }

// SetPackageRunner overrides the runner for package-level kubectl calls (for unit tests).
func SetPackageRunner(r shell.Runner) {
	runnerMu.Lock()
	defer runnerMu.Unlock()
	defaultPackageRunner = r
}

// runPkg is a helper for package-level functions to use the testable runner.
func runPkg(name string, args ...string) ([]byte, error) {
	return getPackageRunner().Run(name, args...)
}

// cpuSpec builds a hotplug-ready CPU topology: one core/thread per socket so
// that live CPU updates (which scale `sockets`) map 1:1 to vCPUs, with
// maxSockets headroom for hotplug. Uses host-passthrough CPU model so that
// nested KVM (SVM/VMX) is available inside the guest for e2e testing.
func cpuSpec(cpu int) map[string]any {
	if cpu < 1 {
		cpu = 1
	}
	max := cpu * 4
	if max < 4 {
		max = 4
	}
	return map[string]any{
		"sockets":    cpu,
		"cores":      1,
		"threads":    1,
		"maxSockets": max,
		"model":      "host-passthrough",
	}
}

// memSpec sets guest memory plus maxGuest headroom so memory can be hotplugged
// live (up to 4× the initial size). Harmless without LiveUpdate enabled.
func memSpec(memMib int) map[string]any {
	if memMib < 1 {
		memMib = 1
	}
	return map[string]any{
		"guest":    fmt.Sprintf("%dMi", memMib),
		"maxGuest": fmt.Sprintf("%dMi", memMib*4),
	}
}

func parseMem(s string) int {
	upper := strings.ToUpper(s)
	var val int
	if strings.HasSuffix(upper, "G") {
		fmt.Sscanf(s, "%d", &val)
		return val * 1024
	}
	if strings.HasSuffix(upper, "M") {
		fmt.Sscanf(s, "%d", &val)
		return val
	}
	// Raw number → treat as MiB
	fmt.Sscanf(s, "%d", &val)
	return val
}

func randomPassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}
