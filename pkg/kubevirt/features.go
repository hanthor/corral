package kubevirt

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// This file holds the "Proxmox-parity" VM operations layered on top of the
// basic lifecycle in client.go: live migration, CPU/memory scaling (live with
// an offline fallback), volume hotplug, disk expansion, snapshots, clone, and
// guest-agent introspection. Everything shells out to virtctl/kubectl — no
// client-side Kubernetes SDK — to keep Corral a single static binary.

// ── Lifecycle extras ──────────────────────────────────────────────

// RestartVM live-restarts a VirtualMachine.
func (c *Client) RestartVM(name string) error {
	return c.virtctlRun("restart", name, "-n", c.Namespace)
}

// PauseVM pauses a running VM (freezes vCPUs).
func (c *Client) PauseVM(name string) error {
	return c.virtctlRun("pause", "vm", name, "-n", c.Namespace)
}

// UnpauseVM resumes a paused VM.
func (c *Client) UnpauseVM(name string) error {
	return c.virtctlRun("unpause", "vm", name, "-n", c.Namespace)
}

// Migrate live-migrates a VM to another node. When targetNode is set, the VM's
// nodeSelector is pinned to it first (so it lands there); otherwise the
// scheduler picks. Only works for live-migratable VMs (ephemeral disk or RWX).
func (c *Client) Migrate(name, targetNode string) error {
	if targetNode == "" && !c.canLiveMigrate(name) {
		return fmt.Errorf("%s cannot be live-migrated: it is not migratable, or no other schedulable node shares its CPU vendor (live migration is impossible between e.g. Intel and AMD hosts)", name)
	}
	if targetNode != "" {
		patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":%q}}}}}`, targetNode)
		if err := c.patchVMMerge(name, patch); err != nil {
			return err
		}
	}
	return c.virtctlRun("migrate", name, "-n", c.Namespace)
}

// MigrationState is a snapshot of a VM's most recent live migration, read from
// the VMI's status.migrationState.
type MigrationState struct {
	Present    bool   `json:"present"` // a migrationState exists on the VMI
	Active     bool   `json:"active"`
	Completed  bool   `json:"completed"`
	Failed     bool   `json:"failed"`
	SourceNode string `json:"sourceNode"`
	TargetNode string `json:"targetNode"`
}

