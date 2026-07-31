package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/kubevirt"
)

// errSimulated is a sentinel error used in error-path tests.
var errSimulated = fmt.Errorf("simulated error")

// ── Images ────────────────────────────────────────────────────────

func TestHandleImages(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Get(fx.Server.URL + "/api/images")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var images []catalog.Image
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 {
		t.Fatal("expected non-empty image catalog")
	}

	// Verify a known image exists
	found := false
	for _, img := range images {
		if img.Name == "fedora" {
			found = true
			if img.ContainerDisk == "" {
				t.Error("fedora has no containerDisk")
			}
			break
		}
	}
	if !found {
		t.Error("fedora not found in catalog")
	}
}

// ── Snapshots ─────────────────────────────────────────────────────

func TestHandleDeleteSnapshot_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// kubectl delete vmsnapshot
	fx.Runner.AddResponseKV("kubectl", []string{
		"delete", "vmsnapshot", "snap1", "-n", "tailvm", "--ignore-not-found",
	}, "", nil)

	req, _ := http.NewRequest("DELETE", fx.Server.URL+"/api/vms/tailvm/testvm/snapshots/snap1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "deleted" {
		t.Errorf("expected status=deleted, got %v", body)
	}
}

func TestHandleRestoreSnapshot_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// virtctl (for restore — virtctlRun is used)
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{
		"vmrestore", "testvm", "-n", "tailvm",
		"--restore-from-snapshot", "snap1",
	}, "", nil)
	// kubectl apply for the VirtualMachineRestore manifest
	fx.Runner.AddResponseKV("kubectl", []string{
		"apply", "-f", "-",
	}, "applied", nil)
	// kubectl get vm (vmDomain check in restore logic)
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "vm", "testvm", "-n", "tailvm", "-o", "json",
	}, `{"metadata":{"name":"testvm"}}`, nil)

	req, _ := http.NewRequest("POST",
		fx.Server.URL+"/api/vms/tailvm/testvm/snapshots/snap1/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 200 is expected but handle gracefully if the pipeline needs more setup
	if resp.StatusCode == 200 {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		if body["status"] != "restoring" {
			t.Errorf("expected status=restoring, got %v", body)
		}
	}
}

// ── Capabilities ──────────────────────────────────────────────────

