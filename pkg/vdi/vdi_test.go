package vdi

import (
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
)

func withFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	SetRunner(fake)
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	return fake
}

func TestCreatePool_ClonesAndLabelsMembers(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden", "-n", "corral-vms", "-o", "name"}, "vm/golden", nil)
	fake.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)
	for i := 1; i <= 3; i++ {
		name := memberName("devpool", i)
		fake.AddResponseKV("kubectl", []string{"get", "vm", name, "-n", "corral-vms", "-o", "name"}, "vm/"+name, nil)
		fake.AddResponseKV("kubectl", []string{"label", "vm", name, "-n", "corral-vms", labelPool + "=devpool", "--overwrite"}, "", nil)
		fake.AddResponseKV("kubectl", []string{"label", "vm", name, "-n", "corral-vms", labelAssignedTo + "-", "--overwrite"}, "", nil)
		fake.AddResponseKV("kubectl", []string{"annotate", "vm", name, "-n", "corral-vms", annoClaimedAt + "-", "--overwrite"}, "", nil)
	}

	pool, err := CreatePool(CreateOpts{Name: "devpool", Namespace: "corral-vms", From: "golden", Size: 3})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if len(pool.Members) != 3 {
		t.Fatalf("got %d members, want 3", len(pool.Members))
	}
	for i, m := range pool.Members {
		want := memberName("devpool", i+1)
		if m.Name != want {
			t.Errorf("member %d = %q, want %q", i, m.Name, want)
		}
	}
}

func TestCreatePool_GoldenVMMissing(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "ghost", "-n", "corral-vms", "-o", "name"}, "", &fakeErr{"not found"})

	_, err := CreatePool(CreateOpts{Name: "devpool", Namespace: "corral-vms", From: "ghost", Size: 2})
	if err == nil {
		t.Error("expected an error when the golden VM doesn't exist")
	}
}

// TestCreatePool_WaitsForCloneToProduceVM is a regression test for a bug
// found live: Clone() returns as soon as the VirtualMachineClone CRD is
// applied, not once KubeVirt's clone controller actually creates the
// target VM — labeling it immediately (the original implementation) races
// the controller and fails on a real cluster before the VM exists yet.
func TestCreatePool_WaitsForCloneToProduceVM(t *testing.T) {
	fake := withFake(t)
	orig, origInterval := cloneWaitTimeout, clonePollInterval
	cloneWaitTimeout, clonePollInterval = 50*time.Millisecond, 5*time.Millisecond
	defer func() { cloneWaitTimeout, clonePollInterval = orig, origInterval }()

	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden", "-n", "corral-vms", "-o", "name"}, "vm/golden", nil)
	fake.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)
	// Deliberately no "get vm devpool-1 ... -o name" response registered —
	// the clone never "finishes" from CreatePool's point of view, so this
	// must time out rather than racing straight into labelMember.
	_, err := CreatePool(CreateOpts{Name: "devpool", Namespace: "corral-vms", From: "golden", Size: 1})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error waiting for the clone, got: %v", err)
	}
}

func TestCreatePool_RejectsZeroSize(t *testing.T) {
	withFake(t)
	_, err := CreatePool(CreateOpts{Name: "devpool", Namespace: "corral-vms", From: "golden", Size: 0})
	if err == nil {
		t.Error("expected an error for --size 0")
	}
}

func TestListPools_GroupsByPoolLabel(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool"}},
			"status":{"printableStatus":"Running"}},
		{"metadata":{"name":"devpool-2","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"alice"},
			"annotations":{"corral.dev/vdi-claimed-at":"2026-07-02T12:00:00Z"}},
			"status":{"printableStatus":"Running"}},
		{"metadata":{"name":"qapool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"qapool"}},
			"status":{"printableStatus":"Stopped"}}
	]}`, nil)

	pools, err := ListPools()
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(pools))
	}
	byName := map[string]Pool{}
	for _, p := range pools {
		byName[p.Name] = p
	}
	if len(byName["devpool"].Members) != 2 {
		t.Errorf("devpool has %d members, want 2", len(byName["devpool"].Members))
	}
	if len(byName["qapool"].Members) != 1 {
		t.Errorf("qapool has %d members, want 1", len(byName["qapool"].Members))
	}
	var claimed Member
	for _, m := range byName["devpool"].Members {
		if m.AssignedTo != "" {
			claimed = m
		}
	}
	if claimed.AssignedTo != "alice" || claimed.ClaimedAt == "" {
		t.Errorf("expected devpool-2 assigned to alice with a claim time, got %+v", claimed)
	}
}

func TestAssign_ClaimsFirstFreeMemberAndStartsIfStopped(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"bob"}},
			"status":{"printableStatus":"Running"}},
		{"metadata":{"name":"devpool-2","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool"}},
			"status":{"printableStatus":"Stopped"}}
	]}`, nil)
	fake.AddResponseKV("kubectl", []string{"create", "-f", "-"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-2", "-n", "corral-vms", labelPool + "=devpool", "--overwrite"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-2", "-n", "corral-vms", labelAssignedTo + "=alice", "--overwrite"}, "", nil)
	fake.AddPrefixResponse("kubectl annotate vm devpool-2 -n corral-vms corral.dev/vdi-claimed-at=", "", nil)
	fake.AddResponseKV("/fake/bin/virtctl", []string{"start", "devpool-2", "-n", "corral-vms"}, "", nil)

	got, err := Assign("corral-vms", "devpool", "alice")
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got != "devpool-2" {
		t.Errorf("Assign picked %q, want devpool-2 (the free member)", got)
	}
	started := false
	for _, c := range fake.Calls() {
		if strings.Contains(c.Name, "virtctl") && len(c.Args) > 0 && c.Args[0] == "start" {
			started = true
		}
	}
	if !started {
		t.Error("expected the newly assigned (stopped) member to be started")
	}
}

