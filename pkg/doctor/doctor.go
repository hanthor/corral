// Package doctor inspects a cluster for the pieces Corral's features need
// (KubeVirt, CDI, the right feature gates + LiveUpdate, snapshot/expand storage,
// the export proxy, metrics) and can reconcile the safe configuration fixes.
// Surfaced via `corral doctor`, the TUI, and the web UI.
package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tuna-os/corral/pkg/shell"
)

// Check is one diagnostic result.
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Backend  string `json:"backend,omitempty"`
	Context  string `json:"context,omitempty"`
	Severity string `json:"severity,omitempty"` // required, warning, or info
	Fixable  bool   `json:"fixable"`            // Corral can reconcile it (config-only, safe)
	fix      func() error
}

var runner shell.Runner = shell.DefaultKubectl
var runnerMu sync.Mutex

// SetRunner overrides the command runner (for unit tests).
func SetRunner(r shell.Runner) {
	runnerMu.Lock()
	defer runnerMu.Unlock()
	runner = r
}

func run(name string, args ...string) ([]byte, error) {
	return runner.Run(name, args...)
}

func ok(name string, args ...string) bool {
	_, err := runner.Run(name, args...)
	return err == nil
}

// okList is for label-selector list queries, where kubectl exits 0 even with
// zero matches — found on a real cluster where "CDI installed" passed with no
// CDI anywhere. Requires an actual resource name (`-o name` output contains
// "/") rather than just a clean exit.
func okList(args ...string) bool {
	out, err := runner.Run("kubectl", append(args, "-o", "name")...)
	return err == nil && strings.Contains(string(out), "/")
}

// Run executes all checks. A machine with no reachable cluster is a valid,
// healthy Corral setup (local QEMU backend) — in that case the dozen KubeVirt
// checks collapse into one explanatory row instead of a wall of failures,
// and the local checks still run.
func Run() []Check {
	runnerMu.Lock()
	defer runnerMu.Unlock()
	return runUnlocked()
}

func runUnlocked() []Check {
	local := localChecks()
	if !ok("kubectl", "get", "--raw", "/livez", "--request-timeout=3s") {
		return append([]Check{{
			Name: "Cluster reachable",
			OK:   false,
			Detail: "no Kubernetes cluster answered (set KUBECONFIG or ~/.kube/config) — " +
				"cluster checks skipped; the local QEMU backend works without one",
		}}, local...)
	}
	return append(clusterChecks(), local...)
}

// localChecks covers the QEMU backend and client-side tooling — everything
// that matters on this machine regardless of any cluster.
func localChecks() []Check {
	var checks []Check

	_, qemuErr := runner.LookPath("qemu-system-x86_64")
	checks = append(checks, Check{
		Name:   "QEMU (local backend)",
		OK:     qemuErr == nil,
		Detail: detailIf(qemuErr == nil, "qemu-system-x86_64 found", "not installed — needed only for local VMs (dnf/apt install qemu-system-x86)"),
	})

	kvm := statDevKVM()
	checks = append(checks, Check{
		Name:   "KVM acceleration",
		OK:     kvm == nil,
		Detail: detailIf(kvm == nil, "/dev/kvm accessible", "no /dev/kvm access — local VMs fall back to slow emulation (add your user to the kvm group, or enable virtualization in BIOS)"),
	})

	// Nested virtualization (#68): only meaningful where KVM works at all,
	// and only a warning — it matters when this host's VMs will themselves
	// run VMs (e.g. a laptop hosting a KubeVirt dev cluster).
	if kvm == nil {
		nested, known := nestedVirtEnabled()
		if known {
			checks = append(checks, Check{
				Name:   "Nested virtualization",
				OK:     nested,
				Detail: detailIf(nested, "kvm module has nested=1 — VMs can host VMs", "disabled — enable with: modprobe -r kvm_intel && modprobe kvm_intel nested=1 (or kvm_amd); only needed if VMs will run VMs"),
			})
		}
	}

	_, tsErr := runner.LookPath("tailscale")
	checks = append(checks, Check{
		Name:   "Tailscale CLI",
		OK:     tsErr == nil,
		Detail: detailIf(tsErr == nil, "found — VMs can join the tailnet", "not installed (optional) — VMs won't auto-join the tailnet"),
	})

	_, vcErr := runner.LookPath("virtctl")
	checks = append(checks, Check{
		Name:   "virtctl CLI",
		OK:     vcErr == nil,
		Detail: detailIf(vcErr == nil, "found — needed for KubeVirt consoles/SSH", "not installed — needed only for the KubeVirt backend (brew install virtctl)"),
	})

	return checks
}