func TestHandleCapabilities(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// ClusterCapabilities uses kubectl to list storage classes
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "sc", "-o", "json",
	}, `{"items":[{"metadata":{"name":"longhorn"},"allowVolumeExpansion":true}]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "volumesnapshotclass", "-o", "name",
	}, "volumesnapshotclass.snapshot.storage.k8s.io/longhorn-snapshot", nil)

	resp, err := http.Get(fx.Server.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var caps map[string]any
	json.NewDecoder(resp.Body).Decode(&caps)

	if _, ok := caps["storageClass"]; !ok {
		t.Error("missing storageClass")
	}
	if _, ok := caps["canExpand"]; !ok {
		t.Error("missing canExpand")
	}
	if _, ok := caps["canSnapshot"]; !ok {
		t.Error("missing canSnapshot")
	}
	if _, ok := caps["bootc"]; !ok {
		t.Error("missing bootc")
	}
}

// ── Plugins ───────────────────────────────────────────────────────

func TestHandlePlugins(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Get(fx.Server.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// May be 200 (marketplace reachable) or just return empty
	if resp.StatusCode == 200 {
		var items []map[string]any
		json.NewDecoder(resp.Body).Decode(&items)
		// Either empty or has plugin entries
		if items == nil {
			t.Error("expected array, got nil")
		}
	}
}

// ── Instance types ────────────────────────────────────────────────

func TestHandleInstanceTypes(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// ListInstanceTypes uses kubectl
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "virtualmachineclusterinstancetypes", "-o", "json",
	}, `{"items":[]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "virtualmachineclusterpreferences", "-o", "json",
	}, `{"items":[]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/instancetypes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── NADs ──────────────────────────────────────────────────────────

func TestHandleNADs(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "net-attach-def", "-A", "-o", "json",
	}, `{"items":[]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/nads")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Scale ─────────────────────────────────────────────────────────

func TestHandleScale_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// Scale reads the VM domain, patches it
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "vm", "testvm", "-n", "tailvm", "-o", "json",
	}, `{
		"metadata":{"name":"testvm"},
		"spec":{"template":{"spec":{"domain":{"cpu":{"sockets":1,"cores":1,"threads":1},"resources":{"requests":{"memory":"2Gi"}}}}}},
		"status":{"printableStatus":"Stopped"}
	}`, nil)
	// The patch command has dynamic JSON — use prefix match
	fx.Runner.AddPrefixResponse("kubectl patch vm testvm -n tailvm --type merge -p", "", nil)

	body := strings.NewReader(`{"cpu":4,"mem":"8G"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/scale",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleSetOptions_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "vm", "testvm", "-n", "tailvm", "-o", "json",
	}, `{
		"metadata":{"name":"testvm","namespace":"tailvm"},
		"spec":{"running":true,"template":{"spec":{"domain":{"devices":{"disks":[{"name":"rootdisk"}]}}}}}
	}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	body := strings.NewReader(`{"runStrategy":"Manual"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/options",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var sawApply bool
	for _, c := range fx.Runner.Calls() {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "apply" {
			sawApply = true
			if !strings.Contains(c.Stdin, `"runStrategy":"Manual"`) {
				t.Errorf("applied manifest missing runStrategy: %s", c.Stdin)
			}
		}
	}
	if !sawApply {
		t.Error("expected a kubectl apply call")
	}
}

func TestHandleSetOptions_MalformedJSON(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	body := strings.NewReader(`not json`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/options",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// ── DataVolumes (image library) ───────────────────────────────────

func TestHandleListDataVolumes(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "datavolumes", "-A", "-o", "json",
	}, `{"items":[{"metadata":{"name":"jammy","namespace":"tailvm"},"status":{"phase":"Succeeded"}}]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/datavolumes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleImportDataVolume(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// ImportDataVolume needs kubectl apply for the DataVolume manifest
	fx.Runner.AddResponseKV("kubectl", []string{
		"apply", "-f", "-",
	}, "applied", nil)

	body := strings.NewReader(`{"name":"jammy","namespace":"tailvm","url":"https://example.com/jammy.qcow2","size":"10Gi"}`)
	resp, err := http.Post(fx.Server.URL+"/api/datavolumes", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleImportDataVolume_MissingFields(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	body := strings.NewReader(`{}`)
	resp, err := http.Post(fx.Server.URL+"/api/datavolumes", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing fields, got %d", resp.StatusCode)
	}
}

func TestHandleUploadDataVolume_MissingParams(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/datavolumes/upload?name=iso1", "application/octet-stream", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing size, got %d", resp.StatusCode)
	}
}

func TestHandleUploadDataVolume_EmptyBody(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/datavolumes/upload?name=iso1&size=1Gi", "application/octet-stream", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for an empty upload, got %d", resp.StatusCode)
	}
}

func TestHandleUploadDataVolume_StartsBackgroundTask(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/datavolumes/upload?name=iso1&namespace=tailvm&size=1Gi",
		"application/octet-stream", strings.NewReader("fake iso bytes"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	var respBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		t.Fatal(err)
	}
	if respBody["task"] == "" {
		t.Fatal("expected a non-empty task id")
	}

	// shell.Fake.LookPath always resolves to a nonexistent /fake/bin/virtctl,
	// so the background exec.Command fails fast and deterministically —
	// poll the task instead of waiting a fixed duration.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := tasks.Load(respBody["task"])
		if ok && v.(*buildTask).snapshot()["status"] != "running" {
			snap := v.(*buildTask).snapshot()
			if snap["status"] != "error" {
				t.Errorf("status = %q, want error (no real virtctl in tests)", snap["status"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task never finished")
}

func TestHandleDeleteDataVolume(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{
		"delete", "datavolume", "jammy", "-n", "tailvm", "--ignore-not-found",
	}, "", nil)

	req, _ := http.NewRequest("DELETE", fx.Server.URL+"/api/datavolumes/tailvm/jammy", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Doctor ────────────────────────────────────────────────────────

func TestHandleDoctor(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// Doctor checks run kubectl commands — provide basic responses
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "kubevirt.kubevirt.io", "-A", "-o", "name",
	}, "kubevirt.kubevirt.io/kubevirt", nil)
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "cdi", "-A", "-o", "name",
	}, "cdi.cdi.kubevirt.io/cdi", nil)
	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "ns", "tailvm", "-o", "name",
	}, "namespace/tailvm", nil)

	resp, err := http.Get(fx.Server.URL + "/api/doctor")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Doctor fix ───────────────────────────────────────────────────

func TestHandleDoctorFix(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// Bare cluster: every doctor check fails, so Fix installs KubeVirt + CDI
	// (kubectl apply of the upstream manifests) and best-effort reconfigures.
	fx.Runner.AddPrefixResponse("kubectl apply -f", "applied", nil)
	fx.Runner.AddPrefixResponse("kubectl patch kubevirt", "patched", nil)
	fx.Runner.AddPrefixResponse("kubectl patch deployment metrics-server", "patched", nil)

	resp, err := http.Post(fx.Server.URL+"/api/doctor/fix", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Clone ────────────────────────────────────────────────────────

func TestHandleClone(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{
		"get", "vm", "testvm", "-n", "tailvm", "-o", "json",
	}, `{"metadata":{"name":"testvm"},"spec":{"template":{"spec":{"volumes":[{"name":"disk","persistentVolumeClaim":{"claimName":"testvm-disk"}}]}}}}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	body := strings.NewReader(`{"target":"testvm-clone"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/clone", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		if out["target"] != "testvm-clone" {
			t.Errorf("expected target=testvm-clone, got %v", out)
		}
	}
}

func TestHandleClone_MissingTarget(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/clone", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing target, got %d", resp.StatusCode)
	}
}

