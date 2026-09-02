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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/shell"
)

const (
	labelPool       = "corral.dev/vdi-pool"
	labelAssignedTo = "corral.dev/vdi-assigned-to"
	annoClaimedAt   = "corral.dev/vdi-claimed-at"
	leasePrefix     = "corral-vdi-"
	claimDuration   = time.Hour
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
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
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

type lease struct {
	Metadata struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity       string `json:"holderIdentity"`
		AcquireTime          string `json:"acquireTime"`
		RenewTime            string `json:"renewTime"`
		LeaseDurationSeconds int32  `json:"leaseDurationSeconds"`
	} `json:"spec"`
}

func leaseName(member string) string { return leasePrefix + member }

func leaseExpired(l lease, now time.Time) bool {
	if l.Spec.HolderIdentity == "" || l.Spec.RenewTime == "" || l.Spec.LeaseDurationSeconds <= 0 {
		return true
	}
	renewed, err := time.Parse(time.RFC3339Nano, l.Spec.RenewTime)
	return err != nil || now.After(renewed.Add(time.Duration(l.Spec.LeaseDurationSeconds)*time.Second))
}

func leaseManifest(namespace, pool, member, identity string, now time.Time, resourceVersion string) []byte {
	metadata := map[string]any{"name": leaseName(member), "namespace": namespace, "labels": map[string]string{labelPool: pool, "corral.dev/vdi-member": member}}
	if resourceVersion != "" {
		metadata["resourceVersion"] = resourceVersion
	}
	data, err := json.Marshal(map[string]any{
		"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": metadata,
		"spec": map[string]any{
			"holderIdentity": identity, "acquireTime": now.Format(time.RFC3339Nano),
			"renewTime": now.Format(time.RFC3339Nano), "leaseDurationSeconds": int32(claimDuration.Seconds()),
		},
	})
	if err != nil {
		panic(err)
	}
	return data
}

// acquireLease uses Lease creation as a first-writer-wins primitive. An
// expired lease is replaced with its resourceVersion, making recovery a
// compare-and-swap instead of another list-then-label race.
func acquireLease(namespace, pool, member, identity string) (bool, error) {
	now := time.Now().UTC()
	if _, err := runner.RunStdin(string(leaseManifest(namespace, pool, member, identity, now, "")), "kubectl", "create", "-f", "-"); err == nil {
		return true, nil
	} else {
		out, getErr := run("kubectl", "get", "lease", leaseName(member), "-n", namespace, "-o", "json")
		if getErr != nil {
			return false, fmt.Errorf("claim %s: lease create failed (%v); read failed: %w", member, err, getErr)
		}
		var current lease
		if decodeErr := json.Unmarshal(out, &current); decodeErr != nil {
			return false, fmt.Errorf("claim %s: decode lease: %w", member, decodeErr)
		}
		if current.Spec.HolderIdentity == identity && !leaseExpired(current, now) {
			return true, nil
		}
		if !leaseExpired(current, now) {
			return false, nil
		}
		if _, replaceErr := runner.RunStdin(string(leaseManifest(namespace, pool, member, identity, now, current.Metadata.ResourceVersion)), "kubectl", "replace", "-f", "-"); replaceErr != nil {
			return false, nil
		}
		return true, nil
	}
}

func readLease(namespace, member string) (lease, error) {
	out, err := run("kubectl", "get", "lease", leaseName(member), "-n", namespace, "-o", "json")
	if err != nil {
		return lease{}, err
	}
	var l lease
	if err := json.Unmarshal(out, &l); err != nil {
		return lease{}, fmt.Errorf("decode claim lease: %w", err)
	}
	return l, nil
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

// Claim atomically acquires one pool member for identity using a Kubernetes
// Lease. Labels and annotations are updated only after the Lease is held and
// remain presentation state for Phase 1 clients.
func Claim(namespace, pool, identity string) (string, error) {
	if strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("claim identity must not be empty")
	}
	items, err := listPoolVMs(pool)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", fmt.Errorf("pool %q not found (or has no members) in ns/%s", pool, namespace)
	}
	for _, v := range items {
		assignedTo := v.Metadata.Labels[labelAssignedTo]
		if assignedTo != "" && assignedTo != identity {
			// Legacy Phase 1 labels have no ownership primitive and must not
			// be stolen. A member with an expired Lease, however, is reclaimable.
			current, leaseErr := readLease(namespace, v.Metadata.Name)
			if leaseErr != nil || !leaseExpired(current, time.Now().UTC()) {
				continue
			}
		}
		name := v.Metadata.Name
		acquired, claimErr := acquireLease(namespace, pool, name, identity)
		if claimErr != nil {
			return "", claimErr
		}
		if !acquired {
			continue
		}
		if err := labelMember(namespace, name, pool, identity); err != nil {
			return "", fmt.Errorf("claimed %s but failed to update presentation labels; release lease %s: %w", name, leaseName(name), err)
		}
		if v.Status.PrintableStatus != "Running" {
			if err := kubevirt.NewClient(namespace).StartVM(name); err != nil {
				return "", fmt.Errorf("claimed %s but failed to start it: %w", name, err)
			}
		}
		return name, nil
	}
	return "", fmt.Errorf("pool %q has no free members (all %d claimed or contended)", pool, len(items))
}

