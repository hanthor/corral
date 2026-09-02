package vdi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/shell"
)

const (
	ctPoolLabel       = "corral.dev/ct-pool"
	ctMemberLabel     = "corral.dev/ct-member"
	ctAssignedLabel   = "corral.dev/vdi-assigned-to"
	ctPoolConfigLabel = "corral.dev/ct-pool-config"
)

// CTPoolSpec is the declarative, cluster-visible source of truth for an
// ephemeral CT pool. It is stored in a ConfigMap; member PVCs carry the pool
// and member labels for discovery and presentation.
type CTPoolSpec struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Image        string `json:"image"`
	Size         int    `json:"size"`
	CPU          int    `json:"cpu,omitempty"`
	Mem          string `json:"mem,omitempty"`
	Disk         string `json:"disk,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	Privileged   bool   `json:"privileged,omitempty"`
	Init         bool   `json:"init,omitempty"`
	Ephemeral    bool   `json:"ephemeral"`
}

type CTPoolStatus struct {
	Pool       string    `json:"pool"`
	Desired    int       `json:"desired"`
	Ready      int       `json:"ready"`
	Created    []string  `json:"created,omitempty"`
	Removed    []string  `json:"removed,omitempty"`
	Protected  []string  `json:"protected,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

type ctPoolPVC struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
}

var ctPoolRunner shell.Runner = shell.DefaultKubectl

// SetCTPoolRunner overrides the ConfigMap/PVC command seam for tests.
func SetCTPoolRunner(r shell.Runner)                        { ctPoolRunner = r }
func ctPoolRun(name string, args ...string) ([]byte, error) { return ctPoolRunner.Run(name, args...) }
func ctPoolApply(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = ctPoolRunner.RunStdin(string(data), "kubectl", "apply", "-f", "-")
	return err
}

func poolConfigName(name string) string           { return "ct-pool-" + name }
func memberNameForPool(pool string, i int) string { return fmt.Sprintf("%s-%d", pool, i) }

func poolConfig(spec CTPoolSpec, status CTPoolStatus) map[string]any {
	specData, _ := json.Marshal(spec)
	statusData, _ := json.Marshal(status)
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{
		"name": poolConfigName(spec.Name), "namespace": spec.Namespace, "labels": map[string]string{ctPoolConfigLabel: "true"},
	}, "data": map[string]string{"spec": string(specData), "status": string(statusData)}}
}