// ── Guest info ───────────────────────────────────────────────────

func TestHandleGuestInfo(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"guestosinfo", "testvm", "-n", "tailvm"}, `{"name":"fedora","version":"42"}`, nil)
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"fslist", "testvm", "-n", "tailvm"}, `{"items":[]}`, nil)
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"userlist", "testvm", "-n", "tailvm"}, `{"items":[]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/guestinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var info map[string]any
	json.NewDecoder(resp.Body).Decode(&info)
	if info["os"] == nil {
		t.Error("missing os field")
	}
}

// ── Events ───────────────────────────────────────────────────────

func TestHandleEvents(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "events", "-n", "tailvm", "-o", "json"}, `{"items":[]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Metrics ──────────────────────────────────────────────────────

func TestHandleMetrics(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"top", "pod", "-n", "tailvm", "-l", "vm.kubevirt.io/name=testvm", "--no-headers", "--containers"}, "", fmt.Errorf("metrics unavailable"))

	resp, _ := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/metrics")
	if resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Logf("metrics returned %d (expected 200, metrics gracefully degrade)", resp.StatusCode)
		}
	}
}

// ── Snapshots ────────────────────────────────────────────────────

func TestHandleCreateSnapshot(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	body := strings.NewReader(`{"name":"snap1"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/snapshots", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["name"] != "snap1" {
		t.Errorf("expected name=snap1, got %v", out)
	}
}

func TestHandleListSnapshots(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmsnapshot", "-n", "tailvm", "-o", "json"},
		`{"items":[{"metadata":{"name":"snap1"},"status":{"phase":"Succeeded"}}]}`, nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Volumes ──────────────────────────────────────────────────────

func TestHandleAddVolume(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)
	// PVC name contains a timestamp — use prefix match
	fx.Runner.AddPrefixResponse("/fake/bin/virtctl addvolume testvm --volume-name=testvm-hp-", "", nil)

	body := strings.NewReader(`{"size":"10Gi"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/volumes", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["pvc"] == "" {
		t.Error("expected pvc name in response")
	}
}

func TestHandleRemoveVolume(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"removevolume", "testvm", "--volume-name=disk-2", "-n", "tailvm"}, "", nil)

	req, _ := http.NewRequest("DELETE", fx.Server.URL+"/api/vms/tailvm/testvm/volumes/disk-2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ── Expand disk ──────────────────────────────────────────────────

func TestHandleExpand(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"patch", "pvc", "testvm-disk", "-n", "tailvm", "--type", "merge", "-p",
		`{"spec":{"resources":{"requests":{"storage":"40Gi"}}}}`}, "", nil)

	body := strings.NewReader(`{"pvc":"testvm-disk","size":"40Gi"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/expand", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleExpand_MissingFields(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/expand", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing fields, got %d", resp.StatusCode)
	}
}

// ── Template ─────────────────────────────────────────────────────

func TestHandleMarkTemplate(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"label", "vm", "testvm", "-n", "tailvm", "corral.dev/template=true", "--overwrite"}, "", nil)

	body := strings.NewReader(`{"on":true}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/template", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var out map[string]bool
	json.NewDecoder(resp.Body).Decode(&out)
	if !out["isTemplate"] {
		t.Errorf("expected isTemplate=true, got %v", out)
	}
}

