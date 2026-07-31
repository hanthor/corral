package demo

// Package demo provides an in-memory fake cluster plugged into the same
// shell.Runner seams the unit tests use, so the web UI, TUI, and CLI all run
// their real parsing/derivation logic against synthetic kubectl/virtctl
// output. Everything is explorable with no cluster: a varied VM fleet, CTs,
// nodes, a live CPU feed for sparklines, and stateful start/stop/pause/
// delete/create so UI transitions can be exercised end to end.
// Enabled by --demo on `corral web` (dashboard) and `corral` (TUI/CLI).

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/shell"
)

// Enable swaps every command-runner seam for the in-memory fake cluster and
// returns the runner so callers with their own seam (pkg/web's defaultRunner)
// can install it there too. Call before any backend use.
func Enable() shell.Runner {
	// Make the in-memory Incus remote an explicit inventory context. Production
	// never invents an Incus target, while demo surfaces still exercise it.
	os.Setenv("CORRAL_INCUS_REMOTE", "local")
	// Likewise for KubeVirt, which production offers only when the host has a
	// kubeconfig. The demo cluster is entirely in memory, so the demo fleet
	// must not depend on whether this machine has ever run kubectl.
	config.SetForceKubevirtContext(true)
	d := newDemoCluster()
	kubevirt.SetDefaultRunner(d)
	kubevirt.SetPackageRunner(d)
	kubevirt.SetApplyRunner(d)
	ct.SetRunner(d)
	doctor.SetRunner(d)
	incus.SetRunner(d)
	d.enableLocalBackend()
	return d
}

// enableLocalBackend gives the demo a fake local QEMU VM (#91 Phase 4):
// throwaway state dirs plus an in-memory systemd, so the "local" node,
// lifecycle, and merged tree render without qemu or systemd units on the
// host. Best-effort — a failure just means no local VM in the demo.
func (d *demoCluster) enableLocalBackend() {
	vmHome, err := os.MkdirTemp("", "corral-demo-vms-*")
	if err != nil {
		return
	}
	unitDir, err := os.MkdirTemp("", "corral-demo-units-*")
	if err != nil {
		return
	}
	name := "laptop-dev"
	dir := filepath.Join(vmHome, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	meta := `{"name":"` + name + `","cpu":2,"memory":"4G","disk_size":"30G","vnc_port":5901,"tailscale_ip":"100.64.0.10"}`
	if os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o644) != nil {
		return
	}
	if os.WriteFile(filepath.Join(unitDir, "corral-"+name+".service"), []byte("# demo unit\n"), 0o644) != nil {
		return
	}
	qemu.SetStateDirs(vmHome, unitDir)
	qemu.SetSystemctl(d.localSystemctl)
}

// localSystemctl is the demo's in-memory systemd --user: is-active reads,
// start/stop flip, everything else succeeds quietly.
func (d *demoCluster) localSystemctl(args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(args) < 1 {
		return []byte{}, nil
	}
	svcName := ""
	if len(args) > 1 {
		svcName = strings.TrimPrefix(args[len(args)-1], "corral-")
	}
	switch args[0] {
	case "is-active":
		if d.localRunning[svcName] {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), fmt.Errorf("inactive")
	case "start":
		d.localRunning[svcName] = true
	case "stop":
		delete(d.localRunning, svcName)
	}
	return []byte{}, nil
}

type demoVM struct {
	Name, NS, Node string
	Status         string // printableStatus: Running | Stopped | Paused | Starting | Migrating
	CPU            int
	Mem            string
	IP             string
	Tags           []string
	Bootc          bool
	Ephemeral      bool
	Template       bool
	ExpiresAt      string
	Load           float64 // baseline CPU load in millicores for the sparkline
}

func (v *demoVM) running() bool {
	return v.Status == "Running" || v.Status == "Paused" || v.Status == "Migrating"
}

type demoCT struct {
	Name, NS, Node, Image string
	CPU                   int
	Mem                   string
	Running               bool
	Privileged            bool
}

type demoCluster struct {
	mu           sync.Mutex
	vms          []*demoVM
	cts          []*demoCT
	localRunning map[string]bool // fake local (qemu) VMs' systemd state
	start        time.Time
}