func listPoolPVCs(spec CTPoolSpec) ([]ctPoolPVC, error) {
	out, err := ctPoolRun("kubectl", "get", "pvc", "-n", spec.Namespace, "-l", ctPoolLabel+"="+spec.Name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var response struct {
		Items []ctPoolPVC `json:"items"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, fmt.Errorf("decode CT pool PVCs: %w", err)
	}
	return response.Items, nil
}

func labelCTMember(spec CTPoolSpec, member string) error {
	_, err := ctPoolRun("kubectl", "label", "pvc", member+"-data", "-n", spec.Namespace,
		ctPoolLabel+"="+spec.Name, ctMemberLabel+"="+member, "--overwrite")
	return err
}

func assignCTMember(spec CTPoolSpec, member, identity string) error {
	_, err := ctPoolRun("kubectl", "label", "pvc", member+"-data", "-n", spec.Namespace,
		ctPoolLabel+"="+spec.Name, ctMemberLabel+"="+member, ctAssignedLabel+"="+identity, "--overwrite")
	return err
}

// ClaimCTMember atomically claims an available CT pool member and mirrors
// ownership on its PVC for display. Existing label-only members are not
// stolen; members with an expired Lease are recoverable.
func ClaimCTMember(spec CTPoolSpec, identity string) (string, error) {
	if !spec.Ephemeral {
		return "", fmt.Errorf("persistent CT pool semantics are unsupported")
	}
	if strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("claim identity must not be empty")
	}
	pvcs, err := listPoolPVCs(spec)
	if err != nil {
		return "", err
	}
	for _, pvc := range pvcs {
		member := pvc.Metadata.Labels[ctMemberLabel]
		if member == "" {
			member = strings.TrimSuffix(pvc.Metadata.Name, "-data")
		}
		assigned := pvc.Metadata.Labels[ctAssignedLabel]
		if assigned != "" && assigned != identity {
			if current, leaseErr := readLease(spec.Namespace, member); leaseErr != nil || !leaseExpired(current, time.Now().UTC()) {
				continue
			}
		}
		acquired, err := AcquireLease(spec.Namespace, spec.Name, member, identity)
		if err != nil {
			return "", err
		}
		if !acquired {
			continue
		}
		if err := assignCTMember(spec, member, identity); err != nil {
			return "", fmt.Errorf("claimed %s but failed to update labels; release lease %s: %w", member, leaseName(member), err)
		}
		if err := ct.Start(member, spec.Namespace); err != nil {
			return "", fmt.Errorf("claimed %s but failed to start it: %w", member, err)
		}
		return member, nil
	}
	return "", fmt.Errorf("CT pool %q has no free members", spec.Name)
}

// Reconcile creates missing members, starts drifted stopped members, removes
// unassigned extras, and records an observable status in the pool ConfigMap.
// Assigned members are never deleted during scale-down or drift repair.
func Reconcile(spec CTPoolSpec) (CTPoolStatus, error) {
	status := CTPoolStatus{Pool: spec.Name, Desired: spec.Size, ObservedAt: time.Now().UTC()}
	if !spec.Ephemeral {
		return status, fmt.Errorf("CT pool %q is not ephemeral; persistent CT pool semantics are unsupported", spec.Name)
	}
	if spec.Size < 1 {
		return status, fmt.Errorf("CT pool size must be >= 1")
	}
	if spec.Name == "" || spec.Namespace == "" || spec.Image == "" {
		return status, fmt.Errorf("CT pool name, namespace, and image are required")
	}
	pvcs, err := listPoolPVCs(spec)
	if err != nil {
		status.Errors = append(status.Errors, err.Error())
		_ = ctPoolApply(poolConfig(spec, status))
		return status, err
	}
	byMember := map[string]ctPoolPVC{}
	for _, pvc := range pvcs {
		member := pvc.Metadata.Labels[ctMemberLabel]
		if member == "" {
			member = strings.TrimSuffix(pvc.Metadata.Name, "-data")
		}
		byMember[member] = pvc
	}

	for i := 1; i <= spec.Size; i++ {
		member := memberNameForPool(spec.Name, i)
		if _, ok := byMember[member]; !ok {
			err := ct.Create(ct.CreateOpts{Name: member, Namespace: spec.Namespace, Image: spec.Image, CPU: spec.CPU, Mem: spec.Mem, Disk: spec.Disk, StorageClass: spec.StorageClass, Privileged: spec.Privileged, Init: spec.Init})
			if err != nil {
				status.Errors = append(status.Errors, fmt.Sprintf("create %s: %v", member, err))
				continue
			}
			if err := labelCTMember(spec, member); err != nil {
				status.Errors = append(status.Errors, fmt.Sprintf("label %s: %v", member, err))
				continue
			}
			status.Created = append(status.Created, member)
		}
	}

	cts, listErr := ct.ListCTs()
	if listErr != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("list CTs: %v", listErr))
	}
	phase := map[string]string{}
	for _, item := range cts {
		if item.Namespace == spec.Namespace {
			phase[item.Name] = item.Phase
		}
	}
	for i := 1; i <= spec.Size; i++ {
		member := memberNameForPool(spec.Name, i)
		if _, exists := byMember[member]; !exists {
			if contains(status.Created, member) {
				status.Ready++
			}
			continue
		}
		if phase[member] == "Stopped" {
			if err := ct.Start(member, spec.Namespace); err != nil {
				status.Errors = append(status.Errors, fmt.Sprintf("start %s: %v", member, err))
				continue
			}
		}
		status.Ready++
	}

	for member, pvc := range byMember {
		if poolMemberIndex(spec.Name, member) <= spec.Size {
			continue
		}
		if pvc.Metadata.Labels[ctAssignedLabel] != "" {
			status.Protected = append(status.Protected, member)
			continue
		}
		if err := ct.Delete(member, spec.Namespace); err != nil {
			status.Errors = append(status.Errors, fmt.Sprintf("remove %s: %v", member, err))
			continue
		}
		status.Removed = append(status.Removed, member)
	}
	sort.Strings(status.Created)
	sort.Strings(status.Removed)
	sort.Strings(status.Protected)
	sort.Strings(status.Errors)
	if err := ctPoolApply(poolConfig(spec, status)); err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("record status: %v", err))
	}
	if len(status.Errors) > 0 {
		return status, fmt.Errorf("CT pool %q reconciliation incomplete: %s", spec.Name, strings.Join(status.Errors, "; "))
	}
	return status, nil
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func poolMemberIndex(pool, member string) int {
	var i int
	if _, err := fmt.Sscanf(member, pool+"-%d", &i); err != nil {
		return 0
	}
	return i
}

// ReleaseCTMember destroys an ephemeral member only after the atomic Lease is
// removed by its owner. The next reconciliation recreates the missing CT from
// the pool's declarative source.
func ReleaseCTMember(spec CTPoolSpec, member, identity string) error {
	if !spec.Ephemeral {
		return fmt.Errorf("persistent CT pool semantics are unsupported")
	}
	if err := ct.Delete(member, spec.Namespace); err != nil {
		return fmt.Errorf("destroy %s: %w", member, err)
	}
	if err := ReleaseLease(spec.Namespace, member, identity); err != nil {
		return fmt.Errorf("release claim for destroyed %s: %w", member, err)
	}
	if _, err := Reconcile(spec); err != nil {
		return fmt.Errorf("recreate released member %s: %w", member, err)
	}
	return nil
}