func TestAssign_NoFreeMembers(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"bob"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)

	_, err := Assign("corral-vms", "devpool", "alice")
	if err == nil {
		t.Error("expected an error when every member is already claimed")
	}
}

func TestAssign_PoolNotFound(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=ghost"}, `{"items":[]}`, nil)

	_, err := Assign("corral-vms", "ghost", "alice")
	if err == nil {
		t.Error("expected an error for a pool with no members")
	}
}

func TestUnassign_ClearsLabelAndStops(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-2", "-n", "corral-vms", "-o",
		`jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-2", "-n", "corral-vms", labelPool + "=devpool", "--overwrite"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-2", "-n", "corral-vms", labelAssignedTo + "-", "--overwrite"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"annotate", "vm", "devpool-2", "-n", "corral-vms", annoClaimedAt + "-", "--overwrite"}, "", nil)
	fake.AddResponseKV("/fake/bin/virtctl", []string{"stop", "devpool-2", "-n", "corral-vms"}, "", nil)

	if err := Unassign("corral-vms", "devpool-2"); err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	stopped := false
	for _, c := range fake.Calls() {
		if strings.Contains(c.Name, "virtctl") && len(c.Args) > 0 && c.Args[0] == "stop" {
			stopped = true
		}
	}
	if !stopped {
		t.Error("expected Unassign to stop the member")
	}
}

func TestClaim_RecordsAtomicLease(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool"}},"status":{"printableStatus":"Running"}}]}`, nil)
	fake.AddResponseKV("kubectl", []string{"create", "-f", "-"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-1", "-n", "corral-vms", labelPool + "=devpool", "--overwrite"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"label", "vm", "devpool-1", "-n", "corral-vms", labelAssignedTo + "=alice", "--overwrite"}, "", nil)
	fake.AddPrefixResponse("kubectl annotate vm devpool-1 -n corral-vms "+annoClaimedAt+"=", "", nil)

	got, err := Claim("corral-vms", "devpool", "alice")
	if err != nil || got != "devpool-1" {
		t.Fatalf("Claim = %q, %v", got, err)
	}
	calls := fake.Calls()
	if len(calls) < 2 || calls[1].Name != "kubectl" || calls[1].Args[0] != "create" {
		t.Fatalf("expected lease create, calls=%+v", calls)
	}
	if !strings.Contains(calls[1].Stdin, `"kind":"Lease"`) || !strings.Contains(calls[1].Stdin, `"holderIdentity":"alice"`) || !strings.Contains(calls[1].Stdin, labelPool) {
		t.Fatalf("lease manifest missing claim state: %s", calls[1].Stdin)
	}
}

func TestRelease_RejectsNonOwner(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "lease", leaseName("devpool-1"), "-n", "corral-vms", "-o", "json"}, `{"metadata":{"resourceVersion":"7"},"spec":{"holderIdentity":"bob","renewTime":"2099-01-01T00:00:00Z","leaseDurationSeconds":3600}}`, nil)
	if err := Release("corral-vms", "devpool-1", "alice"); err == nil || !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("expected ownership error, got %v", err)
	}
	for _, call := range fake.Calls() {
		if call.Args[0] == "delete" || call.Args[0] == "label" {
			t.Fatalf("release mutated a non-owned claim: %+v", call)
		}
	}
}