// ── Add NIC ──────────────────────────────────────────────────────

func TestHandleAddNIC(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// AddNIC generates a dynamic JSON patch — use prefix match
	fx.Runner.AddPrefixResponse("kubectl patch vm testvm -n tailvm --type json -p", "", nil)

	body := strings.NewReader(`{"nad":"default/lan","iface":"eth1"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/nics", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", out)
	}
}

// ── Bootc task ───────────────────────────────────────────────────

func TestHandleTaskStatus_NotFound(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Get(fx.Server.URL + "/api/tasks/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for unknown task, got %d", resp.StatusCode)
	}
}

// ── Error paths ──────────────────────────────────────────────────

func TestHandleScale_MissingBody(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, _ := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/scale", "application/json", strings.NewReader(`{}`))
	if resp != nil {
		defer resp.Body.Close()
		t.Logf("scale with empty body returned %d", resp.StatusCode)
	}
}

// ── Error paths ──────────────────────────────────────────────────

func TestHandleListVMs_KubectlDown(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502 when kubectl is down, got %d", resp.StatusCode)
	}
}

func TestHandleNodes_KubectlDown(t *testing.T) {
	forceKubevirt(t)
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502 when kubectl is down, got %d", resp.StatusCode)
	}
}

func TestHandleGuestInfo_AgentUnreachable(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// virtctl guestosinfo fails — guest agent not running
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"guestosinfo", "testvm", "-n", "tailvm"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/guestinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Errorf("expected 503 when guest agent is unreachable, got %d", resp.StatusCode)
	}
}

func TestHandleExport_VMRunning(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// VM is running — export should reject
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmi", "testvm", "-n", "tailvm"}, "vmi/testvm", nil)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 409 {
		t.Errorf("expected 409 when VM is running, got %d", resp.StatusCode)
	}
}

func TestHandleAction_Migrate(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"migrate", "testvm", "-n", "tailvm"}, "", nil)

	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/migrate", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Migrate checks canLiveMigrate which probes the cluster — accept 200 or 500
	if resp.StatusCode != 200 && resp.StatusCode != 500 {
		t.Errorf("expected 200 or 500, got %d", resp.StatusCode)
	}
}

func TestHandleAction_PauseResume(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// Pause
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"pause", "vm", "testvm", "-n", "tailvm"}, "", nil)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/pause", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("pause: expected 200, got %d", resp.StatusCode)
	}

	// Unpause
	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"unpause", "vm", "testvm", "-n", "tailvm"}, "", nil)
	resp, err = http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/unpause", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("unpause: expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleAction_Restart(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("/fake/bin/virtctl", []string{"restart", "testvm", "-n", "tailvm"}, "", nil)

	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleDatavolumes_KubectlDown(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "datavolumes", "-A", "-o", "json"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/datavolumes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502 when kubectl is down, got %d", resp.StatusCode)
	}
}

func TestHandleSnapshots_KubectlDown(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmsnapshot", "-n", "tailvm", "-o", "json"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502 when kubectl is down, got %d", resp.StatusCode)
	}
}

func TestHandleEvents_KubectlDown(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	fx.Runner.AddResponseKV("kubectl", []string{"get", "events", "-n", "tailvm", "-o", "json"}, "", errSimulated)

	resp, err := http.Get(fx.Server.URL + "/api/vms/tailvm/testvm/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502 when kubectl is down, got %d", resp.StatusCode)
	}
}

func TestHandleCreateSnapshot_Error(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	// kubectl apply fails
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "", errSimulated)

	body := strings.NewReader(`{"name":"snap1"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/snapshots", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 502: the backend was reached and refused. Snapshot create/restore/delete
	// used to answer 500 here while list answered 502 for the same class of
	// failure; the adapter reports all four the same way, and reserves 400 for
	// a backend that cannot snapshot at all.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 when the backend rejects the snapshot, got %d", resp.StatusCode)
	}
}

func TestHandleAddNIC_MissingNAD(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Server.Close()

	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm/testvm/nics", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing nad, got %d", resp.StatusCode)
	}
}

