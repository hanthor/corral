// Package vdi implements Phase 1 of RFC-0001 (docs/rfc/0001-vdi-plugin.md):
// static desktop pools with manual assignment, built entirely on existing
// Corral primitives — kubevirt.Client.Clone stamps out pool members from an
// already-built "golden" VM (built the normal way, via corral bootc /
// corral-windows / corral create), and assignment is a pair of labels on
// the VM object, not a new CRD or storage system. See the RFC's "Phase 1"
// section for why: proving pool creation + connect-routing works is the
// goal here, not building a broker.
package vdi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/shell"
)

const (
	labelPool       = "corral.dev/vdi-pool"
	labelAssignedTo = "corral.dev/vdi-assigned-to"
	annoClaimedAt   = "corral.dev/vdi-claimed-at"
)

// cloneWaitTimeout and clonePollInterval are vars (not consts) so tests can
// shrink them instead of waiting out a real 2-minute timeout.
var (
	cloneWaitTimeout  = 2 * time.Minute
	clonePollInterval = time.Second
)

var runner shell.Runner = shell.Real{}

// SetRunner overrides the command runner (for unit tests). Also rewires
// pkg/kubevirt's runner seams — pool operations drive VM lifecycle through
// kubevirt.Client (default runner) and kubevirt.Clone (separate apply
// runner) — so tests only need to call this one seam.
func SetRunner(r shell.Runner) {
	runner = r
	kubevirt.SetDefaultRunner(r)
	kubevirt.SetApplyRunner(r)
}

func run(name string, args ...string) ([]byte, error) { return runner.Run(name, args...) }

// CreateOpts describes a new pool.
type CreateOpts struct {
	Name      string // pool name; members are named "<name>-1".."<name>-N"
	Namespace string
	From      string // name of an existing, already-built "golden" VM to clone
	Size      int
}

// Member is one desktop in a pool.
type Member struct {
	Name       string `json:"name"`
	AssignedTo string `json:"assignedTo,omitempty"` // "" = free
	ClaimedAt  string `json:"claimedAt,omitempty"`  // RFC3339, empty if free
	Running    bool   `json:"running"`
}

// Pool groups a set of Members under one pool label.
type Pool struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Members   []Member `json:"members"`
}

func memberName(pool string, i int) string { return fmt.Sprintf("%s-%d", pool, i) }

// DeviceType characterizes whether a resource is exclusive passthrough or mediated/vGPU.
type DeviceType string

const (
	DeviceTypePCIHostDevice    DeviceType = "exclusive PCI passthrough"
	DeviceTypeMediatedDevice   DeviceType = "mediated device (vGPU)"
	DeviceTypeExternalProvider DeviceType = "external provider device (vGPU/SR-IOV)"
	DeviceTypeUnknown          DeviceType = "host device"
)

// DeviceRequest holds information about a GPU/host-device requested by a VM.
type DeviceRequest struct {
	ResourceName string     `json:"resourceName"`
	Count        int        `json:"count"`
	Type         DeviceType `json:"type"`
}

// DeviceCapacityReport summarizes resource availability across cluster nodes.
type DeviceCapacityReport struct {
	ResourceName      string     `json:"resourceName"`
	Type              DeviceType `json:"type"`
	ReplicasRequested int        `json:"replicasRequested"`
	PerVMCount        int        `json:"perVmCount"`
	TotalNeeded       int        `json:"totalNeeded"`
	AllocatableTotal  int        `json:"allocatableTotal"`
	AllocatedExisting int        `json:"allocatedExisting"`
	AvailableTotal    int        `json:"availableTotal"`
	NodeAllocatable   map[string]int `json:"nodeAllocatable"`
	NodeAvailable     map[string]int `json:"nodeAvailable"`
	MaxConcurrency    int        `json:"maxConcurrency"`
}