// nestedVirtEnabled reads the kvm_intel/kvm_amd module's nested parameter.
// known=false when neither module directory exists (no KVM, or exotic arch).
// A seam for tests.
var nestedVirtEnabled = func() (nested, known bool) {
	for _, mod := range []string{"kvm_intel", "kvm_amd"} {
		b, err := os.ReadFile("/sys/module/" + mod + "/parameters/nested")
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		return v == "1" || v == "Y" || v == "y", true
	}
	return false, false
}

// statDevKVM is a seam for tests; returns nil when /dev/kvm is usable.
var statDevKVM = func() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	f.Close()
	return nil
}

// clusterChecks runs the KubeVirt-backend diagnostics (requires a cluster).
func clusterChecks() []Check {
	var checks []Check

	// The CR, cluster-wide, via okList — not `-n kubevirt`, and not a plain
	// exit-code check.
	//
	// Both details are load-bearing. The Hyperconverged Cluster Operator
	// installs into kubevirt-hyperconverged, so a namespace-scoped lookup finds
	// nothing on a perfectly good cluster; and `kubectl get` exits 0 with "No
	// resources found" when the namespace is empty, so a plain ok() reported
	// KubeVirt as *installed* on exactly that cluster — a false pass, which is
	// the failure okList exists to prevent (#168).
	kvInstalled := okList("get", "kubevirt", "-A")
	checks = append(checks, Check{
		Name:    "KubeVirt installed",
		OK:      kvInstalled,
		Detail:  detailIf(kvInstalled, "operator + CR present", "missing — fix installs the KubeVirt "+KubeVirtVersion+" operator + CR"),
		Fixable: !kvInstalled,
		fix:     installKubeVirt,
	})
	// CDI, by either of two signals, because one is not enough in practice.
	//
	// The CR is the real answer: it exists once CDI is installed and reconciled,
	// wherever that happened. The operator-deployment label is kept as a
	// fallback for a CDI whose CR has not appeared yet — but it cannot be the
	// only signal, because the upstream label is absent when HCO owns the
	// install and Corral then offers to install a *second* CDI alongside the
	// operator's (#168). Duplicating a cluster-wide operator is much worse than
	// a missed check, so this errs toward finding one.
	cdiInstalled := okList("get", "cdi", "-A") ||
		okList("get", "deploy", "-A", "-l", "cdi.kubevirt.io=cdi-operator")
	checks = append(checks, Check{
		Name:    "CDI installed",
		OK:      cdiInstalled,
		Detail:  detailIf(cdiInstalled, "containerized-data-importer present", "missing — fix installs CDI "+CDIVersion+" (ISO/image imports)"),
		Fixable: !cdiInstalled,
		fix:     installCDI,
	})

	cfg := kubevirtConfig()
	rollout := cfg.RolloutStrategy == "LiveUpdate"
	checks = append(checks, Check{
		Name:    "LiveUpdate rollout strategy",
		OK:      rollout,
		Detail:  "lets CPU/RAM hotplug live-migrate; offline fallback otherwise",
		Fixable: kvInstalled && !rollout,
		fix:     reconcileKubeVirt,
	})
	liveMig := contains(cfg.WorkloadUpdateMethods, "LiveMigrate")
	checks = append(checks, Check{
		Name:    "LiveMigrate workload updates",
		OK:      liveMig,
		Detail:  "required by live CPU/RAM hotplug",
		Fixable: kvInstalled && !liveMig,
		fix:     reconcileKubeVirt,
	})
	for _, gate := range []string{"Snapshot", "HotplugVolumes", "VMExport"} {
		has := contains(cfg.FeatureGates, gate)
		checks = append(checks, Check{
			Name:    "Feature gate: " + gate,
			OK:      has,
			Detail:  gateDetail(gate),
			Fixable: kvInstalled && !has,
			fix:     reconcileKubeVirt,
		})
	}

	scs := storageClasses()
	hasDefault, hasExpand := false, false
	for _, sc := range scs {
		if sc.Default {
			hasDefault = true
		}
		if sc.Expand {
			hasExpand = true
		}
	}
	checks = append(checks, Check{
		Name:   "Default StorageClass",
		OK:     hasDefault,
		Detail: "needed to provision VM disks",
	})
	checks = append(checks, Check{
		Name:   "Expandable StorageClass",
		OK:     hasExpand,
		Detail: detailIf(hasExpand, "online disk expansion available", "no SC with allowVolumeExpansion — install Longhorn/CSI (setup guide)"),
	})
	hasSnap := hasSnapshotClass()
	checks = append(checks, Check{
		Name:   "VolumeSnapshotClass",
		OK:     hasSnap,
		Detail: detailIf(hasSnap, "persistent-VM snapshots available", "needs a CSI driver + external-snapshotter (setup guide)"),
	})

	// Storage clone/migration readiness (#68), from CDI's StorageProfiles —
	// CDI is already a corral prerequisite and its profiles state exactly
	// what the StorageClass object can't: the effective clone strategy and
	// access modes. Only reported when a default SC's profile exists (CDI
	// absent or profiles unpopulated → stay silent rather than guess).
	if prof := defaultStorageProfile(scs); prof != nil {
		fastClone := prof.CloneStrategy == "csi-clone" || prof.CloneStrategy == "snapshot"
		checks = append(checks, Check{
			Name: "Fast VM cloning",
			OK:   fastClone,
			Detail: detailIf(fastClone,
				fmt.Sprintf("CDI clone strategy %q — corral clone avoids full disk copies", prof.CloneStrategy),
				fmt.Sprintf("CDI clone strategy %q — every corral clone is a full host-assisted disk copy (CSI driver lacks clone/snapshot support)", orUnset(prof.CloneStrategy))),
		})
		checks = append(checks, Check{
			Name: "Migratable storage (RWX)",
			OK:   prof.RWX,
			Detail: detailIf(prof.RWX,
				"default SC supports ReadWriteMany — disks don't block live migration",
				"default SC has no ReadWriteMany mode — VMs on it can't live-migrate (RWO disks pin to a node)"),
		})
	}

	// Advisory, not a pass/fail: Longhorn is network-replicated storage and
	// measurably slower than local-path/topolvm for VM disk IO — corral's own
	// PreferredStorageClass() already deprioritizes it for new disks. If it's
	// still the *cluster* default, every VM created without an explicit
	// --storage-class override still lands on it. No auto-fix — changing
	// which SC is annotated default is an infra decision, not something
	// corral should do unasked.
	if defaultSC := defaultLonghornSC(scs); defaultSC != "" {
		checks = append(checks, Check{
			Name: "Default StorageClass performance",
			OK:   false,
			Detail: fmt.Sprintf("cluster default %q is Longhorn (network-replicated) — slower disk IO than "+
				"local-path/topolvm; corral's own create flow already prefers a faster SC when one exists, "+
				"but the cluster default still affects anything created with --storage-class unset elsewhere", defaultSC),
		})
	}

	expProxy := okList("get", "deploy", "-A", "-l", "kubevirt.io=virt-exportproxy")
	checks = append(checks, Check{
		Name:   "Export proxy",
		OK:     expProxy,
		Detail: "deployed by the VMExport gate — powers backup/export",
	})
	metrics := ok("kubectl", "get", "apiservices", "v1beta1.metrics.k8s.io")
	checks = append(checks, Check{
		Name:    "metrics-server",
		OK:      metrics,
		Detail:  detailIf(metrics, "live CPU/memory + summary sparkline", "missing — fix installs metrics-server (powers the CPU graph)"),
		Fixable: !metrics,
		fix:     installMetricsServer,
	})

	// Informational: the pull-through cache that speeds bootc builds. Not
	// auto-fixed — it's an opt-in deploy (deploy/registry-cache.yaml).
	cache := ok("kubectl", "get", "svc", "registry-cache", "-n", "corral")
	checks = append(checks, Check{
		Name:   "registry cache",
		OK:     cache,
		Detail: detailIf(cache, "ghcr.io pulls cached on-cluster — faster bootc builds", "not deployed (optional) — kubectl apply -f deploy/registry-cache.yaml to speed bootc builds"),
	})

	// GPU/PCI passthrough — only checked if a device is actually permitted
	// (corral gpu enable). Not auto-fixable: IOMMU is a per-node BIOS+kernel
	// setting corral has no way to flip. Only signal available without a
	// privileged per-node probe: does the device plugin report the resource
	// as Allocatable on any node? It won't unless IOMMU + vfio-pci already
	// work — KubeVirt's device plugin only advertises what it can actually
	// bind, so "permitted but never allocatable anywhere" is the strongest
	// symptom of a passthrough prerequisite (IOMMU off, vfio-pci not bound,
	// or a wrong PCI vendor:device selector) that's checkable this way.
	if resources := permittedGPUResourceNames(); len(resources) > 0 {
		allocatable := nodeAllocatableResources()
		var missing []string
		for _, r := range resources {
			if !allocatable[r] {
				missing = append(missing, r)
			}
		}
		ok := len(missing) == 0
		checks = append(checks, Check{
			Name: "GPU/PCI passthrough",
			OK:   ok,
			Detail: detailIf(ok, "all permitted devices are allocatable on at least one node",
				fmt.Sprintf("permitted but not allocatable on any node: %s — check IOMMU is enabled "+
					"(BIOS + intel_iommu=on/amd_iommu=on kernel params) and the device is bound to "+
					"vfio-pci on the node that has it (see the setup guide)", strings.Join(missing, ", "))),
		})
	}

	return checks
}

// permittedGPUResourceNames returns the resourceName of every PCI/mediated
// device permitted in the KubeVirt CR (empty if none, or no CR yet).
func permittedGPUResourceNames() []string {
	out, err := run("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json")
	if err != nil {
		return nil
	}
	var kv struct {
		Spec struct {
			Configuration struct {
				PermittedHostDevices struct {
					PCIHostDevices  []struct{ ResourceName string } `json:"pciHostDevices"`
					MediatedDevices []struct{ ResourceName string } `json:"mediatedDevices"`
				} `json:"permittedHostDevices"`
			} `json:"configuration"`
		} `json:"spec"`
	}
	if json.Unmarshal(out, &kv) != nil {
		return nil
	}
	var names []string
	for _, d := range kv.Spec.Configuration.PermittedHostDevices.PCIHostDevices {
		names = append(names, d.ResourceName)
	}
	for _, d := range kv.Spec.Configuration.PermittedHostDevices.MediatedDevices {
		names = append(names, d.ResourceName)
	}
	return names
}

// nodeAllocatableResources returns the set of resource names (standard and
// extended, e.g. "amd.com/gpu") allocatable on at least one node.
func nodeAllocatableResources() map[string]bool {
	out, err := run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	set := map[string]bool{}
	for _, item := range res.Items {
		for name, qty := range item.Status.Allocatable {
			if qty != "0" {
				set[name] = true
			}
		}
	}
	return set
}