// MigrationState reads the VMI's live-migration progress. A VMI with no
// migration yet returns Present=false (not an error).
func (c *Client) MigrationState(name string) (MigrationState, error) {
	out, err := c.runner().Run("kubectl", "get", "vmi", name, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return MigrationState{}, err
	}
	var vmi struct {
		Status struct {
			MigrationState *struct {
				Completed  bool   `json:"completed"`
				Failed     bool   `json:"failed"`
				SourceNode string `json:"sourceNode"`
				TargetNode string `json:"targetNode"`
			} `json:"migrationState"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &vmi); err != nil {
		return MigrationState{}, err
	}
	ms := vmi.Status.MigrationState
	if ms == nil {
		return MigrationState{}, nil
	}
	return MigrationState{
		Present:    true,
		Active:     !ms.Completed && !ms.Failed,
		Completed:  ms.Completed,
		Failed:     ms.Failed,
		SourceNode: ms.SourceNode,
		TargetNode: ms.TargetNode,
	}, nil
}

func (c *Client) virtctlRun(args ...string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}
	_, err = c.runner().Run(virtctl, args...)
	return err
}

func (c *Client) patchVMMerge(name, patch string) error {
	_, err := c.runner().Run("kubectl", "patch", "vm", name, "-n", c.Namespace,
		"--type", "merge", "-p", patch)
	return err
}

// ── CPU / memory scaling ──────────────────────────────────────────

type vmDomainInfo struct {
	sockets, cores, threads, maxSockets int
	guestMib, maxGuestMib               int
	running                             bool
}

func (c *Client) vmDomain(name string) (vmDomainInfo, error) {
	out, err := c.runner().Run("kubectl", "get", "vm", name, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return vmDomainInfo{}, fmt.Errorf("reading VM %s: %w", name, err)
	}
	var vm struct {
		Spec struct {
			Running  *bool `json:"running"`
			Template struct {
				Spec struct {
					Domain struct {
						CPU struct {
							Cores      int `json:"cores"`
							Sockets    int `json:"sockets"`
							Threads    int `json:"threads"`
							MaxSockets int `json:"maxSockets"`
						} `json:"cpu"`
						Memory struct {
							Guest    string `json:"guest"`
							MaxGuest string `json:"maxGuest"`
						} `json:"memory"`
					} `json:"domain"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &vm); err != nil {
		return vmDomainInfo{}, err
	}
	d := vm.Spec.Template.Spec.Domain
	return vmDomainInfo{
		sockets:     d.CPU.Sockets,
		cores:       d.CPU.Cores,
		threads:     d.CPU.Threads,
		maxSockets:  d.CPU.MaxSockets,
		guestMib:    quantityToMib(d.Memory.Guest),
		maxGuestMib: quantityToMib(d.Memory.MaxGuest),
		running:     vm.Spec.Running != nil && *vm.Spec.Running,
	}, nil
}

// canLiveMigrate reports whether the VM can ACTUALLY be live-migrated: KubeVirt
// must mark it LiveMigratable (masquerade net, migratable storage) AND there
// must be another schedulable node sharing its CPU vendor. The vendor check
// matters on heterogeneous clusters — you cannot live-migrate a running VM
// between an Intel and an AMD host, so without a same-vendor target the
// migration would hang in Scheduling forever.
func (c *Client) canLiveMigrate(name string) bool {
	s, ok := vmiStatusIndexWithRunner(c.runner())[c.Namespace+"/"+name]
	if !ok || !s.LiveMigratable {
		return false
	}
	return hasMigrationTarget(s.Node, nodeVendorsWithRunner(c.runner()))
}

// nodeVendors maps each schedulable node to its KubeVirt CPU vendor label.
func nodeVendors() map[string]string {
	return nodeVendorsWithRunner(getPackageRunner())
}
func nodeVendorsWithRunner(r shell.Runner) map[string]string {
	out, err := r.Run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	vendors := map[string]string{}
	for _, n := range res.Items {
		if n.Metadata.Labels["kubevirt.io/schedulable"] != "true" {
			continue
		}
		for k := range n.Metadata.Labels {
			if v, ok := strings.CutPrefix(k, "cpu-vendor.node.kubevirt.io/"); ok {
				vendors[n.Metadata.Name] = v
				break
			}
		}
	}
	return vendors
}

// hasMigrationTarget reports whether a node other than `node` shares its vendor.
func hasMigrationTarget(node string, vendors map[string]string) bool {
	want, ok := vendors[node]
	if !ok || want == "" {
		return false
	}
	for n, v := range vendors {
		if n != node && v == want {
			return true
		}
	}
	return false
}

// ScaleCPU sets the VM's vCPU count (live when possible, else offline).
func (c *Client) ScaleCPU(name string, vcpus int) error {
	if vcpus < 1 {
		return fmt.Errorf("cpu must be >= 1")
	}
	return c.Scale(name, vcpus, "")
}

// ScaleMemory sets the VM's guest memory, e.g. "8G" (live when possible, else offline).
func (c *Client) ScaleMemory(name, mem string) error {
	return c.Scale(name, 0, mem)
}

// Scale changes CPU and/or memory in one operation. When the VM is running and
// genuinely live-migratable (same-vendor target node, masquerade net) and the
// new values fit the maxSockets/maxGuest headroom, both are hotplugged live
// with no downtime. Otherwise both are applied in a single stop→patch→start
// cycle (one reboot, not two). vcpus==0 / mem=="" mean "leave unchanged".
func (c *Client) Scale(name string, vcpus int, mem string) error {
	if vcpus == 0 && mem == "" {
		return fmt.Errorf("nothing to change")
	}
	d, err := c.vmDomain(name)
	if err != nil {
		return err
	}
	mib := 0
	if mem != "" {
		if mib = quantityToMib(mem); mib < 1 {
			return fmt.Errorf("invalid memory %q", mem)
		}
	}
	socketsBased := d.cores <= 1 && d.threads <= 1
	cpuFits := vcpus == 0 || (socketsBased && d.maxSockets >= vcpus)
	memFits := mem == "" || mib <= d.maxGuestMib

	if d.running && cpuFits && memFits && c.canLiveMigrate(name) {
		if vcpus > 0 {
			if err := c.patchVMMerge(name,
				fmt.Sprintf(`{"spec":{"template":{"spec":{"domain":{"cpu":{"sockets":%d}}}}}}`, vcpus)); err != nil {
				return err
			}
		}
		if mem != "" {
			if err := c.patchVMMerge(name,
				fmt.Sprintf(`{"spec":{"template":{"spec":{"domain":{"memory":{"guest":"%dMi"}}}}}}`, mib)); err != nil {
				return err
			}
		}
		return nil
	}

	domain := map[string]any{}
	if vcpus > 0 {
		domain["cpu"] = cpuSpec(vcpus)
	}
	if mem != "" {
		domain["memory"] = memSpec(mib)
	}
	return c.offlinePatch(name, d.running, domain)
}

// offlinePatch applies a domain-level merge patch, stopping/starting the VM if
// it is running so the change takes effect immediately.
func (c *Client) offlinePatch(name string, running bool, domain map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"domain": domain}}},
	})
	if err != nil {
		return err
	}
	if running {
		if err := c.StopVM(name); err != nil {
			return err
		}
		c.waitStopped(name)
	}
	if err := c.patchVMMerge(name, string(body)); err != nil {
		return err
	}
	if running {
		return c.StartVM(name)
	}
	return nil
}

func (c *Client) waitStopped(name string) {
	for i := 0; i < 60; i++ {
		if _, err := c.runner().Run("kubectl", "get", "vmi", name, "-n", c.Namespace); err != nil {
			return // VMI gone
		}
		time.Sleep(time.Second)
	}
}

// ── Volumes ───────────────────────────────────────────────────────

// AddVolume creates a new PVC and hotplugs it onto the running VM. Returns the
// PVC name. The PVC is labeled for cleanup on VM delete.
func (c *Client) AddVolume(name, size string) (string, error) {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return "", err
	}
	if size == "" {
		size = "10Gi"
	}
	pvcName := fmt.Sprintf("%s-hp-%d", name, time.Now().Unix())
	pvc := GeneratePVCWithClass(pvcName, c.Namespace, size, PreferredStorageClass())
	pvc["metadata"].(map[string]any)["labels"] = map[string]any{"corral.dev/vm": name}
	if err := Apply(pvc); err != nil {
		return "", fmt.Errorf("creating disk PVC: %w", err)
	}
	_, err = c.runner().Run(virtctl, "addvolume", name, "--volume-name="+pvcName, "-n", c.Namespace)
	if err != nil {
		return "", fmt.Errorf("addvolume: %w", err)
	}
	return pvcName, nil
}

// RemoveVolume detaches a hotplugged volume from the VM.
func (c *Client) RemoveVolume(name, vol string) error {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return err
	}
	_, err = c.runner().Run(virtctl, "removevolume", name, "--volume-name="+vol, "-n", c.Namespace)
	return err
}

// ExpandDisk grows a PVC to the given size (requires an expandable StorageClass).
func (c *Client) ExpandDisk(pvc, size string) error {
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":%q}}}}`, size)
	_, err := c.runner().Run("kubectl", "patch", "pvc", pvc, "-n", c.Namespace,
		"--type", "merge", "-p", patch)
	return err
}

// ── Snapshots / clone ─────────────────────────────────────────────

// SnapshotInfo is a row in the snapshots list.
type SnapshotInfo struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Ready   bool   `json:"ready"`
	Created string `json:"created"`
}