func TestDeletePool_DeletesAllMembers(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms"},"status":{"printableStatus":"Running"}},
		{"metadata":{"name":"devpool-2","namespace":"corral-vms"},"status":{"printableStatus":"Stopped"}}
	]}`, nil)
	fake.AddResponseKV("/fake/bin/virtctl", []string{"stop", "devpool-1", "-n", "corral-vms"}, "", nil)
	fake.AddResponseKV("/fake/bin/virtctl", []string{"stop", "devpool-2", "-n", "corral-vms"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"delete", "vm", "devpool-1", "-n", "corral-vms", "--ignore-not-found"}, "", nil)
	fake.AddResponseKV("kubectl", []string{"delete", "vm", "devpool-2", "-n", "corral-vms", "--ignore-not-found"}, "", nil)
	for _, name := range []string{"devpool-1", "devpool-2"} {
		for _, suffix := range []string{"disk", "data", "iso", "bootc-disk"} {
			fake.AddPrefixResponse("kubectl delete pvc,datavolume "+name+"-"+suffix, "", nil)
		}
	}
	fake.AddPrefixResponse("kubectl delete", "", nil) // catch-all for the rest of DeleteVM's cleanup calls

	if err := DeletePool("corral-vms", "devpool"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	deleted := map[string]bool{}
	for _, c := range fake.Calls() {
		if c.Name == "kubectl" && len(c.Args) > 1 && c.Args[0] == "delete" && c.Args[1] == "vm" {
			deleted[c.Args[2]] = true
		}
	}
	if !deleted["devpool-1"] || !deleted["devpool-2"] {
		t.Errorf("expected both members deleted, got %v", deleted)
	}
}

func TestDeletePool_NotFound(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=ghost"}, `{"items":[]}`, nil)

	if err := DeletePool("corral-vms", "ghost"); err == nil {
		t.Error("expected an error deleting a pool with no members")
	}
}

func TestValidateUSBRedir_Success(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"alice"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)

	m, err := ValidateUSBRedir(USBRedirOpts{
		Namespace: "corral-vms",
		Member:    "devpool-1",
		User:      "alice",
	})
	if err != nil {
		t.Fatalf("ValidateUSBRedir: %v", err)
	}
	if m.Name != "devpool-1" || m.AssignedTo != "alice" {
		t.Errorf("got %+v, want member devpool-1 assigned to alice", m)
	}
}

func TestValidateUSBRedir_Unassigned(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)

	_, err := ValidateUSBRedir(USBRedirOpts{
		Namespace: "corral-vms",
		Member:    "devpool-1",
	})
	if err == nil || !strings.Contains(err.Error(), "not assigned") {
		t.Errorf("expected not assigned error, got %v", err)
	}
}

func TestValidateUSBRedir_UnauthorizedUser(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"bob"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)

	_, err := ValidateUSBRedir(USBRedirOpts{
		Namespace: "corral-vms",
		Member:    "devpool-1",
		User:      "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "authorization failed") {
		t.Errorf("expected authorization failed error, got %v", err)
	}
}

func TestValidateUSBRedir_Stopped(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", labelPool + "=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"alice"}},
			"status":{"printableStatus":"Stopped"}}
	]}`, nil)

	_, err := ValidateUSBRedir(USBRedirOpts{
		Namespace: "corral-vms",
		Member:    "devpool-1",
		User:      "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("expected stopped member error, got %v", err)
	}
}

func TestBuildVirtctlUSBCmd(t *testing.T) {
	cmd := BuildVirtctlUSBCmd("/usr/local/bin/virtctl", "devpool-1", "corral-vms", "1050:0407", "custom-ctx")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "usbredir devpool-1 -n corral-vms --device=1050:0407") {
		t.Errorf("unexpected virtctl command args: %s", args)
	}
	if !strings.Contains(args, "--context=custom-ctx") {
		t.Errorf("context was not injected into virtctl command: %s", args)
	}
}

func TestListLocalUSBDevices_Seam(t *testing.T) {
	orig := ListUSBDevicesFunc
	defer func() { ListUSBDevicesFunc = orig }()

	ListUSBDevicesFunc = func() ([]USBDevice, error) {
		return []USBDevice{
			{
				BusNum:      "3",
				DevNum:      "2",
				VendorID:    "1050",
				ProductID:   "0407",
				Description: "Yubico.com YubiKey OTP+FIDO+CCID",
			},
		}, nil
	}

	devs, err := ListLocalUSBDevices()
	if err != nil {
		t.Fatalf("ListLocalUSBDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}
	if devs[0].Selector() != "1050:0407" {
		t.Errorf("Selector = %q, want 1050:0407", devs[0].Selector())
	}
	consequences := USBConsequences(devs[0])
	if len(consequences) < 3 {
		t.Errorf("expected consequences explained, got %v", consequences)
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