func newDemoCluster() *demoCluster {
	expires := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	return &demoCluster{
		start:        time.Now(),
		localRunning: map[string]bool{},
		vms: []*demoVM{
			{Name: "web-prod", NS: "corral-vms", Node: "corral-1", Status: "Running", CPU: 2, Mem: "4Gi", IP: "10.42.1.20", Tags: []string{"prod", "web"}, Load: 240},
			{Name: "db-prod", NS: "corral-vms", Node: "corral-2", Status: "Running", CPU: 4, Mem: "8Gi", IP: "10.42.2.31", Tags: []string{"prod", "db"}, Load: 900},
			{Name: "builder", NS: "corral-vms", Node: "corral-3", Status: "Running", CPU: 4, Mem: "8Gi", IP: "10.42.3.12", Bootc: true, Load: 1800},
			{Name: "scratch", NS: "corral-vms", Node: "corral-1", Status: "Running", CPU: 1, Mem: "2Gi", IP: "10.42.1.44", Tags: []string{"scratch"}, Ephemeral: true, ExpiresAt: expires, Load: 60},
			{Name: "win11-desktop", NS: "corral-vms", Node: "corral-2", Status: "Paused", CPU: 4, Mem: "8Gi", IP: "10.42.2.55", Tags: []string{"desktop"}},
			{Name: "dev-fedora", NS: "corral-vms", Node: "", Status: "Stopped", CPU: 2, Mem: "4Gi", Tags: []string{"dev"}},
			{Name: "iso-install", NS: "corral-vms", Node: "corral-3", Status: "Starting", CPU: 2, Mem: "4Gi"},
			{Name: "golden-ubuntu", NS: "corral-vms", Node: "", Status: "Stopped", CPU: 2, Mem: "4Gi", Template: true},
		},
		cts: []*demoCT{
			{Name: "dev-shell", NS: "corral-vms", Node: "corral-1", Image: "docker.io/library/debian:bookworm", CPU: 1, Mem: "512Mi", Running: true, Privileged: true},
			{Name: "files", NS: "corral-vms", Node: "", Image: "docker.io/library/alpine:3.20", CPU: 1, Mem: "256Mi", Running: false},
		},
	}
}

func (d *demoCluster) find(name, ns string) *demoVM {
	for _, v := range d.vms {
		if v.Name == name && (ns == "" || v.NS == ns) {
			return v
		}
	}
	return nil
}

// ── shell.Runner ──────────────────────────────────────────────────

func (d *demoCluster) LookPath(name string) (string, error) { return "/demo/bin/" + name, nil }

func (d *demoCluster) Run(name string, args ...string) ([]byte, error) {
	return d.dispatch("", name, args)
}

func (d *demoCluster) RunStdin(stdin string, name string, args ...string) ([]byte, error) {
	return d.dispatch(stdin, name, args)
}

// hasJSONOutput reports whether args ask for `-o json` specifically —
// NOT jsonpath, whose callers expect a plain string, not an items list.
func hasJSONOutput(args []string) bool {
	for i, a := range args {
		if a == "-ojson" || (a == "-o" && i+1 < len(args) && args[i+1] == "json") {
			return true
		}
	}
	return false
}

// flagValue returns the value following a flag like "-n" in args.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (d *demoCluster) dispatch(stdin, name string, args []string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if filepath.Base(name) == "incus" {
		if len(args) >= 1 && args[0] == "list" {
			// One container and one virtual machine: Incus hosts both, and
			// Corral models them as different things (a container is a CT, a
			// VM is a VM). A single-container fixture hid that split, and with
			// it the bug where every Incus instance was listed twice.
			return []byte(`[` +
				`{"name":"incus-demo-container","type":"container","status":"Running","status_code":103,` +
				`"location":"localhost","config":{"limits.cpu":"2","limits.memory":"2Gi"},` +
				`"state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.60.0.21","scope":"global"}]}}}},` +
				`{"name":"incus-demo-vm","type":"virtual-machine","status":"Running","status_code":103,` +
				`"location":"localhost","config":{"limits.cpu":"4","limits.memory":"4Gi"},` +
				`"state":{"network":{"enp5s0":{"addresses":[{"family":"inet","address":"10.60.0.22","scope":"global"}]}}}}` +
				`]`), nil
		}
		return []byte{}, nil
	}
	if filepath.Base(name) == "virtctl" && len(args) >= 2 {
		return d.virtctl(args)
	}
	if filepath.Base(name) != "kubectl" {
		return []byte{}, nil // ssh/systemctl/etc — succeed quietly
	}

	key := strings.Join(args, " ")
	switch {
	case key == "get vms -A -o json":
		return d.vmListJSON(), nil
	case key == "get vmis -A -o json":
		return d.vmiListJSON(), nil
	case key == "get nodes -o json":
		return demoNodesJSON, nil
	case strings.HasPrefix(key, "get deploy -A -l cdi.kubevirt.io=cdi-operator"):
		return []byte("deployment.apps/cdi-operator"), nil
	case strings.HasPrefix(key, "get deploy -A -l kubevirt.io=virt-exportproxy"):
		return []byte("deployment.apps/virt-exportproxy"), nil
	case strings.HasPrefix(key, "get pods -A -l kubevirt.io=virt-launcher"):
		return d.launcherPodsJSON(), nil
	case strings.HasPrefix(key, "top pod -A -l kubevirt.io=virt-launcher"):
		return d.topLines(), nil
	case args[0] == "top" && strings.Contains(key, "-l kubevirt.io/vm="):
		// Per-VM live usage (Client.Metrics): "NAME CPU MEM" without -A.
		vmName := key[strings.Index(key, "kubevirt.io/vm=")+len("kubevirt.io/vm="):]
		vmName = strings.Fields(vmName)[0]
		if v := d.find(vmName, flagValue(args, "-n")); v != nil && v.Status == "Running" {
			return []byte(fmt.Sprintf("virt-launcher-%s %dm %s\n", v.Name, int(v.Load), strings.TrimSuffix(v.Mem, "i"))), nil
		}
		return []byte{}, nil
	case strings.HasPrefix(key, "get pvc -A -l corral.dev/ct=true"):
		return d.ctPVCsJSON(), nil
	case strings.HasPrefix(key, "get pods -A -l corral.dev/ct=true"):
		return d.ctPodsJSON(), nil
	case len(args) >= 2 && args[0] == "get" && (args[1] == "vm" || args[1] == "vms") && len(args) > 2 && !strings.HasPrefix(args[2], "-"):
		v := d.find(args[2], flagValue(args, "-n"))
		if v == nil {
			return nil, fmt.Errorf("virtualmachine %q not found", args[2])
		}
		if strings.Contains(key, "-o name") {
			return []byte("virtualmachine.kubevirt.io/" + v.Name), nil
		}
		return d.vmJSON(v), nil
	case strings.HasPrefix(key, "get kubevirt"):
		if hasJSONOutput(args) {
			return demoKubeVirtCRJSON, nil // doctor: gates, rollout, workload updates
		}
		return []byte("kubevirt"), nil
	case key == "get sc -o json" || key == "get storageclass -o json":
		return demoStorageClassJSON, nil
	case strings.HasPrefix(key, "get storageprofile"):
		return []byte(`{"status":{"cloneStrategy":"csi-clone","claimPropertySets":[{"accessModes":["ReadWriteMany","ReadWriteOnce"]}]}}`), nil
	case strings.HasPrefix(key, "get volumesnapshotclass"):
		if hasJSONOutput(args) {
			return []byte(`{"items":[{"metadata":{"name":"demo-snapclass"},"driver":"demo.csi.corral.dev"}]}`), nil
		}
		return []byte("volumesnapshotclass.snapshot.storage.k8s.io/demo-snapclass"), nil
	case len(args) >= 3 && args[0] == "delete" && args[1] == "vm":
		d.deleteVM(args[2], flagValue(args, "-n"))
		return []byte{}, nil
	case len(args) >= 3 && args[0] == "delete" && (args[1] == "pod" || args[1] == "pvc"):
		d.deleteCT(args[1], args[2], flagValue(args, "-n"))
		return []byte{}, nil
	case args[0] == "apply":
		d.applyManifest(stdin)
		return []byte{}, nil
	case args[0] == "label" && len(args) >= 3:
		d.applyTagLabel(args)
		return []byte{}, nil
	case args[0] == "get" && hasJSONOutput(args):
		// A named get ("get datavolume foo -o json") must 404 like kubectl
		// would — returning a list shape parses as a zero object and callers
		// misread it (e.g. DataVolumeStatus turning "" into "↓ importing").
		if len(args) > 2 && !strings.HasPrefix(args[2], "-") {
			return nil, fmt.Errorf("%s %q not found", args[1], args[2])
		}
		return []byte(`{"items":[]}`), nil
	case args[0] == "get" || args[0] == "top":
		return []byte{}, nil
	default:
		// patch / annotate / create / scale / wait / …: succeed silently.
		return []byte{}, nil
	}
}

func (d *demoCluster) virtctl(args []string) ([]byte, error) {
	v := d.find(args[1], flagValue(args, "-n"))
	if v == nil {
		return nil, fmt.Errorf("VM %q not found", args[1])
	}
	switch args[0] {
	case "start":
		v.Status = "Running"
		if v.IP == "" {
			v.IP = fmt.Sprintf("10.42.9.%d", 100+rand.Intn(100))
		}
		if v.Node == "" {
			v.Node = []string{"corral-1", "corral-2", "corral-3"}[rand.Intn(3)]
		}
		if v.Load == 0 {
			v.Load = 100 + float64(rand.Intn(400))
		}
	case "stop":
		v.Status = "Stopped"
	case "pause":
		v.Status = "Paused"
	case "unpause":
		v.Status = "Running"
	case "restart":
		v.Status = "Running"
	case "migrate":
		v.Status = "Migrating"
	}
	return []byte{}, nil
}

func (d *demoCluster) deleteVM(name, ns string) {
	for i, v := range d.vms {
		if v.Name == name && (ns == "" || v.NS == ns) {
			d.vms = append(d.vms[:i], d.vms[i+1:]...)
			return
		}
	}
}

func (d *demoCluster) deleteCT(kind, name, ns string) {
	// Deleting the pod stops the CT; deleting its PVC removes it.
	ctName := strings.TrimSuffix(strings.TrimPrefix(name, "ct-"), "-data")
	for i, c := range d.cts {
		if (c.Name == name || c.Name == ctName) && (ns == "" || c.NS == ns) {
			if kind == "pvc" {
				d.cts = append(d.cts[:i], d.cts[i+1:]...)
			} else {
				c.Running = false
				c.Node = ""
			}
			return
		}
	}
}

var (
	manifestKind = regexp.MustCompile(`"?kind"?\s*:\s*"?(\w+)"?`)
	manifestName = regexp.MustCompile(`"?name"?\s*:\s*"?([a-z0-9][a-z0-9-]*)"?`)
	manifestNS   = regexp.MustCompile(`"?namespace"?\s*:\s*"?([a-z0-9][a-z0-9-]*)"?`)
)

// applyManifest handles `kubectl apply -f -`: creating a VirtualMachine adds a
// VM to the fleet; a CT pod flips its CT to Running. Everything else is a
// silent success — enough for the create flows to land somewhere visible.
func (d *demoCluster) applyManifest(stdin string) {
	kind := ""
	if m := manifestKind.FindStringSubmatch(stdin); m != nil {
		kind = m[1]
	}
	nameM := manifestName.FindStringSubmatch(stdin)
	if nameM == nil {
		return
	}
	name, ns := nameM[1], "corral-vms"
	if m := manifestNS.FindStringSubmatch(stdin); m != nil {
		ns = m[1]
	}
	switch kind {
	case "VirtualMachine":
		if d.find(name, ns) == nil {
			d.vms = append(d.vms, &demoVM{
				Name: name, NS: ns, Status: "Running", CPU: 2, Mem: "4Gi",
				Node: "corral-1", IP: fmt.Sprintf("10.42.9.%d", 100+rand.Intn(100)),
				Load: 100 + float64(rand.Intn(300)),
			})
		}
	case "Pod":
		for _, c := range d.cts {
			if c.Name == name && c.NS == ns {
				c.Running = true
				c.Node = "corral-1"
				return
			}
		}
	case "PersistentVolumeClaim":
		if strings.Contains(stdin, "corral.dev/ct") {
			ctName := name
			if m := regexp.MustCompile(`corral.dev/ct-name"?\s*:\s*"?([a-z0-9-]+)`).FindStringSubmatch(stdin); m != nil {
				ctName = m[1]
			}
			for _, c := range d.cts {
				if c.Name == ctName && c.NS == ns {
					return
				}
			}
			d.cts = append(d.cts, &demoCT{Name: ctName, NS: ns, Image: "docker.io/library/debian:bookworm", CPU: 1, Mem: "512Mi"})
		}
	}
}

// applyTagLabel handles `kubectl label vm <name> corral.dev/tag.<t>=true|-`
// so the tag chips work in demo mode.
func (d *demoCluster) applyTagLabel(args []string) {
	v := d.find(args[2], flagValue(args, "-n"))
	if v == nil {
		return
	}
	for _, a := range args[3:] {
		if !strings.HasPrefix(a, "corral.dev/tag.") {
			continue
		}
		if tag, ok := strings.CutSuffix(a, "=true"); ok {
			t := strings.TrimPrefix(tag, "corral.dev/tag.")
			v.Tags = append(v.Tags, t)
		} else if t, ok := strings.CutSuffix(strings.TrimPrefix(a, "corral.dev/tag."), "-"); ok {
			out := v.Tags[:0]
			for _, x := range v.Tags {
				if x != t {
					out = append(out, x)
				}
			}
			v.Tags = out
		}
	}
}

// ── JSON builders ─────────────────────────────────────────────────

func (d *demoCluster) vmListJSON() []byte {
	items := make([]map[string]any, 0, len(d.vms))
	for _, v := range d.vms {
		items = append(items, d.vmItem(v))
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

func (d *demoCluster) vmJSON(v *demoVM) []byte {
	b, _ := json.Marshal(d.vmItem(v))
	return b
}

func (d *demoCluster) vmItem(v *demoVM) map[string]any {
	labels := map[string]string{}
	for _, t := range v.Tags {
		labels["corral.dev/tag."+t] = "true"
	}
	if v.Template {
		labels["corral.dev/template"] = "true"
	}
	if v.Ephemeral {
		labels["corral.dev/ephemeral"] = "true"
	}
	annotations := map[string]string{}
	if v.ExpiresAt != "" {
		annotations["corral.dev/expires-at"] = v.ExpiresAt
	}
	domain := map[string]any{
		"cpu":    map[string]any{"cores": v.CPU, "sockets": 1, "threads": 1},
		"memory": map[string]any{"guest": v.Mem},
		"devices": map[string]any{
			"disks": []map[string]any{
				{"name": "rootdisk", "disk": map[string]any{"bus": "virtio"}},
				{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}},
			},
		},
	}
	if v.Bootc {
		domain["firmware"] = map[string]any{"kernelBoot": map[string]any{"container": map[string]any{"image": "ghcr.io/tuna-os/demo:latest"}}}
	}
	spec := map[string]any{
		"domain": domain,
		"volumes": []map[string]any{
			{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": v.Name + "-root"}},
			{"name": "cloudinit", "cloudInitNoCloud": map[string]any{}},
		},
	}
	if v.Node != "" && !v.running() {
		spec["nodeSelector"] = map[string]string{"kubernetes.io/hostname": v.Node}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name": v.Name, "namespace": v.NS,
			"labels": labels, "annotations": annotations,
		},
		"spec": map[string]any{
			"runStrategy": "RerunOnFailure",
			"template":    map[string]any{"spec": spec},
		},
		"status": map[string]any{
			"ready":           v.Status == "Running",
			"printableStatus": v.Status,
		},
	}
}

func (d *demoCluster) vmiListJSON() []byte {
	items := []map[string]any{}
	for _, v := range d.vms {
		if !v.running() {
			continue
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": v.Name, "namespace": v.NS},
			"status": map[string]any{
				"nodeName":   v.Node,
				"phase":      "Running",
				"interfaces": []map[string]any{{"ipAddress": v.IP}},
				"conditions": []map[string]any{
					{"type": "LiveMigratable", "status": "True"},
					{"type": "AgentConnected", "status": "True"},
				},
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

func (d *demoCluster) launcherPodsJSON() []byte {
	items := []map[string]any{}
	for _, v := range d.vms {
		if !v.running() {
			continue
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":      "virt-launcher-" + v.Name,
				"namespace": v.NS,
				"labels":    map[string]string{"vm.kubevirt.io/name": v.Name},
			},
			"status": map[string]any{"phase": "Running"},
		})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

// topLines feeds the CPU sparkline: baseline load per VM plus a slow sine
// wobble and jitter, so the graph visibly moves between 5s polls.
func (d *demoCluster) topLines() []byte {
	t := time.Since(d.start).Seconds()
	var b strings.Builder
	for _, v := range d.vms {
		if !v.running() || v.Status == "Paused" {
			continue
		}
		milli := v.Load * (1 + 0.35*math.Sin(t/45+v.Load)) * (0.92 + 0.16*rand.Float64())
		fmt.Fprintf(&b, "%s virt-launcher-%s %dm %s\n", v.NS, v.Name, int(milli), v.Mem)
	}
	return []byte(b.String())
}

func (d *demoCluster) ctPVCsJSON() []byte {
	items := []map[string]any{}
	for _, c := range d.cts {
		spec, _ := json.Marshal(map[string]any{
			"image": c.Image, "cpu": c.CPU, "mem": c.Mem, "privileged": c.Privileged,
		})
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name": "ct-" + c.Name + "-data", "namespace": c.NS,
				"labels":      map[string]string{"corral.dev/ct": "true", "corral.dev/ct-name": c.Name},
				"annotations": map[string]string{"corral.ct/spec": string(spec)},
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

func (d *demoCluster) ctPodsJSON() []byte {
	items := []map[string]any{}
	for _, c := range d.cts {
		if !c.Running {
			continue
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": c.Name, "namespace": c.NS},
			"spec":     map[string]any{"nodeName": c.Node},
			"status": map[string]any{
				"phase":             "Running",
				"containerStatuses": []map[string]any{{"ready": true}},
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

// demoKubeVirtCRJSON makes `corral doctor` (and the Cluster health page)
// report a healthy, fully-featured cluster — every gate the UI can render
// as green is on, so the healthy state is exercisable too.
var demoKubeVirtCRJSON = []byte(`{
  "spec": {
    "configuration": {
      "vmRolloutStrategy": "LiveUpdate",
      "developerConfiguration": {"featureGates": ["Snapshot", "HotplugVolumes", "VMExport", "ExpandDisks"]}
    },
    "workloadUpdateStrategy": {"workloadUpdateMethods": ["LiveMigrate"]}
  },
  "status": {"phase": "Deployed"}
}`)

var demoStorageClassJSON = []byte(`{"items": [{
  "metadata": {
    "name": "demo-nvme",
    "annotations": {"storageclass.kubernetes.io/is-default-class": "true"}
  },
  "provisioner": "demo.csi.corral.dev",
  "allowVolumeExpansion": true
}]}`)

var demoNodesJSON = func() []byte {
	node := func(name, role string) map[string]any {
		labels := map[string]string{"kubernetes.io/hostname": name}
		if role != "" {
			labels["node-role.kubernetes.io/"+role] = "true"
		}
		return map[string]any{
			"metadata": map[string]any{"name": name, "labels": labels},
			"status": map[string]any{
				"nodeInfo":   map[string]any{"kubeletVersion": "v1.33.2", "architecture": "amd64"},
				"conditions": []map[string]any{{"type": "Ready", "status": "True"}},
			},
		}
	}
	b, _ := json.Marshal(map[string]any{"items": []map[string]any{
		node("corral-1", "control-plane"),
		node("corral-2", ""),
		node("corral-3", ""),
	}})
	return b
}()