// Snapshot creates a VirtualMachineSnapshot. If snap is empty a name is
// generated. Returns the snapshot name. With the guest agent connected,
// KubeVirt freezes the filesystem for a consistent snapshot automatically.
func (c *Client) Snapshot(vm, snap string) (string, error) {
	if snap == "" {
		snap = fmt.Sprintf("%s-snap-%d", vm, time.Now().Unix())
	}
	obj := map[string]any{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineSnapshot",
		"metadata": map[string]any{
			"name":      snap,
			"namespace": c.Namespace,
			"labels":    map[string]any{"corral.dev/vm": vm},
		},
		"spec": map[string]any{
			"source": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm},
		},
	}
	return snap, Apply(obj)
}

// ListSnapshots returns snapshots, optionally filtered to a single VM.
func (c *Client) ListSnapshots(vm string) ([]SnapshotInfo, error) {
	out, err := c.runner().Run("kubectl", "get", "vmsnapshot", "-n", c.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Source struct {
					Name string `json:"name"`
				} `json:"source"`
			} `json:"spec"`
			Status struct {
				ReadyToUse *bool `json:"readyToUse"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	snaps := []SnapshotInfo{}
	for _, it := range res.Items {
		if vm != "" && it.Spec.Source.Name != vm {
			continue
		}
		snaps = append(snaps, SnapshotInfo{
			Name:    it.Metadata.Name,
			Source:  it.Spec.Source.Name,
			Ready:   it.Status.ReadyToUse != nil && *it.Status.ReadyToUse,
			Created: it.Metadata.CreationTimestamp,
		})
	}
	return snaps, nil
}

// RestoreSnapshot restores a VM from a snapshot (VM must be stopped).
func (c *Client) RestoreSnapshot(vm, snap string) error {
	obj := map[string]any{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineRestore",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("%s-restore-%d", vm, time.Now().Unix()),
			"namespace": c.Namespace,
		},
		"spec": map[string]any{
			"target":                     map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm},
			"virtualMachineSnapshotName": snap,
		},
	}
	return Apply(obj)
}

// DeleteSnapshot removes a VirtualMachineSnapshot.
func (c *Client) DeleteSnapshot(snap string) error {
	_, err := c.runner().Run("kubectl", "delete", "vmsnapshot", snap, "-n", c.Namespace, "--ignore-not-found")
	return err
}

// Clone creates a VirtualMachineClone from src into a new VM named dst.
func (c *Client) Clone(src, dst string) error {
	obj := map[string]any{
		"apiVersion": "clone.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineClone",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("clone-%s-%d", dst, time.Now().Unix()),
			"namespace": c.Namespace,
		},
		"spec": map[string]any{
			"source": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": src},
			"target": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": dst},
			// Copy all labels except the template marker — a clone isn't a template.
			"labelFilters": []string{"*", "!corral.dev/template"},
		},
	}
	return Apply(obj)
}

// ── Export / backup ───────────────────────────────────────────────

// primaryPVC returns the claim name of the VM's first persistent (PVC-backed)
// volume — the disk worth backing up. Empty for fully ephemeral VMs.
func (c *Client) primaryPVC(name string) (string, error) {
	out, err := c.runner().Run("kubectl", "get", "vm", name, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("reading VM %s: %w", name, err)
	}
	var vm struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						PersistentVolumeClaim *struct {
							ClaimName string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &vm); err != nil {
		return "", err
	}
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
			return v.PersistentVolumeClaim.ClaimName, nil
		}
	}
	return "", nil
}

// Export backs up a VM's disk to outputPath as a gzip image via the KubeVirt
// export API (virtctl vmexport). The VM should be stopped — its RWO PVC can't
// be read while a running VM holds it. volume defaults to the primary PVC;
// outputPath defaults to "<name>.img.gz". virtctl creates the
// VirtualMachineExport, downloads, and cleans it up.
func (c *Client) Export(name, volume, outputPath string) (string, error) {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return "", err
	}
	if volume == "" {
		if volume, err = c.primaryPVC(name); err != nil {
			return "", err
		}
		if volume == "" {
			return "", fmt.Errorf("%s has no persistent disk to export (ephemeral container-disk VMs have nothing to back up)", name)
		}
	}
	return c.exportFormat(virtctl, name, volume, outputPath, "gzip")
}

// ExportRaw downloads the VM's disk uncompressed (no gzip) — the source for an
// on-the-fly qcow2 conversion. Same preconditions as Export.
func (c *Client) ExportRaw(name, volume, outputPath string) (string, error) {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return "", err
	}
	if volume == "" {
		if volume, err = c.primaryPVC(name); err != nil {
			return "", err
		}
		if volume == "" {
			return "", fmt.Errorf("%s has no persistent disk to export (ephemeral container-disk VMs have nothing to back up)", name)
		}
	}
	return c.exportFormat(virtctl, name, volume, outputPath, "raw")
}

func (c *Client) exportFormat(virtctl, name, volume, outputPath, format string) (string, error) {
	if outputPath == "" {
		outputPath = name + ".img.gz"
	}
	expName := name + "-export"
	// Clear any export left over from an interrupted run so --vm can recreate it.
	shell.CommandForContext(c.Context, "kubectl", "delete", "vmexport", expName, "-n", c.Namespace, "--ignore-not-found").Run()
	// --port-forward tunnels straight to the exporter pod (internal links); the
	// export proxy has no external Ingress, so external links never appear.
	args := []string{"vmexport", "download", expName,
		"--namespace=" + c.Namespace, "--vm=" + name, "--volume=" + volume,
		"--output=" + outputPath, "--format=" + format, "--insecure", "--port-forward"}
	cmd := shell.CommandForContext(c.Context, virtctl, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("vmexport download (is the VM stopped?): %w", err)
	}
	return outputPath, nil
}

// ── Guest agent ───────────────────────────────────────────────────

// GuestInfo returns guest-agent data (OS, filesystems, users). Errors when the
// agent is not connected.
func (c *Client) GuestInfo(name string) (map[string]any, error) {
	virtctl, err := c.ensureVirtctl()
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	out, err := c.runner().Run(virtctl, "guestosinfo", name, "-n", c.Namespace)
	if err != nil {
		return nil, fmt.Errorf("guest agent not available (is qemu-guest-agent installed and running?)")
	}
	var os any
	json.Unmarshal(out, &os)
	res["os"] = os
	if out, err := c.runner().Run(virtctl, "fslist", name, "-n", c.Namespace); err == nil {
		var fs any
		json.Unmarshal(out, &fs)
		res["filesystems"] = fs
	}
	if out, err := c.runner().Run(virtctl, "userlist", name, "-n", c.Namespace); err == nil {
		var users any
		json.Unmarshal(out, &users)
		res["users"] = users
	}
	return res, nil
}

// ── Secondary networks (Multus) ───────────────────────────────────

// ListNADs returns Multus NetworkAttachmentDefinitions ("ns/name"). Empty when
// Multus isn't installed.
func ListNADs() []string {
	out, err := runPkg("kubectl", "get", "net-attach-def", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return []string{}
	}
	nads := []string{}
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			nads = append(nads, n)
		}
	}
	return nads
}

// ResolveNAD picks the NAD `--lan`/`--network-nad` should use: explicit wins
// outright; otherwise exactly one NAD on the cluster is used automatically,
// and zero or several is an error rather than a guess (guessing wrong wires
// the VM onto the wrong LAN silently). available is ListNADs()'s output,
// passed in so this stays a pure, testable decision — no cluster call here.
func ResolveNAD(explicit string, available []string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch len(available) {
	case 0:
		return "", fmt.Errorf("--lan needs a Multus NetworkAttachmentDefinition on the cluster, but none exist (see docs/lan-networking.md)")
	case 1:
		return available[0], nil
	default:
		return "", fmt.Errorf("multiple NetworkAttachmentDefinitions exist (%s) — specify one with --network-nad", strings.Join(available, ", "))
	}
}

// AddNIC attaches a secondary interface backed by a Multus NAD to the VM
// (bridge binding). Applied to the VM spec; KubeVirt hotplugs it on a running
// VM where supported, else it takes effect on next boot.
func (c *Client) AddNIC(name, nad, iface string) error {
	if iface == "" {
		iface = "net1"
	}
	// JSON-patch: append an interface + a matching multus network.
	patch := fmt.Sprintf(`[
      {"op":"add","path":"/spec/template/spec/domain/devices/interfaces/-","value":{"name":%q,"bridge":{}}},
      {"op":"add","path":"/spec/template/spec/networks/-","value":{"name":%q,"multus":{"networkName":%q}}}
    ]`, iface, iface, nad)
	_, err := c.runner().Run("kubectl", "patch", "vm", name, "-n", c.Namespace,
		"--type", "json", "-p", patch)
	return err
}

// ── Templates ─────────────────────────────────────────────────────

// MarkTemplate labels (or unlabels) a VM as a golden template. Templates are
// ordinary stopped VMs that "create from template" clones.
// tagLabelPrefix namespaces user tags as VM labels so they survive round-trips
// and are selectable with `kubectl get vm -l corral.dev/tag.<name>`.
const tagLabelPrefix = "corral.dev/tag."

// tagsFromLabels extracts the sorted set of user tags from a VM's labels.
func tagsFromLabels(labels map[string]string) []string {
	var tags []string
	for k, v := range labels {
		if v == "true" && strings.HasPrefix(k, tagLabelPrefix) {
			if t := strings.TrimPrefix(k, tagLabelPrefix); t != "" {
				tags = append(tags, t)
			}
		}
	}
	sort.Strings(tags)
	return tags
}

// sanitizeTag normalises a user tag to a valid Kubernetes label-name segment
// (lowercase alnum plus -_.). Returns "" if nothing valid remains.
func sanitizeTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	var b strings.Builder
	for _, r := range tag {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// SetTag adds (on) or removes (off) a single tag label on a VM.
func (c *Client) SetTag(name, tag string, on bool) error {
	t := sanitizeTag(tag)
	if t == "" {
		return fmt.Errorf("invalid tag %q (use letters, digits, -_.)", tag)
	}
	val := tagLabelPrefix + t + "=true"
	if !on {
		val = tagLabelPrefix + t + "-" // trailing - removes the label
	}
	_, err := c.runner().Run("kubectl", "label", "vm", name, "-n", c.Namespace, val, "--overwrite")
	return err
}

func (c *Client) MarkTemplate(name string, on bool) error {
	val := "corral.dev/template=true"
	if !on {
		val = "corral.dev/template-" // trailing - removes the label
	}
	_, err := c.runner().Run("kubectl", "label", "vm", name, "-n", c.Namespace, val, "--overwrite")
	return err
}

// CreateFromTemplate clones a template VM into a new VM (via VirtualMachineClone,
// which copies disks too). The clone is not itself a template.
func (c *Client) CreateFromTemplate(template, newName string) error {
	if err := c.Clone(template, newName); err != nil {
		return err
	}
	return nil
}

// ── Instancetypes / preferences ───────────────────────────────────

// ListInstanceTypes returns cluster-wide KubeVirt instancetype names (sizing).
func ListInstanceTypes() []string {
	return kubectlNames("virtualmachineclusterinstancetypes")
}

// ListPreferences returns cluster-wide KubeVirt preference names (guest defaults).
func ListPreferences() []string {
	return kubectlNames("virtualmachineclusterpreferences")
}

func kubectlNames(resource string) []string {
	out, err := runPkg("kubectl", "get", resource,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return []string{}
	}
	names := []string{}
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// ── ISO / image library (CDI DataVolumes) ─────────────────────────

// DataVolumeInfo is a row in the image library.
type DataVolumeInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Size      string `json:"size"`
	Phase     string `json:"phase"`
	Progress  string `json:"progress"`
	Source    string `json:"source"`
}

// ListDataVolumes returns all CDI DataVolumes (imported ISOs/images).
func ListDataVolumes() ([]DataVolumeInfo, error) {
	out, err := runPkg("kubectl", "get", "datavolumes", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Source struct {
					HTTP *struct {
						URL string `json:"url"`
					} `json:"http"`
					Registry *struct {
						URL string `json:"url"`
					} `json:"registry"`
				} `json:"source"`
				PVC     *pvcResources `json:"pvc"`
				Storage *pvcResources `json:"storage"`
			} `json:"spec"`
			Status struct {
				Phase    string `json:"phase"`
				Progress string `json:"progress"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	dvs := []DataVolumeInfo{}
	for _, it := range res.Items {
		size := ""
		if it.Spec.PVC != nil {
			size = it.Spec.PVC.Resources.Requests.Storage
		} else if it.Spec.Storage != nil {
			size = it.Spec.Storage.Resources.Requests.Storage
		}
		src := ""
		if it.Spec.Source.HTTP != nil {
			src = it.Spec.Source.HTTP.URL
		} else if it.Spec.Source.Registry != nil {
			src = it.Spec.Source.Registry.URL
		}
		dvs = append(dvs, DataVolumeInfo{
			Name:      it.Metadata.Name,
			Namespace: it.Metadata.Namespace,
			Size:      size,
			Phase:     it.Status.Phase,
			Progress:  it.Status.Progress,
			Source:    src,
		})
	}
	return dvs, nil
}

type pvcResources struct {
	Resources struct {
		Requests struct {
			Storage string `json:"storage"`
		} `json:"requests"`
	} `json:"resources"`
}

// ImportDataVolume creates a CDI DataVolume importing an image from a URL.
func ImportDataVolume(name, namespace, url, size string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if size == "" {
		size = DetectISOSize(url)
	}
	EnsureNamespace(namespace)
	dv := GenerateDataVolume(name, namespace, url, size)
	if sc := PreferredStorageClass(); sc != "" {
		dv["spec"].(map[string]any)["pvc"].(map[string]any)["storageClassName"] = sc
	}
	return Apply(dv)
}

// DeleteDataVolume removes a DataVolume (and its PVC).
func DeleteDataVolume(namespace, name string) error {
	_, err := runPkg("kubectl", "delete", "datavolume", name, "-n", namespace, "--ignore-not-found")
	return err
}

// ── Observability: events + metrics ───────────────────────────────

// EventInfo is one row in the Proxmox-style task/event log.
type EventInfo struct {
	Time    string `json:"time"`
	Type    string `json:"type"` // Normal | Warning
	Reason  string `json:"reason"`
	Object  string `json:"object"` // kind/name
	Message string `json:"message"`
}

// Events returns recent Kubernetes events for a VM (its VM/VMI objects and its
// virt-launcher pod), newest first.
func (c *Client) Events(name string) ([]EventInfo, error) {
	out, err := c.runner().Run("kubectl", "get", "events", "-n", c.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []struct {
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			LastTimestamp  string `json:"lastTimestamp"`
			EventTime      string `json:"eventTime"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	launcher := "virt-launcher-" + name
	evs := []EventInfo{}
	for _, it := range res.Items {
		n := it.InvolvedObject.Name
		if n != name && !strings.HasPrefix(n, launcher) {
			continue
		}
		t := it.LastTimestamp
		if t == "" {
			t = it.EventTime
		}
		evs = append(evs, EventInfo{
			Time:    t,
			Type:    it.Type,
			Reason:  it.Reason,
			Object:  it.InvolvedObject.Kind + "/" + n,
			Message: it.Message,
		})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Time > evs[j].Time })
	if len(evs) > 50 {
		evs = evs[:50]
	}
	return evs, nil
}

// Metrics returns the VM's live CPU and memory usage (its virt-launcher pod),
// via metrics-server. Empty strings if metrics aren't available yet.
func (c *Client) Metrics(name string) map[string]string {
	out, err := c.runner().Run("kubectl", "top", "pod", "-n", c.Namespace,
		"-l", "kubevirt.io/vm="+name, "--no-headers")
	res := map[string]string{"cpu": "", "mem": ""}
	if err != nil {
		return res
	}
	if f := strings.Fields(string(out)); len(f) >= 3 {
		res["cpu"] = f[1]
		res["mem"] = f[2]
	}
	return res
}

// SampleAllCPU returns current CPU usage in millicores for every running VM,
// keyed by "namespace/vm". It joins `kubectl top` (usage by pod) with the
// virt-launcher pods' vm.kubevirt.io/name label. Returns nil when
// metrics-server is unavailable, so callers degrade gracefully.
func SampleAllCPU() map[string]int {
	out, err := runPkg("kubectl", "get", "pods", "-A", "-l", "kubevirt.io=virt-launcher", "-o", "json")
	if err != nil {
		return nil
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &pods) != nil {
		return nil
	}
	podToVM := map[string]string{} // "ns/podname" -> "ns/vm"
	for _, p := range pods.Items {
		if vm := p.Metadata.Labels["vm.kubevirt.io/name"]; vm != "" {
			podToVM[p.Metadata.Namespace+"/"+p.Metadata.Name] = p.Metadata.Namespace + "/" + vm
		}
	}
	if len(podToVM) == 0 {
		return map[string]int{}
	}
	top, err := runPkg("kubectl", "top", "pod", "-A", "-l", "kubevirt.io=virt-launcher", "--no-headers")
	if err != nil {
		return nil // metrics-server absent
	}
	res := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(top)), "\n") {
		f := strings.Fields(line) // NAMESPACE NAME CPU(cores) MEMORY(bytes)
		if len(f) < 3 {
			continue
		}
		if key, ok := podToVM[f[0]+"/"+f[1]]; ok {
			res[key] = parseMilliCPU(f[2])
		}
	}
	return res
}

// parseMilliCPU parses a `kubectl top` CPU value ("123m", or "1" = 1 core).
func parseMilliCPU(s string) int {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "m"))
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f * 1000)
	}
	return 0
}

// ── Storage / capability detection ────────────────────────────────

// GeneratePVCWithClass is GeneratePVC with an explicit StorageClass ("" = cluster default).
func GeneratePVCWithClass(name, namespace, size, storageClass string) map[string]any {
	pvc := GeneratePVC(name, namespace, size)
	if storageClass != "" {
		pvc["spec"].(map[string]any)["storageClassName"] = storageClass
	}
	return pvc
}

// PreferredStorageClass returns the StorageClass Corral prefers for new disks:
// "local-path" when present (local NVMe speed — no network IO), else "" (cluster default).
// Snapshot and expansion features are detected automatically from the cluster.
func PreferredStorageClass() string {
	return ClusterCapabilities().StorageClass
}

// ClusterCapabilities reports which optional storage operations are available,
// so callers can enable/disable UI controls rather than fail on click.
func ClusterCapabilities() types.Capabilities {
	out, err := runPkg("kubectl", "get", "sc", "-o", "json")
	if err != nil {
		return types.Capabilities{}
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			AllowVolumeExpansion *bool `json:"allowVolumeExpansion"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return types.Capabilities{}
	}
	expand := map[string]bool{}
	var preferred, def string
	for _, it := range res.Items {
		expand[it.Metadata.Name] = it.AllowVolumeExpansion != nil && *it.AllowVolumeExpansion
		if it.Metadata.Name == "local-path" {
			preferred = "local-path"
		} else if (strings.Contains(it.Metadata.Name, "topolvm") || it.Metadata.Name == "topolvm") && preferred != "local-path" {
			preferred = it.Metadata.Name
		}
		if it.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			def = it.Metadata.Name
		}
	}
	effective := preferred
	if effective == "" {
		effective = def
	}
	return types.Capabilities{
		StorageClass: effective,
		CanExpand:    expand[effective],
		CanSnapshot:  hasSnapshotClass(),
	}
}

func hasSnapshotClass() bool {
	out, err := runPkg("kubectl", "get", "volumesnapshotclass", "-o", "name")
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// quantityToMib parses a Kubernetes memory quantity (Gi/G/Mi/M/Ki/K or raw) to MiB.
func quantityToMib(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var num float64
	var unit string
	fmt.Sscanf(s, "%f%s", &num, &unit)
	switch strings.ToLower(unit) {
	case "gi", "g":
		return int(num * 1024)
	case "mi", "m", "":
		return int(num)
	case "ki", "k":
		return int(num / 1024)
	default:
		return int(num)
	}
}
