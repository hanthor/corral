package vdi

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/shell"
)

func TestReconcileCTPoolCreatesMissingMembersAndRecordsStatus(t *testing.T) {
	poolRunner := shell.NewFake()
	poolRunner.AddResponseKV("kubectl", []string{"get", "pvc", "-n", "vdi", "-l", ctPoolLabel + "=linux", "-o", "json"}, `{"items":[]}`, nil)
	poolRunner.AddResponseKV("kubectl", []string{"label", "pvc", "linux-1-data", "-n", "vdi", ctPoolLabel + "=linux", ctMemberLabel + "=linux-1", "--overwrite"}, "", nil)
	poolRunner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
	SetCTPoolRunner(poolRunner)
	ctRunner := shell.NewFake()
	ctRunner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
	ctRunner.AddResponseKV("kubectl", []string{"get", "pvc", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[]}`, nil)
	ctRunner.AddResponseKV("kubectl", []string{"get", "pods", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[]}`, nil)
	ct.SetRunner(ctRunner)
	t.Cleanup(func() { SetCTPoolRunner(shell.DefaultKubectl); ct.SetRunner(shell.DefaultKubectl) })

	status, err := Reconcile(CTPoolSpec{Name: "linux", Namespace: "vdi", Image: "debian:13", Size: 1, Ephemeral: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(status.Created) != 1 || status.Created[0] != "linux-1" {
		t.Fatalf("created = %+v", status.Created)
	}
	if status.Desired != 1 {
		t.Fatalf("desired = %d", status.Desired)
	}
	if len(status.Errors) != 0 {
		t.Fatalf("errors = %+v", status.Errors)
	}
}

func TestReconcileCTPoolProtectsAssignedMembersOnScaleDown(t *testing.T) {
	poolRunner := shell.NewFake()
	poolRunner.AddResponseKV("kubectl", []string{"get", "pvc", "-n", "vdi", "-l", ctPoolLabel + "=linux", "-o", "json"}, `{"items":[
		{"metadata":{"name":"linux-1-data","labels":{"corral.dev/ct-pool":"linux","corral.dev/ct-member":"linux-1"}}},
		{"metadata":{"name":"linux-2-data","labels":{"corral.dev/ct-pool":"linux","corral.dev/ct-member":"linux-2","corral.dev/vdi-assigned-to":"alice"}}}]}`, nil)
	poolRunner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", nil)
	SetCTPoolRunner(poolRunner)
	ctRunner := shell.NewFake()
	ctRunner.AddResponseKV("kubectl", []string{"get", "pvc", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[]}`, nil)
	ctRunner.AddResponseKV("kubectl", []string{"get", "pods", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[]}`, nil)
	ct.SetRunner(ctRunner)
	t.Cleanup(func() { SetCTPoolRunner(shell.DefaultKubectl); ct.SetRunner(shell.DefaultKubectl) })

	status, err := Reconcile(CTPoolSpec{Name: "linux", Namespace: "vdi", Image: "debian:13", Size: 1, Ephemeral: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(status.Protected) != 1 || status.Protected[0] != "linux-2" {
		t.Fatalf("protected = %+v", status.Protected)
	}
	for _, call := range ctRunner.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "delete") {
			t.Fatalf("assigned member was deleted: %+v", call)
		}
	}
}

func TestReconcileCTPoolRejectsPersistentMode(t *testing.T) {
	if _, err := Reconcile(CTPoolSpec{Name: "linux", Namespace: "vdi", Image: "debian", Size: 1}); err == nil || !strings.Contains(err.Error(), "persistent CT") {
		t.Fatalf("expected explicit persistent-mode error, got %v", err)
	}
}