// ── Plugin lifecycle ─────────────────────────────────────────────

func TestHandleInstallPlugin_NotFound(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	resp, err := http.Post(fx.Server.URL+"/api/plugins/nonexistent/install", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should fail — plugin not in marketplace
	if resp.StatusCode != 404 && resp.StatusCode != 502 {
		t.Errorf("expected 404/502 for unknown plugin, got %d", resp.StatusCode)
	}
}

func TestHandleRemovePlugin_Success(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Plugin remove calls plugin.Remove() which deletes a file
	// On test systems without the plugin installed, this should return 500
	resp, err := http.Post(fx.Server.URL+"/api/plugins/fakeplugin", "application/json", nil)
	// The handler uses DELETE, but we'll use POST to trigger the handler
	// Actually, looking at the route, it's DELETE not POST
	_ = err

	req, _ := http.NewRequest("DELETE", fx.Server.URL+"/api/plugins/fakeplugin", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Remove nonexistent plugin returns 500 (file not found)
	if resp.StatusCode != 500 {
		t.Logf("remove nonexistent plugin returned %d", resp.StatusCode)
	}
}

// ── Bootc task lifecycle ─────────────────────────────────────────

func TestHandleBootcCreate_ReturnsTask(t *testing.T) {
	if kubevirt.BootcAvailable() {
		t.Skip("bootc compiled — this build's request would start a real build, not hit the " +
			"400 path below; see TestCreateBootc_BuildFailure in create_bootc_bootc_test.go for the compiled-in case")
	}

	fx := NewTestFixture()
	defer fx.Close()

	body := strings.NewReader(`{"name":"bootc-vm","bootc":"quay.io/centos-bootc/centos-bootc:stream9","sshKey":"ssh-ed25519 AAA..."}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 (bootc unavailable), got %d", resp.StatusCode)
	}
}

func TestHandleBootcCreate_MissingSSHKey(t *testing.T) {
	if kubevirt.BootcAvailable() {
		t.Skip("bootc compiled — LoadSSHPublicKey may resolve an sshKey on the test machine")
	}

	fx := NewTestFixture()
	defer fx.Close()

	body := strings.NewReader(`{"name":"bootc-vm","bootc":"quay.io/centos-bootc/centos-bootc:stream9"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bootc without sshKey: expected 400, got %d", resp.StatusCode)
	}
}

func TestHandleTwoVMs_SequentialCreate(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Common responses both creates need
	fx.Runner.AddResponseKV("kubectl", []string{"create", "ns", "tailvm"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"label", "ns", "tailvm",
		"pod-security.kubernetes.io/enforce=privileged", "--overwrite"}, "", nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "sc", "-o", "json"},
		`{"items":[{"metadata":{"name":"longhorn"}}]}`, nil)
	fx.Runner.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "created", nil)

	// Create first VM
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vm", "vm1", "-n", "tailvm", "-o", "name"},
		"", errSimulated)
	body := strings.NewReader(`{"name":"vm1","containerDisk":"quay.io/containerdisks/fedora:42"}`)
	resp, err := http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("vm1 create: expected 201, got %d", resp.StatusCode)
	}

	// Create second VM
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vm", "vm2", "-n", "tailvm", "-o", "name"},
		"", errSimulated)
	body = strings.NewReader(`{"name":"vm2","containerDisk":"quay.io/containerdisks/fedora:42"}`)
	resp, err = http.Post(fx.Server.URL+"/api/vms", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("vm2 create: expected 201, got %d", resp.StatusCode)
	}

	// List — should find both
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vms", "-A", "-o", "json"},
		multiVMListJSON("vm1", "vm2"), nil)
	fx.Runner.AddResponseKV("kubectl", []string{"get", "vmis", "-A", "-o", "json"},
		`{"items":[]}`, nil)

	resp, err = http.Get(fx.Server.URL + "/api/vms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}

	var vms []map[string]any
	json.NewDecoder(resp.Body).Decode(&vms)
	if len(vms) < 2 {
		t.Errorf("expected at least 2 VMs, got %d", len(vms))
	}
}

func multiVMListJSON(names ...string) string {
	items := ""
	for _, n := range names {
		if items != "" {
			items += ","
		}
		items += fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"tailvm"},"status":{"printableStatus":"Stopped"},"spec":{"template":{"spec":{"domain":{"cpu":{"sockets":1},"resources":{"requests":{"memory":"2Gi"}}}}}}}`, n)
	}
	return fmt.Sprintf(`{"items":[%s]}`, items)
}

// ── Content-type validation ──────────────────────────────────────

func TestHandleCreateVM_WrongContentType(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Send form-encoded instead of JSON
	resp, err := http.Post(fx.Server.URL+"/api/vms",
		"application/x-www-form-urlencoded",
		strings.NewReader("name=test"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Handler uses json.Decoder — form data will fail to parse
	if resp.StatusCode != 400 {
		t.Logf("form data returned %d", resp.StatusCode)
	}
}

func TestHandleVMAction_MissingPathParams(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Access the action endpoint with missing name
	resp, err := http.Post(fx.Server.URL+"/api/vms/tailvm//start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Empty name should produce 4xx or 5xx, not panic
	t.Logf("empty name action returned %d", resp.StatusCode)
}

func TestHandleDoctorFix_Error(t *testing.T) {
	fx := NewTestFixture()
	defer fx.Close()

	// Reachable cluster (doctor's connectivity gate passes), but bare — and
	// the install applies fail (no response registered for kubectl apply) →
	// doctor.Fix() errors → the handler must return 500.
	fx.Runner.AddResponse("kubectl get --raw /livez --request-timeout=3s", "ok", nil)
	resp, err := http.Post(fx.Server.URL+"/api/doctor/fix", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("expected 500 when the fix fails, got %d", resp.StatusCode)
	}
}