// Assign is the Phase 1 compatibility name for Claim.
func Assign(namespace, pool, user string) (string, error) { return Claim(namespace, pool, user) }

// Release releases a member only when identity owns its Lease. Deleting the
// Lease is the atomic ownership transition; labels are then cleared as
// presentation state. An empty identity is accepted for an administrative
// compatibility release of legacy label-only assignments.
func Release(namespace, member, identity string) error {
	l, err := readLease(namespace, member)
	if err != nil && identity != "" {
		return fmt.Errorf("read claim for %s: %w", member, err)
	}
	if err == nil && identity != "" && l.Spec.HolderIdentity != identity {
		return fmt.Errorf("member %s is claimed by %q, not %q", member, l.Spec.HolderIdentity, identity)
	}
	if err := labelMember(namespace, member, poolOf(namespace, member), ""); err != nil {
		return fmt.Errorf("release %s: clear presentation labels: %w", member, err)
	}
	if err == nil {
		if _, deleteErr := run("kubectl", "delete", "lease", leaseName(member), "-n", namespace, "--resource-version", l.Metadata.ResourceVersion, "--ignore-not-found"); deleteErr != nil {
			return fmt.Errorf("release %s: delete claim lease: %w", member, deleteErr)
		}
	}
	if err := kubevirt.NewClient(namespace).StopVM(member); err != nil {
		return err
	}
	return nil
}

// Unassign preserves the original administrative API.
func Unassign(namespace, member string) error { return Release(namespace, member, "") }

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

// USBDevice describes a local USB device available for redirection.
type USBDevice struct {
	BusNum      string `json:"busNum"`
	DevNum      string `json:"devNum"`
	VendorID    string `json:"vendorId"`
	ProductID   string `json:"productId"`
	Description string `json:"description"`
	Driver      string `json:"driver,omitempty"`
	Busy        bool   `json:"busy,omitempty"`
	BusyReason  string `json:"busyReason,omitempty"`
}

// FormatSelector returns the vendorID:productID or bus.dev selector.
func (d USBDevice) Selector() string {
	if d.VendorID != "" && d.ProductID != "" {
		return fmt.Sprintf("%s:%s", d.VendorID, d.ProductID)
	}
	return fmt.Sprintf("%s.%s", d.BusNum, d.DevNum)
}

// USBConsequence explains what USB redirection entails.
type USBConsequence string

const (
	USBConsequenceExclusive   USBConsequence = "the local host loses access to the device while redirection is active"
	USBConsequenceMigration   USBConsequence = "live migration is blocked while a host device is redirected"
	USBConsequenceTermination USBConsequence = "disconnecting the session will drop guest connection to the device"
	USBConsequenceTrust       USBConsequence = "the assigned guest receives raw USB access to the device"
)

// USBConsequences returns the consequences of redirecting dev.
func USBConsequences(dev USBDevice) []USBConsequence {
	return []USBConsequence{
		USBConsequenceExclusive,
		USBConsequenceMigration,
		USBConsequenceTermination,
		USBConsequenceTrust,
	}
}

// ListUSBDevicesFunc is the sysfs scanner or lsusb runner seam for testing.
var ListUSBDevicesFunc = listUSBDevicesDefault

