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

const kvCRWithDevices = `{
  "spec": {"configuration": {"permittedHostDevices": {
    "pciHostDevices": [
      {"pciVendorSelector": "1002:744c", "resourceName": "amd.com/gpu"},
      {"pciVendorSelector": "10de:2204", "resourceName": "nvidia.com/GA102", "externalResourceProvider": true}
    ],
    "mediatedDevices": [
      {"mdevNameSelector": "GRID-T4-2Q", "resourceName": "nvidia.com/GRID-T4-2Q"}
    ]
  }}}
}`

const goldenVMWithGPU = `{
  "metadata": {"name": "golden-gpu", "namespace": "corral-vms"},
  "spec": {
    "template": {
      "spec": {
        "domain": {
          "devices": {
            "gpus": [
              {"name": "gpu1", "deviceName": "amd.com/gpu"}
            ]
          }
        }
      }
    }
  }
}`

const goldenVMWithMediated = `{
  "metadata": {"name": "golden-mdev", "namespace": "corral-vms"},
  "spec": {
    "template": {
      "spec": {
        "domain": {
          "devices": {
            "gpus": [
              {"name": "gpu1", "deviceName": "nvidia.com/GRID-T4-2Q"}
            ]
          }
        }
      }
    }
  }
}`

func TestValidatePoolDeviceCapacity_ExclusiveSuccess(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "json"}, goldenVMWithGPU, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"amd.com/gpu": "2"}}},
	    {"metadata": {"name": "node-2"}, "status": {"allocatable": {"amd.com/gpu": "2"}}}
	  ]
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)

	reps, err := ValidatePoolDeviceCapacity("corral-vms", "golden-gpu", 4)
	if err != nil {
		t.Fatalf("ValidatePoolDeviceCapacity: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reps))
	}
	if reps[0].Type != DeviceTypePCIHostDevice {
		t.Errorf("got type %q, want %q", reps[0].Type, DeviceTypePCIHostDevice)
	}
	if reps[0].AllocatableTotal != 4 || reps[0].AvailableTotal != 4 || reps[0].MaxConcurrency != 4 {
		t.Errorf("unexpected capacity: %+v", reps[0])
	}
}

func TestValidatePoolDeviceCapacity_MediatedDevice(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-mdev", "-n", "corral-vms", "-o", "json"}, goldenVMWithMediated, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"nvidia.com/GRID-T4-2Q": "8"}}}
	  ]
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)

	reps, err := ValidatePoolDeviceCapacity("corral-vms", "golden-mdev", 8)
	if err != nil {
		t.Fatalf("ValidatePoolDeviceCapacity: %v", err)
	}
	if reps[0].Type != DeviceTypeMediatedDevice {
		t.Errorf("got type %q, want %q", reps[0].Type, DeviceTypeMediatedDevice)
	}
	if reps[0].AvailableTotal != 8 {
		t.Errorf("expected 8 available, got %d", reps[0].AvailableTotal)
	}
}

func TestValidatePoolDeviceCapacity_ExistingAllocationsAndHeterogeneousNodes(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "json"}, goldenVMWithGPU, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	// node-1 has 2 GPUs, node-2 has 1 GPU, node-3 has 0
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"amd.com/gpu": "2"}}},
	    {"metadata": {"name": "node-2"}, "status": {"allocatable": {"amd.com/gpu": "1"}}},
	    {"metadata": {"name": "node-3"}, "status": {"allocatable": {"cpu": "16"}}}
	  ]
	}`, nil)
	// Existing VMI already consumes 1 GPU on node-1
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{
	  "items": [
	    {
	      "metadata": {"name": "workstation-1", "namespace": "default"},
	      "status": {"nodeName": "node-1", "phase": "Running"},
	      "spec": {"domain": {"devices": {"gpus": [{"deviceName": "amd.com/gpu"}]}}}
	    }
	  ]
	}`, nil)

	// Asking for size 2 should succeed (1 on node-1, 1 on node-2)
	reps, err := ValidatePoolDeviceCapacity("corral-vms", "golden-gpu", 2)
	if err != nil {
		t.Fatalf("ValidatePoolDeviceCapacity: %v", err)
	}
	if reps[0].AllocatedExisting != 1 || reps[0].AvailableTotal != 2 {
		t.Errorf("expected 1 allocated, 2 available; got %+v", reps[0])
	}

	// Asking for size 3 should fail (only 2 available across nodes)
	_, err = ValidatePoolDeviceCapacity("corral-vms", "golden-gpu", 3)
	if err == nil || !strings.Contains(err.Error(), "insufficient device capacity") {
		t.Fatalf("expected insufficient capacity error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "node node-1: 1 available / 2 allocatable") {
		t.Errorf("expected per-node breakdown in error: %v", err)
	}
}

func TestValidatePoolDeviceCapacity_NotAllocatableOnAnyNode(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "json"}, goldenVMWithGPU, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"cpu": "8"}}}
	  ]
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)

	_, err := ValidatePoolDeviceCapacity("corral-vms", "golden-gpu", 1)
	if err == nil || !strings.Contains(err.Error(), "not allocatable on any node") {
		t.Fatalf("expected not allocatable error, got: %v", err)
	}
}

func TestValidatePoolDeviceCapacity_UnknownCapability(t *testing.T) {
	fake := withFake(t)
	// VM requests a custom vendor device not listed in KubeVirt permittedHostDevices
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-custom", "-n", "corral-vms", "-o", "json"}, `{
	  "metadata": {"name": "golden-custom", "namespace": "corral-vms"},
	  "spec": {
	    "template": {
	      "spec": {
	        "domain": {
	          "devices": {
	            "hostDevices": [
	              {"name": "fpga1", "deviceName": "xilinx.com/fpga"}
	            ]
	          }
	        }
	      }
	    }
	  }
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"xilinx.com/fpga": "2"}}}
	  ]
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)

	reps, err := ValidatePoolDeviceCapacity("corral-vms", "golden-custom", 2)
	if err != nil {
		t.Fatalf("ValidatePoolDeviceCapacity: %v", err)
	}
	if reps[0].Type != DeviceTypeUnknown {
		t.Errorf("got type %q, want %q", reps[0].Type, DeviceTypeUnknown)
	}
}

func TestCreatePool_DeviceCapacityExhaustionRefusesCreation(t *testing.T) {
	fake := withFake(t)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "name"}, "vm/golden-gpu", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "json"}, goldenVMWithGPU, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, kvCRWithDevices, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{
	  "items": [
	    {"metadata": {"name": "node-1"}, "status": {"allocatable": {"amd.com/gpu": "1"}}}
	  ]
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)

	// Attempt to create pool of size 2 with only 1 GPU in cluster
	_, err := CreatePool(CreateOpts{Name: "gpupool", Namespace: "corral-vms", From: "golden-gpu", Size: 2})
	if err == nil || !strings.Contains(err.Error(), "insufficient device capacity") {
		t.Fatalf("expected admission refusal, got: %v", err)
	}

	// Verify no clone apply commands ran
	for _, c := range fake.Calls() {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "apply" {
			t.Errorf("unexpected apply call on refused pool creation: %v", c)
		}
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }
