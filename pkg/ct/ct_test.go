package ct

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
)

func withFake(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()
	SetRunner(fake)
	t.Cleanup(func() { SetRunner(shell.Real{}) })
	return fake
}

func appliedManifests(t *testing.T, r *shell.Fake) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range r.Calls() {
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "apply" {
			var m map[string]any
			if err := json.Unmarshal([]byte(c.Stdin), &m); err != nil {
				t.Fatalf("applied manifest not valid JSON: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestCreate_AppliesPVCThenPod(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	err := Create(CreateOpts{
		Name: "web1", Namespace: "corral-ct", Image: "debian:bookworm",
		CPU: 2, Mem: "1Gi", Disk: "10Gi",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	manifests := appliedManifests(t, r)
	if len(manifests) != 2 {
		t.Fatalf("applied %d manifests, want 2 (PVC + pod)", len(manifests))
	}

	pvc := manifests[0]
	if pvc["kind"] != "PersistentVolumeClaim" {
		t.Fatalf("first manifest kind = %v, want PersistentVolumeClaim", pvc["kind"])
	}
	pvcMeta := pvc["metadata"].(map[string]any)
	if pvcMeta["name"] != "web1-data" {
		t.Errorf("PVC name = %v, want web1-data", pvcMeta["name"])
	}
	annotations := pvcMeta["annotations"].(map[string]any)
	var spec ctSpec
	if err := json.Unmarshal([]byte(annotations[specAnnotation].(string)), &spec); err != nil {
		t.Fatalf("spec annotation not valid JSON: %v", err)
	}
	if spec.Image != "debian:bookworm" || spec.CPU != 2 || spec.Mem != "1Gi" {
		t.Errorf("persisted spec = %+v", spec)
	}

	pod := manifests[1]
	if pod["kind"] != "Pod" {
		t.Fatalf("second manifest kind = %v, want Pod", pod["kind"])
	}
	podMeta := pod["metadata"].(map[string]any)
	if podMeta["name"] != "web1" {
		t.Errorf("pod name = %v, want web1", podMeta["name"])
	}
	podSpec := pod["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	if container["image"] != "debian:bookworm" {
		t.Errorf("container image = %v", container["image"])
	}
	sc := container["securityContext"].(map[string]any)
	if sc["privileged"] != false {
		t.Errorf("expected unprivileged by default, got %v", sc)
	}
}

func TestCreate_PrivilegedOptIn(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	if err := Create(CreateOpts{Name: "priv1", Namespace: "corral-ct", Image: "debian", Privileged: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	manifests := appliedManifests(t, r)
	pod := manifests[1]
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	sc := container["securityContext"].(map[string]any)
	if sc["privileged"] != true {
		t.Errorf("expected privileged: true, got %v", sc)
	}
}

func TestCreate_PrivilegedGetsPersistentRootfs(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	if err := Create(CreateOpts{Name: "priv1", Namespace: "corral-ct", Image: "debian", Privileged: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	manifests := appliedManifests(t, r)
	pod := manifests[1]
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)

	mounts := container["volumeMounts"].([]any)
	mount := mounts[0].(map[string]any)
	if mount["mountPath"] != rootfsMountPath {
		t.Errorf("privileged CT mountPath = %v, want %s", mount["mountPath"], rootfsMountPath)
	}

	cmd := container["command"].([]any)
	joined := fmt.Sprint(cmd)
	for _, want := range []string{"cp -a --one-file-system", "chroot", rootfsMountPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("privileged CT command missing %q:\n%v", want, cmd)
		}
	}
}

func TestCreate_UnprivilegedKeepsDataMountAndSleepInfinity(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	if err := Create(CreateOpts{Name: "web2", Namespace: "corral-ct", Image: "debian"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	manifests := appliedManifests(t, r)
	pod := manifests[1]
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)

	mounts := container["volumeMounts"].([]any)
	mount := mounts[0].(map[string]any)
	if mount["mountPath"] != dataMountPath {
		t.Errorf("unprivileged CT mountPath = %v, want %s", mount["mountPath"], dataMountPath)
	}
	cmd := container["command"].([]any)
	if fmt.Sprint(cmd) != fmt.Sprint([]string{"sleep", "infinity"}) {
		t.Errorf("unprivileged CT command = %v, want sleep infinity", cmd)
	}
}

func TestExecCommand_PrivilegedRechroots(t *testing.T) {
	withFake(t).AddResponseKV("kubectl", []string{
		"get", "pvc", "priv1-data", "-n", "corral-ct", "-o",
		`jsonpath={.metadata.annotations.corral\.ct/spec}`,
	}, `{"image":"debian","cpu":1,"mem":"512Mi","privileged":true}`, nil)

	cmd, err := ExecCommand("priv1", "corral-ct")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "chroot") || !strings.Contains(joined, rootfsMountPath) {
		t.Errorf("privileged ExecCommand = %v, want a chroot into %s", cmd, rootfsMountPath)
	}
}

func TestExecCommand_UnprivilegedPlainShell(t *testing.T) {
	withFake(t).AddResponseKV("kubectl", []string{
		"get", "pvc", "web2-data", "-n", "corral-ct", "-o",
		`jsonpath={.metadata.annotations.corral\.ct/spec}`,
	}, `{"image":"debian","cpu":1,"mem":"512Mi","privileged":false}`, nil)

	cmd, err := ExecCommand("web2", "corral-ct")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	if len(cmd) != 1 || cmd[0] != "sh" {
		t.Errorf("unprivileged ExecCommand = %v, want [sh]", cmd)
	}
}

func TestStart_RecreatesPodFromPVCAnnotation(t *testing.T) {
	r := withFake(t)
	specJSON := `{"image":"debian:bookworm","cpu":4,"mem":"2Gi","privileged":false}`
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "web1-data", "-n", "corral-ct", "-o",
		`jsonpath={.metadata.annotations.corral\.ct/spec}`,
	}, specJSON, nil)
	r.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)

	if err := Start("web1", "corral-ct"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	manifests := appliedManifests(t, r)
	if len(manifests) != 1 {
		t.Fatalf("applied %d manifests, want 1 (just the pod)", len(manifests))
	}
	pod := manifests[0]
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if container["image"] != "debian:bookworm" {
		t.Errorf("recreated pod image = %v, want debian:bookworm (from PVC annotation)", container["image"])
	}
	res := container["resources"].(map[string]any)["limits"].(map[string]any)
	if res["cpu"] != "4" || res["memory"] != "2Gi" {
		t.Errorf("recreated pod resources = %v, want cpu=4 mem=2Gi", res)
	}
}

func TestStart_NoExistingCT_Errors(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "ghost-data", "-n", "corral-ct", "-o",
		`jsonpath={.metadata.annotations.corral\.ct/spec}`,
	}, "", &fakeErr{"not found"})

	if err := Start("ghost", "corral-ct"); err == nil {
		t.Error("expected an error starting a CT with no data PVC")
	}
}

func TestStop_DeletesPodOnly(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"delete", "pod", "web1", "-n", "corral-ct", "--ignore-not-found"}, "", nil)

	if err := Stop("web1", "corral-ct"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, c := range r.Calls() {
		if len(c.Args) > 1 && c.Args[0] == "delete" && c.Args[1] == "pvc" {
			t.Error("Stop must not delete the data PVC")
		}
	}
}

func TestDelete_RemovesPodServiceAndPVC(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"delete", "pod", "web1", "-n", "corral-ct", "--ignore-not-found"}, "", nil)
	r.AddResponseKV("kubectl", []string{"delete", "svc", "web1-svc", "-n", "corral-ct", "--ignore-not-found"}, "", nil)
	r.AddResponseKV("kubectl", []string{"delete", "pvc", "web1-data", "-n", "corral-ct", "--ignore-not-found"}, "", nil)

	if err := Delete("web1", "corral-ct"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deleted := map[string]bool{}
	for _, c := range r.Calls() {
		if len(c.Args) > 1 && c.Args[0] == "delete" {
			deleted[c.Args[1]] = true
		}
	}
	for _, kind := range []string{"pod", "svc", "pvc"} {
		if !deleted[kind] {
			t.Errorf("expected a delete call for %q", kind)
		}
	}
}

func TestListCTs_MergesStartedAndStoppedState(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"get", "pvc", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[
		{"metadata":{"name":"web1-data","namespace":"corral-ct","labels":{"corral.dev/ct-name":"web1"},
			"annotations":{"corral.ct/spec":"{\"image\":\"debian\",\"cpu\":2,\"mem\":\"1Gi\"}"}}},
		{"metadata":{"name":"stopped1-data","namespace":"corral-ct","labels":{"corral.dev/ct-name":"stopped1"},
			"annotations":{"corral.ct/spec":"{\"image\":\"alpine\",\"cpu\":1,\"mem\":\"512Mi\"}"}}}
	]}`, nil)
	r.AddResponseKV("kubectl", []string{"get", "pods", "-A", "-l", "corral.dev/ct=true", "-o", "json"}, `{"items":[
		{"metadata":{"name":"web1","namespace":"corral-ct"},
			"spec":{"nodeName":"node-a"},
			"status":{"phase":"Running","containerStatuses":[{"ready":true}]}}
	]}`, nil)

	cts, err := ListCTs()
	if err != nil {
		t.Fatalf("ListCTs: %v", err)
	}
	if len(cts) != 2 {
		t.Fatalf("got %d CTs, want 2", len(cts))
	}
	byName := map[string]CT{}
	for _, c := range cts {
		byName[c.Name] = c
	}
	if byName["web1"].Phase != "Running" || !byName["web1"].Ready {
		t.Errorf("web1 = %+v, want Running+Ready (has a live pod)", byName["web1"])
	}
	if byName["web1"].Node != "node-a" {
		t.Errorf("web1.Node = %q, want node-a", byName["web1"].Node)
	}
	if byName["stopped1"].Phase != "Stopped" || byName["stopped1"].Ready {
		t.Errorf("stopped1 = %+v, want Stopped (no pod, PVC only)", byName["stopped1"])
	}
	if byName["stopped1"].Node != "" {
		t.Errorf("stopped1.Node = %q, want empty (no pod)", byName["stopped1"].Node)
	}
}

func TestExists(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{"get", "pvc", "web1-data", "-n", "corral-ct", "-o", "name"}, "persistentvolumeclaim/web1-data", nil)

	if !Exists("web1", "corral-ct") {
		t.Error("expected Exists to be true")
	}
	if Exists("ghost", "corral-ct") {
		t.Error("expected Exists to be false for an unregistered command")
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func TestGeneratePod_InitUsesImageEntrypoint(t *testing.T) {
	// Default unprivileged: command forced to sleep (generic images exit).
	pod := generatePod("a", "ns", ctSpec{Image: "debian:12"})
	ctr := pod["spec"].(map[string]any)["containers"].([]map[string]any)[0]
	if _, ok := ctr["command"]; !ok {
		t.Error("default unprivileged CT should force a sleep command")
	}

	// Init: no command override — the curated image's entrypoint is PID 1.
	pod = generatePod("b", "ns", ctSpec{Image: "ghcr.io/tuna-os/ct-debian:latest", Init: true})
	ctr = pod["spec"].(map[string]any)["containers"].([]map[string]any)[0]
	if _, ok := ctr["command"]; ok {
		t.Errorf("Init CT should use the image entrypoint, got command %v", ctr["command"])
	}

	// Privileged ignores Init — the bootstrap chroot script is its init.
	pod = generatePod("c", "ns", ctSpec{Image: "debian:12", Privileged: true, Init: true})
	ctr = pod["spec"].(map[string]any)["containers"].([]map[string]any)[0]
	if cmd, ok := ctr["command"].([]string); !ok || len(cmd) == 0 || cmd[0] != "/bin/sh" {
		t.Errorf("privileged CT must keep the bootstrap command, got %v", ctr["command"])
	}
}

func TestScale_UpdatesSpecAnnotation(t *testing.T) {
	r := withFake(t)

	oldSpec := `{"image":"alpine","cpu":1,"mem":"512Mi","privileged":false}`
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "test-scale-data", "-n", "ns",
		"-o", "jsonpath={.metadata.annotations.corral\\.ct/spec}",
	}, oldSpec, nil)

	// PVC patch — dynamic -p value, use prefix.
	r.AddPrefixResponse("kubectl patch pvc test-scale-data -n ns --type=json -p", "", nil)

	// Pod doesn't exist (stopped CT).
	r.AddResponseKV("kubectl", []string{
		"get", "pod", "test-scale", "-n", "ns",
		"-o", "name", "--ignore-not-found",
	}, "", nil)

	if err := Scale("test-scale", "ns", 2, "1Gi"); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	// Verify the PVC patch contained the updated spec.
	calls := r.Calls()
	var patchCall *shell.Call
	for i := range calls {
		if len(calls[i].Args) > 0 && calls[i].Args[0] == "patch" && calls[i].Args[1] == "pvc" {
			patchCall = &calls[i]
			break
		}
	}
	if patchCall == nil {
		t.Fatal("expected a kubectl patch pvc call")
	}
	if !containsArg(patchCall.Args, `"cpu":2`) {
		t.Errorf("PVC patch should contain cpu:2, got args: %v", patchCall.Args)
	}
	if !containsArg(patchCall.Args, `"mem":"1Gi"`) {
		t.Errorf("PVC patch should contain mem:1Gi, got args: %v", patchCall.Args)
	}
}

func TestScale_PatchesRunningPod(t *testing.T) {
	r := withFake(t)

	oldSpec := `{"image":"alpine","cpu":1,"mem":"512Mi","privileged":false}`
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "test-scale-data", "-n", "ns",
		"-o", "jsonpath={.metadata.annotations.corral\\.ct/spec}",
	}, oldSpec, nil)

	r.AddPrefixResponse("kubectl patch pvc test-scale-data -n ns --type=json -p", "", nil)
	r.AddPrefixResponse("kubectl patch pod test-scale -n ns --type=json -p", "", nil)

	// Pod exists.
	r.AddResponseKV("kubectl", []string{
		"get", "pod", "test-scale", "-n", "ns",
		"-o", "name", "--ignore-not-found",
	}, "pod/test-scale", nil)

	if err := Scale("test-scale", "ns", 4, "4Gi"); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	// Verify pod patch was issued.
	found := false
	for _, c := range r.Calls() {
		if len(c.Args) > 1 && c.Args[0] == "patch" && c.Args[1] == "pod" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a kubectl patch pod call for running CT")
	}
}

func TestScale_PartialUpdate(t *testing.T) {
	r := withFake(t)

	oldSpec := `{"image":"alpine","cpu":1,"mem":"512Mi","privileged":false}`
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "test-scale-data", "-n", "ns",
		"-o", "jsonpath={.metadata.annotations.corral\\.ct/spec}",
	}, oldSpec, nil)

	r.AddPrefixResponse("kubectl patch pvc test-scale-data -n ns --type=json -p", "", nil)
	r.AddPrefixResponse("kubectl patch pod test-scale -n ns --type=json -p", "", nil)

	r.AddResponseKV("kubectl", []string{
		"get", "pod", "test-scale", "-n", "ns",
		"-o", "name", "--ignore-not-found",
	}, "pod/test-scale", nil)

	if err := Scale("test-scale", "ns", 2, ""); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	// Pod patch should NOT contain memory changes.
	for _, c := range r.Calls() {
		if len(c.Args) > 1 && c.Args[0] == "patch" && c.Args[1] == "pod" {
			for _, a := range c.Args {
				if len(a) > 10 && containsStr(a, "memory") {
					t.Errorf("pod patch should NOT contain memory when mem is empty, got: %v", c.Args)
				}
			}
		}
	}
}

func TestScale_ZeroIsNoop(t *testing.T) {
	r := withFake(t)
	r.AddResponseKV("kubectl", []string{
		"get", "pvc", "test-scale-data", "-n", "ns",
		"-o", "jsonpath={.metadata.annotations.corral\\.ct/spec}",
	}, `{"image":"alpine","cpu":1,"mem":"512Mi","privileged":false}`, nil)
	r.AddPrefixResponse("kubectl patch pvc test-scale-data -n ns --type=json -p", "", nil)
	r.AddResponseKV("kubectl", []string{
		"get", "pod", "test-scale", "-n", "ns",
		"-o", "name", "--ignore-not-found",
	}, "", nil)

	if err := Scale("test-scale", "ns", 0, ""); err != nil {
		t.Fatalf("Scale with zeros should not error: %v", err)
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func containsArg(args []string, substr string) bool {
	for _, a := range args {
		if containsStr(a, substr) {
			return true
		}
	}
	return false
}