// FixOne reconciles a single named fixable check.
func FixOne(name string) error {
	for _, c := range Run() {
		if c.Name != name {
			continue
		}
		if c.OK {
			return nil
		}
		if !c.Fixable || c.fix == nil {
			return fmt.Errorf("%q is not auto-fixable", name)
		}
		return c.fix()
	}
	return fmt.Errorf("no check named %q", name)
}

// Fix reconciles every fixable check that isn't OK. Returns the names fixed.
func Fix() ([]string, error) {
	var fixed []string
	done := map[string]bool{} // dedupe shared fixes
	for _, c := range Run() {
		if c.OK || !c.Fixable || c.fix == nil {
			continue
		}
		key := fmt.Sprintf("%p", c.fix)
		if !done[key] {
			if err := c.fix(); err != nil {
				return fixed, err
			}
			done[key] = true
		}
		fixed = append(fixed, c.Name)
	}
	return fixed, nil
}

// ── Installation ──────────────────────────────────────────────────

// Pinned upstream versions the installer applies on a bare cluster.
const (
	KubeVirtVersion = "v1.8.3"
	CDIVersion      = "v1.65.0"
)

func applyURL(url string) error {
	out, err := run("kubectl", "apply", "-f", url)
	if err != nil {
		return fmt.Errorf("kubectl apply -f %s: %s", url, strings.TrimSpace(string(out)))
	}
	return nil
}