func listUSBDevicesDefault() ([]USBDevice, error) {
	// Discover via /sys/bus/usb/devices
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return nil, fmt.Errorf("reading USB devices from sysfs: %w", err)
	}

	var devs []USBDevice
	for _, entry := range entries {
		devPath := filepath.Join("/sys/bus/usb/devices", entry.Name())
		idVendorBytes, err := os.ReadFile(filepath.Join(devPath, "idVendor"))
		if err != nil {
			continue
		}
		idProductBytes, err := os.ReadFile(filepath.Join(devPath, "idProduct"))
		if err != nil {
			continue
		}
		vendorID := strings.TrimSpace(string(idVendorBytes))
		productID := strings.TrimSpace(string(idProductBytes))
		if vendorID == "" || productID == "" {
			continue
		}

		busnumBytes, _ := os.ReadFile(filepath.Join(devPath, "busnum"))
		devnumBytes, _ := os.ReadFile(filepath.Join(devPath, "devnum"))
		busnum := strings.TrimSpace(string(busnumBytes))
		devnum := strings.TrimSpace(string(devnumBytes))

		productBytes, _ := os.ReadFile(filepath.Join(devPath, "product"))
		manufacturerBytes, _ := os.ReadFile(filepath.Join(devPath, "manufacturer"))
		product := strings.TrimSpace(string(productBytes))
		manufacturer := strings.TrimSpace(string(manufacturerBytes))

		desc := strings.TrimSpace(manufacturer + " " + product)
		if desc == "" {
			desc = fmt.Sprintf("USB Device %s:%s", vendorID, productID)
		}

		dev := USBDevice{
			BusNum:      busnum,
			DevNum:      devnum,
			VendorID:    vendorID,
			ProductID:   productID,
			Description: desc,
		}

		// Check if it looks busy or is a root hub
		if strings.HasPrefix(entry.Name(), "usb") || desc == "Linux Foundation root hub" {
			dev.Busy = true
			dev.BusyReason = "USB root hub cannot be redirected"
		}

		devs = append(devs, dev)
	}

	return devs, nil
}

// ListLocalUSBDevices returns local USB devices discovered on the host.
func ListLocalUSBDevices() ([]USBDevice, error) {
	return ListUSBDevicesFunc()
}

// USBRedirOpts holds parameters for redirecting a USB device to an assigned desktop.
type USBRedirOpts struct {
	Namespace string
	Member    string
	Device    string // vendorID:productID or bus.dev
	User      string // expected caller identity for ownership check ("" skips check)
	Context   string
}

// ValidateUSBRedir verifies assignment, running state, and authorization for member.
func ValidateUSBRedir(opts USBRedirOpts) (Member, error) {
	ns := opts.Namespace
	member := opts.Member
	if member == "" {
		return Member{}, fmt.Errorf("member name is required")
	}

	pool := poolOf(ns, member)
	if pool == "" {
		return Member{}, fmt.Errorf("member %q is not part of a VDI pool in ns/%s", member, ns)
	}

	items, err := listPoolVMs(pool)
	if err != nil {
		return Member{}, fmt.Errorf("querying pool %q: %w", pool, err)
	}

	var targetItem *vmListItem
	for _, it := range items {
		if it.Metadata.Name == member {
			targetItem = &it
			break
		}
	}

	if targetItem == nil {
		return Member{}, fmt.Errorf("member %q not found in pool %q (ns/%s)", member, pool, ns)
	}

	m := toMember(*targetItem)
	if m.AssignedTo == "" {
		return m, fmt.Errorf("member %q is not assigned to any user (claim/assign first with `corral vdi assign`)", member)
	}

	if opts.User != "" && m.AssignedTo != opts.User {
		return m, fmt.Errorf("authorization failed: member %q is assigned to %q, not %q", member, m.AssignedTo, opts.User)
	}

	if !m.Running {
		return m, fmt.Errorf("member %q is stopped; start it before redirecting USB devices", member)
	}

	return m, nil
}

// BuildVirtctlUSBCmd constructs the virtctl usbredir command.
func BuildVirtctlUSBCmd(virtctlPath, member, namespace, deviceSelector, kubeContext string) *exec.Cmd {
	args := []string{"usbredir", member, "-n", namespace}
	if deviceSelector != "" {
		args = append(args, "--device="+deviceSelector)
	}
	return shell.CommandForContext(kubeContext, virtctlPath, args...)
}