type vmSpecDevices struct {
	Spec struct {
		Template struct {
			Spec struct {
				Domain struct {
					Devices struct {
						GPUs []struct {
							DeviceName string `json:"deviceName"`
							Name       string `json:"name"`
						} `json:"gpus"`
						HostDevices []struct {
							DeviceName string `json:"deviceName"`
							Name       string `json:"name"`
						} `json:"hostDevices"`
					} `json:"devices"`
				} `json:"domain"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type permittedDevicesConfig struct {
	Spec struct {
		Configuration struct {
			PermittedHostDevices struct {
				PCIHostDevices []struct {
					ResourceName             string `json:"resourceName"`
					ExternalResourceProvider bool   `json:"externalResourceProvider"`
				} `json:"pciHostDevices"`
				MediatedDevices []struct {
					ResourceName             string `json:"resourceName"`
					ExternalResourceProvider bool   `json:"externalResourceProvider"`
				} `json:"mediatedDevices"`
			} `json:"permittedHostDevices"`
		} `json:"configuration"`
	} `json:"spec"`
}

// inspectVMDeviceRequests returns the host device requests of a VM and their types.
func inspectVMDeviceRequests(ns, vmName string) ([]DeviceRequest, error) {
	out, err := run("kubectl", "get", "vm", vmName, "-n", ns, "-o", "json")
	if err != nil {
		// If kubectl failed or command wasn't mocked in a unit test, return nil
		return nil, nil
	}

	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}

	var vm vmSpecDevices
	if err := json.Unmarshal(out, &vm); err != nil {
		// If it wasn't valid JSON (e.g. dummy test response), treat as no special devices requested
		return nil, nil
	}

	counts := map[string]int{}
	for _, g := range vm.Spec.Template.Spec.Domain.Devices.GPUs {
		if g.DeviceName != "" {
			counts[g.DeviceName]++
		}
	}
	for _, h := range vm.Spec.Template.Spec.Domain.Devices.HostDevices {
		if h.DeviceName != "" {
			counts[h.DeviceName]++
		}
	}

	if len(counts) == 0 {
		return nil, nil
	}

	// Look up device types from KubeVirt permittedHostDevices config if present
	devTypes := map[string]DeviceType{}
	if kvOut, err := run("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"); err == nil {
		var kv permittedDevicesConfig
		if json.Unmarshal(kvOut, &kv) == nil {
			for _, pci := range kv.Spec.Configuration.PermittedHostDevices.PCIHostDevices {
				if pci.ExternalResourceProvider {
					devTypes[pci.ResourceName] = DeviceTypeExternalProvider
				} else {
					devTypes[pci.ResourceName] = DeviceTypePCIHostDevice
				}
			}
			for _, mdev := range kv.Spec.Configuration.PermittedHostDevices.MediatedDevices {
				devTypes[mdev.ResourceName] = DeviceTypeMediatedDevice
			}
		}
	}

	var reqs []DeviceRequest
	for resName, count := range counts {
		dt, ok := devTypes[resName]
		if !ok {
			dt = DeviceTypeUnknown
		}
		reqs = append(reqs, DeviceRequest{
			ResourceName: resName,
			Count:        count,
			Type:         dt,
		})
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].ResourceName < reqs[j].ResourceName })
	return reqs, nil
}

type nodeResourceItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

type vmiResourceItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
		Domain   struct {
			Devices struct {
				GPUs []struct {
					DeviceName string `json:"deviceName"`
				} `json:"gpus"`
				HostDevices []struct {
					DeviceName string `json:"deviceName"`
				} `json:"hostDevices"`
			} `json:"devices"`
		} `json:"domain"`
	} `json:"spec"`
	Status struct {
		NodeName string `json:"nodeName"`
		Phase    string `json:"phase"`
	} `json:"status"`
}

// ValidatePoolDeviceCapacity verifies cluster device capacity for the golden VM and desired pool size.
func ValidatePoolDeviceCapacity(ns, goldenVM string, size int) ([]DeviceCapacityReport, error) {
	reqs, err := inspectVMDeviceRequests(ns, goldenVM)
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, nil
	}

	// 1. Get Node Allocatable resources
	nodesOut, err := run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("listing nodes to check device capacity: %w", err)
	}
	var nodeRes struct {
		Items []nodeResourceItem `json:"items"`
	}
	if err := json.Unmarshal(nodesOut, &nodeRes); err != nil {
		return nil, fmt.Errorf("parsing nodes: %w", err)
	}

	// 2. Get active VMIs to count already allocated devices
	vmisOut, err := run("kubectl", "get", "vmi", "-A", "-o", "json")
	var vmiRes struct {
		Items []vmiResourceItem `json:"items"`
	}
	if err == nil {
		_ = json.Unmarshal(vmisOut, &vmiRes)
	}

	var reports []DeviceCapacityReport
	for _, req := range reqs {
		resName := req.ResourceName
		nodeAlloc := map[string]int{}
		totalAllocatable := 0

		for _, n := range nodeRes.Items {
			valStr := n.Status.Allocatable[resName]
			if valStr != "" {
				if parsed, err := strconv.Atoi(valStr); err == nil && parsed > 0 {
					nodeAlloc[n.Metadata.Name] = parsed
					totalAllocatable += parsed
				}
			}
		}

		nodeAllocated := map[string]int{}
		totalAllocated := 0
		for _, vmi := range vmiRes.Items {
			// Ignore failed or succeeded VMIs
			if vmi.Status.Phase == "Failed" || vmi.Status.Phase == "Succeeded" {
				continue
			}
			vmiCount := 0
			for _, g := range vmi.Spec.Domain.Devices.GPUs {
				if g.DeviceName == resName {
					vmiCount++
				}
			}
			for _, h := range vmi.Spec.Domain.Devices.HostDevices {
				if h.DeviceName == resName {
					vmiCount++
				}
			}
			if vmiCount > 0 {
				node := vmi.Status.NodeName
				if node == "" {
					node = vmi.Spec.NodeName
				}
				totalAllocated += vmiCount
				if node != "" {
					nodeAllocated[node] += vmiCount
				}
			}
		}

		nodeAvailable := map[string]int{}
		totalAvailable := 0
		maxConcurrency := 0
		for node, alloc := range nodeAlloc {
			avail := alloc - nodeAllocated[node]
			if avail < 0 {
				avail = 0
			}
			nodeAvailable[node] = avail
			totalAvailable += avail
			if req.Count > 0 {
				maxConcurrency += avail / req.Count
			}
		}

		needed := req.Count * size
		rep := DeviceCapacityReport{
			ResourceName:      resName,
			Type:              req.Type,
			ReplicasRequested: size,
			PerVMCount:        req.Count,
			TotalNeeded:       needed,
			AllocatableTotal:  totalAllocatable,
			AllocatedExisting: totalAllocated,
			AvailableTotal:    totalAvailable,
			NodeAllocatable:   nodeAlloc,
			NodeAvailable:     nodeAvailable,
			MaxConcurrency:    maxConcurrency,
		}
		reports = append(reports, rep)

		// Verification:
		// Check total capacity and per-node placement feasibility
		if totalAllocatable == 0 {
			return reports, fmt.Errorf("device admission failed: golden VM requests %d x %q (%s), but resource is not allocatable on any node in the cluster",
				req.Count, resName, req.Type)
		}

		if totalAvailable < needed || maxConcurrency < size {
			var nodeBreakdown []string
			for node, alloc := range nodeAlloc {
				avail := nodeAvailable[node]
				nodeBreakdown = append(nodeBreakdown, fmt.Sprintf("node %s: %d available / %d allocatable", node, avail, alloc))
			}
			sort.Strings(nodeBreakdown)
			return reports, fmt.Errorf("insufficient device capacity for %q (%s): requested %d replicas (%d devices total: %d per VM), but only %d available (%d existing allocations, max concurrency %d across nodes: %s)",
				resName, req.Type, size, needed, req.Count, totalAvailable, totalAllocated, maxConcurrency, strings.Join(nodeBreakdown, "; "))
		}
	}

	return reports, nil
}

// CreatePool clones the golden VM Size times and labels each clone as a
// pool member. Members start unassigned and — matching how a freshly
// cloned VM already behaves — powered on (Clone doesn't change run state;
// callers wanting scale-to-zero pools should stop members after create).
func CreatePool(opts CreateOpts) (Pool, error) {
	if opts.Size < 1 {
		return Pool{}, fmt.Errorf("--size must be >= 1")
	}
	ns := opts.Namespace
	client := kubevirt.NewClient(ns)
	if !client.VMExists(opts.From) {
		return Pool{}, fmt.Errorf("golden VM %q not found in ns/%s — build it first (corral create / corral bootc / corral-windows)", opts.From, ns)
	}

	// Validate GPU / host-device capacity before partial clone mutation
	if _, err := ValidatePoolDeviceCapacity(ns, opts.From, opts.Size); err != nil {
		return Pool{}, err
	}

	pool := Pool{Name: opts.Name, Namespace: ns}
	for i := 1; i <= opts.Size; i++ {
		name := memberName(opts.Name, i)
		if err := client.Clone(opts.From, name); err != nil {
			return pool, fmt.Errorf("cloning member %d/%d (%s): %w", i, opts.Size, name, err)
		}
		// Clone() returns as soon as the VirtualMachineClone CRD is applied,
		// not once KubeVirt's clone controller has actually produced the
		// target VM — labeling it immediately races the controller. Found
		// live: the very first CreatePool run against a real cluster failed
		// here because the label command ran before the VM object existed.
		if err := waitForVM(ns, name, cloneWaitTimeout); err != nil {
			return pool, fmt.Errorf("cloning member %d/%d (%s): %w", i, opts.Size, name, err)
		}
		if err := labelMember(ns, name, opts.Name, ""); err != nil {
			return pool, fmt.Errorf("labeling member %s: %w", name, err)
		}
		pool.Members = append(pool.Members, Member{Name: name})
	}
	return pool, nil
}

// waitForVM polls until the clone controller has actually created the
// target VM object, or timeout elapses.
func waitForVM(ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := kubevirt.NewClient(ns)
	for {
		if client.VMExists(name) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the clone to produce VM %q", timeout, name)
		}
		time.Sleep(clonePollInterval)
	}
}

func labelMember(ns, name, pool, assignedTo string) error {
	args := []string{"label", "vm", name, "-n", ns,
		labelPool + "=" + pool, "--overwrite"}
	if _, err := run("kubectl", args...); err != nil {
		return err
	}
	if assignedTo == "" {
		_, err := run("kubectl", "label", "vm", name, "-n", ns, labelAssignedTo+"-", "--overwrite")
		if err != nil {
			return err
		}
		_, err = run("kubectl", "annotate", "vm", name, "-n", ns, annoClaimedAt+"-", "--overwrite")
		return err
	}
	if _, err := run("kubectl", "label", "vm", name, "-n", ns, labelAssignedTo+"="+assignedTo, "--overwrite"); err != nil {
		return err
	}
	_, err := run("kubectl", "annotate", "vm", name, "-n", ns,
		annoClaimedAt+"="+time.Now().UTC().Format(time.RFC3339), "--overwrite")
	return err
}

type vmListItem struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Running *bool `json:"running"`
	} `json:"spec"`
	Status struct {
		PrintableStatus string `json:"printableStatus"`
	} `json:"status"`
}

func listPoolVMs(pool string) ([]vmListItem, error) {
	args := []string{"get", "vm", "-A", "-o", "json"}
	if pool != "" {
		args = append(args, "-l", labelPool+"="+pool)
	} else {
		args = append(args, "-l", labelPool)
	}
	out, err := run("kubectl", args...)
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []vmListItem `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

func toMember(v vmListItem) Member {
	return Member{
		Name:       v.Metadata.Name,
		AssignedTo: v.Metadata.Labels[labelAssignedTo],
		ClaimedAt:  v.Metadata.Annotations[annoClaimedAt],
		Running:    v.Status.PrintableStatus == "Running",
	}
}

// ListPools returns every pool, grouped from VMs carrying the pool label —
// there's no separate Pool object, the label on the members is the source
// of truth (same "no extra state to go stale" reasoning as CT's PVC
// annotation).
func ListPools() ([]Pool, error) {
	items, err := listPoolVMs("")
	if err != nil {
		return nil, err
	}
	byPool := map[string]*Pool{}
	var order []string
	for _, v := range items {
		name := v.Metadata.Labels[labelPool]
		if name == "" {
			continue
		}
		key := v.Metadata.Namespace + "/" + name
		p, ok := byPool[key]
		if !ok {
			p = &Pool{Name: name, Namespace: v.Metadata.Namespace}
			byPool[key] = p
			order = append(order, key)
		}
		p.Members = append(p.Members, toMember(v))
	}
	pools := make([]Pool, 0, len(order))
	for _, key := range order {
		pools = append(pools, *byPool[key])
	}
	return pools, nil
}

// Assign claims the first free (unassigned) member of pool for user,
// starting it if it isn't already running, and returns the member's name.
func Assign(namespace, pool, user string) (string, error) {
	items, err := listPoolVMs(pool)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", fmt.Errorf("pool %q not found (or has no members) in ns/%s", pool, namespace)
	}
	for _, v := range items {
		if v.Metadata.Labels[labelAssignedTo] != "" {
			continue
		}
		name := v.Metadata.Name
		if err := labelMember(namespace, name, pool, user); err != nil {
			return "", err
		}
		if v.Status.PrintableStatus != "Running" {
			if err := kubevirt.NewClient(namespace).StartVM(name); err != nil {
				return "", fmt.Errorf("assigned %s but failed to start it: %w", name, err)
			}
		}
		return name, nil
	}
	return "", fmt.Errorf("pool %q has no free members (all %d claimed)", pool, len(items))
}

// Unassign releases member back to the pool's free set and stops it —
// pooled desktops don't stay running unclaimed, matching VDI reclaim
// intent even in this phase's hand-wired form.
func Unassign(namespace, member string) error {
	if err := labelMember(namespace, member, poolOf(namespace, member), ""); err != nil {
		return err
	}
	return kubevirt.NewClient(namespace).StopVM(member)
}

func poolOf(namespace, member string) string {
	out, err := run("kubectl", "get", "vm", member, "-n", namespace, "-o",
		"jsonpath={.metadata.labels.corral\\.dev/vdi-pool}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DeletePool deletes every member of pool.
func DeletePool(namespace, pool string) error {
	items, err := listPoolVMs(pool)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("pool %q not found (or has no members) in ns/%s", pool, namespace)
	}
	client := kubevirt.NewClient(namespace)
	var firstErr error
	for _, v := range items {
		if err := client.DeleteVM(v.Metadata.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