// installKubeVirt applies the upstream operator + CR, then best-effort
// reconciles Corral's feature gates (the CR may not be admitted yet — a
// follow-up `doctor fix` finishes the job once virt-operator is up).
func installKubeVirt() error {
	// Same guard as installCDI, for the same reason: a second virt-operator
	// reconciling the same CRDs against a different version is a cluster-wide
	// problem, and HCO-managed clusters are exactly where detection is hardest.
	if okList("get", "kubevirt", "-A") {
		return fmt.Errorf("KubeVirt is already installed in this cluster (a KubeVirt resource exists) — " +
			"installing another operator would leave two reconciling the same CRDs; " +
			"if Corral is not detecting it, please report the output of `kubectl get kubevirt -A`")
	}
	base := "https://github.com/kubevirt/kubevirt/releases/download/" + KubeVirtVersion
	if err := applyURL(base + "/kubevirt-operator.yaml"); err != nil {
		return err
	}
	if err := applyURL(base + "/kubevirt-cr.yaml"); err != nil {
		return err
	}
	reconcileKubeVirt() // best-effort; needs the admission webhook to be up
	return nil
}

// installMetricsServer applies the upstream metrics-server, then patches it with
// --kubelet-insecure-tls — kubelets on many clusters (Talos, kubeadm) serve
// self-signed certs that metrics-server otherwise rejects.
func installMetricsServer() error {
	const url = "https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
	if err := applyURL(url); err != nil {
		return err
	}
	out, err := run("kubectl", "patch", "deployment", "metrics-server", "-n", "kube-system",
		"--type=json", "-p",
		`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`)
	if err != nil {
		return fmt.Errorf("patch metrics-server: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// installCDI applies the upstream containerized-data-importer operator + CR.
// installCDI refuses when CDI is already present anywhere in the cluster.
//
// The check above should have caught that, but this is the operation that
// cannot be allowed to run twice: a second CDI operator fighting the first over
// the same CRDs is a cluster-wide mess, and the reporter of #168 was one
// `--fix` away from it. A detection bug should cost a misleading row, not a
// duplicated operator, so the destructive step re-asks rather than trusting it.
func installCDI() error {
	if okList("get", "cdi", "-A") {
		return fmt.Errorf("CDI is already installed in this cluster (a CDI resource exists) — " +
			"installing another operator would leave two fighting over the same CRDs; " +
			"if Corral is not detecting it, please report the output of `kubectl get cdi -A`")
	}
	base := "https://github.com/kubevirt/containerized-data-importer/releases/download/" + CDIVersion
	if err := applyURL(base + "/cdi-operator.yaml"); err != nil {
		return err
	}
	return applyURL(base + "/cdi-cr.yaml")
}

// ── KubeVirt config ───────────────────────────────────────────────

type kvConfig struct {
	RolloutStrategy       string
	WorkloadUpdateMethods []string
	FeatureGates          []string
}

func kubevirtConfig() kvConfig {
	out, err := run("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json")
	if err != nil {
		return kvConfig{}
	}
	var kv struct {
		Spec struct {
			Configuration struct {
				VMRolloutStrategy      string `json:"vmRolloutStrategy"`
				DeveloperConfiguration struct {
					FeatureGates []string `json:"featureGates"`
				} `json:"developerConfiguration"`
			} `json:"configuration"`
			WorkloadUpdateStrategy struct {
				WorkloadUpdateMethods []string `json:"workloadUpdateMethods"`
			} `json:"workloadUpdateStrategy"`
		} `json:"spec"`
	}
	if json.Unmarshal(out, &kv) != nil {
		return kvConfig{}
	}
	return kvConfig{
		RolloutStrategy:       kv.Spec.Configuration.VMRolloutStrategy,
		WorkloadUpdateMethods: kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods,
		FeatureGates:          kv.Spec.Configuration.DeveloperConfiguration.FeatureGates,
	}
}

// reconcileKubeVirt enables LiveUpdate + LiveMigrate + the Snapshot/
// HotplugVolumes/VMExport gates, preserving any other gates already set.
func reconcileKubeVirt() error {
	cfg := kubevirtConfig()
	gates := union(cfg.FeatureGates, []string{"Snapshot", "HotplugVolumes", "VMExport"})
	methods := union(cfg.WorkloadUpdateMethods, []string{"LiveMigrate"})
	patch := map[string]any{
		"spec": map[string]any{
			"configuration": map[string]any{
				"vmRolloutStrategy":      "LiveUpdate",
				"developerConfiguration": map[string]any{"featureGates": gates},
			},
			"workloadUpdateStrategy": map[string]any{"workloadUpdateMethods": methods},
		},
	}
	body, _ := json.Marshal(patch)
	out, err := run("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt",
		"--type", "merge", "-p", string(body))
	if err != nil {
		return fmt.Errorf("patching KubeVirt: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ── Storage helpers ───────────────────────────────────────────────

type scInfo struct {
	Name        string
	Provisioner string
	Default     bool
	Expand      bool
}

func storageClasses() []scInfo {
	out, err := run("kubectl", "get", "sc", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Provisioner          string `json:"provisioner"`
			AllowVolumeExpansion *bool  `json:"allowVolumeExpansion"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	var scs []scInfo
	for _, it := range res.Items {
		scs = append(scs, scInfo{
			Name:        it.Metadata.Name,
			Provisioner: it.Provisioner,
			Default:     it.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true",
			Expand:      it.AllowVolumeExpansion != nil && *it.AllowVolumeExpansion,
		})
	}
	return scs
}

// storageProfileInfo is what doctor needs from a CDI StorageProfile.
type storageProfileInfo struct {
	CloneStrategy string
	RWX           bool
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// defaultStorageProfile returns the CDI StorageProfile for the cluster's
// default StorageClass, or nil when there's no default SC / no profile.
func defaultStorageProfile(scs []scInfo) *storageProfileInfo {
	var def string
	for _, sc := range scs {
		if sc.Default {
			def = sc.Name
		}
	}
	if def == "" {
		return nil
	}
	out, err := run("kubectl", "get", "storageprofile", def, "-o", "json")
	if err != nil {
		return nil
	}
	var prof struct {
		Status struct {
			CloneStrategy     string `json:"cloneStrategy"`
			ClaimPropertySets []struct {
				AccessModes []string `json:"accessModes"`
			} `json:"claimPropertySets"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &prof) != nil {
		return nil
	}
	info := &storageProfileInfo{CloneStrategy: prof.Status.CloneStrategy}
	for _, set := range prof.Status.ClaimPropertySets {
		for _, m := range set.AccessModes {
			if m == "ReadWriteMany" {
				info.RWX = true
			}
		}
	}
	return info
}

// defaultLonghornSC returns the name of the cluster's default StorageClass
// if it's Longhorn (driver.longhorn.io), "" otherwise.
func defaultLonghornSC(scs []scInfo) string {
	for _, sc := range scs {
		if sc.Default && sc.Provisioner == "driver.longhorn.io" {
			return sc.Name
		}
	}
	return ""
}

func hasSnapshotClass() bool {
	out, err := run("kubectl", "get", "volumesnapshotclass", "-o", "name")
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// ── small helpers ─────────────────────────────────────────────────

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func union(a, b []string) []string {
	out := append([]string{}, a...)
	for _, x := range b {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func detailIf(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func gateDetail(gate string) string {
	switch gate {
	case "Snapshot":
		return "VM snapshots / restore"
	case "HotplugVolumes":
		return "add/remove disks on running VMs"
	case "VMExport":
		return "backup/export of VM disks"
	}
	return ""
}
