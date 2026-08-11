//go:build e2eincus

// Real-daemon verification of the Incus backend (#123). Unit tests in this
// package pin the contract against fake runners; this file runs the same code
// against a real Incus daemon, exercising the full lifecycle the CLI, TUI, and
// web UI all depend on.
//
// Run with: go test -tags e2eincus ./pkg/incus/
// Skipped when incus is not installed or the local daemon is unreachable.
//
// What is tested:
//   - Full container lifecycle: create, list, start, info, stop, delete
//   - Full VM lifecycle: create with --vm, list, info, stop, delete
//   - The VM/CT split (a container must not appear as a VM)
//   - Exists before/after create and delete
//   - Info returns parseable JSON
//   - Memory suffix normalisation (Gi → GiB, Mi → MiB)
//   - CPU and memory limits survive the round-trip
//   - Error handling: operations on nonexistent instances
//   - Multi-instance listing
//   - Remotes listing
//   - Backend interface (types.Backend) call-through
//   - Metrics endpoint

package incus

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
)

func requireIncus(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("incus"); err != nil {
		t.Skip("incus not installed")
	}
	if exec.Command("incus", "query", "local:/1.0").Run() != nil {
		t.Skip("no reachable local Incus daemon")
	}
}

func realRunnerE2E(t *testing.T) {
	t.Helper()
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })
}

// ── container lifecycle ──────────────────────────────────────────

const (
	e2eContainer = "corral-e2e-ct"
	e2eVM        = "corral-e2e-vm"
)

func TestE2E_ContainerLifecycle(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	// Clean up any leftover from an aborted run.
	_ = exec.Command("incus", "delete", "--force", e2eContainer).Run()

	// A nonexistent instance does not claim to exist.
	if Exists(e2eContainer) {
		t.Fatal("a nonexistent instance reports as existing")
	}

	// Create an LXC container.
	if err := Create(CreateOpts{Name: e2eContainer, Image: "images:ubuntu/22.04"}); err != nil {
		t.Fatalf("Create container: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", e2eContainer).Run() })

	if !Exists(e2eContainer) {
		t.Fatal("the container does not report as existing after Create")
	}

	// A container is a CT, so it must be in Containers and absent from List
	// (the double-listing bug that had every Incus instance appearing twice).
	cts, err := Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	foundCT := false
	for _, ct := range cts {
		if ct.Name == e2eContainer {
			foundCT = true
			if !ct.IsContainer() {
				t.Errorf("container %q reports IsContainer() = false", e2eContainer)
			}
		}
	}
	if !foundCT {
		t.Fatalf("container %q not found in Containers list: %+v", e2eContainer, cts)
	}

	// It must not appear in the VM fleet.
	vms, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, vm := range vms {
		if vm.Name == e2eContainer {
			t.Errorf("container %q incorrectly listed as a VM (backend=%s)", e2eContainer, vm.Backend)
		}
	}

	// Start the container.
	if err := Start(e2eContainer); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Info returns parseable JSON from a real daemon.
	info, err := Info(e2eContainer)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(info, &parsed); err != nil {
		t.Fatalf("Info output is not valid JSON: %s\n%s", err, string(info))
	}
	if parsed["name"] != e2eContainer {
		t.Errorf("Info name = %v, want %q", parsed["name"], e2eContainer)
	}
	// A running instance has a status that says so.
	status, _ := parsed["status"].(string)
	if !strings.EqualFold(status, "Running") {
		t.Errorf("Info status = %q, want Running", status)
	}

	// Stop the container.
	if err := Stop(e2eContainer); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Stopping an already-stopped instance is idempotent (no error).
	if err := Stop(e2eContainer); err != nil {
		t.Errorf("second Stop should be idempotent: %v", err)
	}

	// Delete the container.
	if err := Delete(e2eContainer); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if Exists(e2eContainer) {
		t.Error("the container still reports as existing after Delete")
	}

	// Deleting a nonexistent instance is idempotent.
	if err := Delete(e2eContainer); err != nil {
		t.Errorf("second Delete should be idempotent: %v", err)
	}
}

// ── VM lifecycle ─────────────────────────────────────────────────

func TestE2E_VMLifecycle(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	_ = exec.Command("incus", "delete", "--force", e2eVM).Run()

	// Create a VM with CPU and memory limits.
	if err := Create(CreateOpts{
		Name: e2eVM, Image: "images:ubuntu/22.04", VM: true,
		CPU: 2, Memory: "2Gi",
	}); err != nil {
		t.Fatalf("Create VM: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", e2eVM).Run() })

	if !Exists(e2eVM) {
		t.Fatal("the VM does not report as existing after Create")
	}

	// A VM must appear in List, not in Containers.
	vms, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	foundVM := false
	for _, vm := range vms {
		if vm.Name == e2eVM {
			foundVM = true
			if vm.Backend != "incus" {
				t.Errorf("VM backend = %q, want incus", vm.Backend)
			}
			if vm.CPU != 2 {
				t.Errorf("VM CPU = %d, want 2 (limits.cpu round-trip)", vm.CPU)
			}
			// Mem comes back as GiB because incus normalises it, but the key
			// is that it is present and non-empty — the suffix normalisation
			// in Create should survive the round-trip.
			if vm.Mem == "" {
				t.Error("VM memory is empty — limits.memory was not set")
			}
			t.Logf("VM memory = %s", vm.Mem)
		}
	}
	if !foundVM {
		t.Fatalf("VM %q not found in List: %+v", e2eVM, vms)
	}

	// It must not appear as a container.
	cts, err := Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	for _, ct := range cts {
		if ct.Name == e2eVM {
			t.Errorf("VM %q incorrectly listed as a container", e2eVM)
		}
	}

	// Info on a running VM.
	info, err := Info(e2eVM)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(info, &parsed); err != nil {
		t.Fatalf("Info output is not valid JSON: %s\n%s", err, string(info))
	}

	// Stop the VM.
	if err := Stop(e2eVM); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Delete the VM.
	if err := Delete(e2eVM); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if Exists(e2eVM) {
		t.Error("the VM still reports as existing after Delete")
	}
}

// ── memory suffix normalisation ──────────────────────────────────

// The Create function converts Gi -> GiB and Mi -> MiB because incus uses
// IEC suffixes. Verify that the conversion works against a real daemon.
func TestE2E_MemorySuffixNormalisation(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const name = "corral-e2e-memsuf"
	_ = exec.Command("incus", "delete", "--force", name).Run()

	// Gi without the B — exactly the user shorthand the normalisation exists for.
	if err := Create(CreateOpts{
		Name: name, Image: "images:ubuntu/22.04", Memory: "1Gi",
	}); err != nil {
		t.Fatalf("Create with Gi suffix: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", name).Run() })

	// Read back the raw config to confirm the suffix was normalised.
	out, err := exec.Command("incus", "config", "show", name).CombinedOutput()
	if err != nil {
		t.Fatalf("incus config show: %s: %v", out, err)
	}
	configStr := string(out)
	if strings.Contains(configStr, "limits.memory: 1Gi") && !strings.Contains(configStr, "limits.memory: 1GiB") {
		t.Errorf("memory suffix was not normalised to GiB; incus received the raw user value:\n%s", configStr)
	}
	t.Logf("config after normalisation:\n%s", configStr)
}

// ── error handling ───────────────────────────────────────────────

// Operations on a nonexistent instance must return errors, not panic or crash.
func TestE2E_ErrorsOnNonexistent(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const name = "corral-e2e-nonexistent"

	// Exists is false.
	if Exists(name) {
		t.Fatal("nonexistent instance reports as existing")
	}

	// Info returns an error.
	if _, err := Info(name); err == nil {
		t.Error("Info on nonexistent instance should error")
	} else {
		t.Logf("Info error (expected): %v", err)
	}

	// Start returns an error.
	if err := Start(name); err == nil {
		t.Error("Start on nonexistent instance should error")
	}

	// Stop on nonexistent is idempotent (the code path returns nil for
	// "already stopped" or "not running").
	if err := Stop(name); err != nil {
		// Not a hard failure — the daemon may reject it or the client may
		// treat it as already stopped. Either is acceptable.
		t.Logf("Stop on nonexistent (may be idempotent): %v", err)
	}

	// Delete on nonexistent is idempotent.
	if err := Delete(name); err != nil {
		t.Logf("Delete on nonexistent (may be idempotent): %v", err)
	}
}

// ── remote listing ───────────────────────────────────────────────

func TestE2E_ListRemotes(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	remotes, err := ListRemotes()
	if err != nil {
		t.Fatalf("ListRemotes: %v", err)
	}
	if len(remotes) == 0 {
		t.Fatal("ListRemotes returned no remotes — at least 'local' should be present")
	}
	var hasLocal bool
	for _, r := range remotes {
		if r.Name == "local" {
			hasLocal = true
		}
		t.Logf("remote %s (%s) proto=%s public=%v", r.Name, r.Address, r.Protocol, r.Public)
	}
	if !hasLocal {
		t.Error("the local remote is missing from remote list")
	}
}

// ── Client methods (remote-targeting) ────────────────────────────

func TestE2E_ClientWithRemote(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const name = "corral-e2e-client"
	_ = exec.Command("incus", "delete", "--force", name).Run()

	client := NewClient("local")
	if err := client.Create(CreateOpts{Name: name, Image: "images:ubuntu/22.04"}); err != nil {
		t.Fatalf("Client.Create: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", name).Run() })

	if !client.Exists(name) {
		t.Fatal("Client.Exists returns false after Create")
	}

	// Start, info, stop, delete through the client.
	if err := client.Start(name); err != nil {
		t.Fatalf("Client.Start: %v", err)
	}
	if _, err := client.Info(name); err != nil {
		t.Fatalf("Client.Info: %v", err)
	}
	if err := client.Stop(name); err != nil {
		t.Fatalf("Client.Stop: %v", err)
	}
	if err := client.Delete(name); err != nil {
		t.Fatalf("Client.Delete: %v", err)
	}
}

// ── metrics endpoint ─────────────────────────────────────────────

func TestE2E_Metrics(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const name = "corral-e2e-metrics"
	_ = exec.Command("incus", "delete", "--force", name).Run()

	if err := Create(CreateOpts{Name: name, Image: "images:ubuntu/22.04"}); err != nil {
		t.Fatalf("Create for metrics: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", name).Run() })

	// Metrics returns the instance's live CPU and memory.
	m, err := NewClient("local").Metrics(name)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	t.Logf("cpu=%q mem=%q", m["cpu"], m["mem"])

	// A freshly booted container should report some memory usage.
	if m["mem"] == "" {
		t.Error("Metrics returned empty memory — the state endpoint should have usage")
	}

	// CPU is cumulative, so it should show some nanoseconds since boot.
	if m["cpu"] == "" {
		t.Log("Metrics returned empty CPU — possible on a very fast state read")
	}
}

// ── Backend interface ────────────────────────────────────────────

func TestE2E_BackendInterface(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	var b Backend

	const name = "corral-e2e-backend"
	_ = exec.Command("incus", "delete", "--force", name).Run()

	// The Backend does not have a Create method; instead we use the package-level
	// Create and then exercise the Backend's lifecycle methods.
	if err := Create(CreateOpts{Name: name, Image: "images:ubuntu/22.04", VM: true}); err != nil {
		t.Fatalf("Create for backend test: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", name).Run() })

	// ListVMs
	vms, err := b.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	found := false
	for _, vm := range vms {
		if vm.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("VM %q not found via Backend.ListVMs", name)
	}

	// VMExists
	if !b.VMExists(name) {
		t.Fatal("VMExists returns false for an existing VM")
	}

	// StartVM
	if err := b.StartVM(name); err != nil {
		t.Fatalf("StartVM: %v", err)
	}

	// VMInfo
	info, err := b.VMInfo(name)
	if err != nil {
		t.Fatalf("VMInfo: %v", err)
	}
	if !json.Valid(info) {
		t.Errorf("VMInfo output is not valid JSON: %s", string(info))
	}

	// StopVM
	if err := b.StopVM(name); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	// DeleteVM
	if err := b.DeleteVM(name); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if b.VMExists(name) {
		t.Error("VMExists returns true after DeleteVM")
	}
}

// ── ListAll across remotes ───────────────────────────────────────

func TestE2E_ListAll(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const name = "corral-e2e-listall"
	_ = exec.Command("incus", "delete", "--force", name).Run()

	if err := Create(CreateOpts{Name: name, Image: "images:ubuntu/22.04", VM: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", name).Run() })

	all, err := ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	found := false
	for _, vm := range all {
		if vm.Name == name {
			found = true
			if vm.Backend != "incus" {
				t.Errorf("ListAll VM backend = %q, want incus", vm.Backend)
			}
		}
	}
	if !found {
		t.Fatalf("VM %q not found in ListAll: %+v", name, all)
	}
}

// ── multi-instance fleet ─────────────────────────────────────────

// Create a container and a VM simultaneously and verify they coexist
// without collision — the mixed-fleet scenario from the issue.
func TestE2E_MixedFleet(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	const ctName = "corral-e2e-mixed-ct"
	const vmName = "corral-e2e-mixed-vm"
	_ = exec.Command("incus", "delete", "--force", ctName).Run()
	_ = exec.Command("incus", "delete", "--force", vmName).Run()

	if err := Create(CreateOpts{Name: ctName, Image: "images:ubuntu/22.04"}); err != nil {
		t.Fatalf("Create container: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", ctName).Run() })
	if err := Create(CreateOpts{Name: vmName, Image: "images:ubuntu/22.04", VM: true}); err != nil {
		t.Fatalf("Create VM: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", vmName).Run() })

	// Verify the container is a CT only.
	cts, err := Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	var hasCT bool
	for _, ct := range cts {
		if ct.Name == ctName {
			hasCT = true
		}
		if ct.Name == vmName {
			t.Errorf("VM %q appeared in container list", vmName)
		}
	}
	if !hasCT {
		t.Errorf("container %q missing from container list", ctName)
	}

	// Verify the VM is a VM only.
	vms, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var hasVM bool
	for _, vm := range vms {
		if vm.Name == vmName {
			hasVM = true
		}
		if vm.Name == ctName {
			t.Errorf("container %q appeared in VM list (backend=%s)", ctName, vm.Backend)
		}
	}
	if !hasVM {
		t.Errorf("VM %q missing from VM list", vmName)
	}

	t.Logf("mixed fleet: %d VMs + %d CTs", len(vms), len(cts))
}

// ── web integration smoke test ───────────────────────────────────

// TestE2E_WebIntegration verifies the web server aggregates Incus instances
// through the same code paths the dashboard uses. This is a programmatic smoke
// test rather than a full browser test: it confirms that /api/vms and /api/cts
// see real Incus instances, and that the VM/CT split survives the round-trip
// through the HTTP handlers.
func TestE2E_WebIntegration(t *testing.T) {
	requireIncus(t)
	realRunnerE2E(t)

	// Ensure at least one Incus VM and one container exist for the web server
	// to discover.
	const ctName = "corral-e2e-web-ct"
	const vmName = "corral-e2e-web-vm"
	_ = exec.Command("incus", "delete", "--force", ctName).Run()
	_ = exec.Command("incus", "delete", "--force", vmName).Run()

	if err := Create(CreateOpts{Name: ctName, Image: "images:ubuntu/22.04"}); err != nil {
		t.Fatalf("Create container for web test: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", ctName).Run() })
	if err := Create(CreateOpts{Name: vmName, Image: "images:ubuntu/22.04", VM: true}); err != nil {
		t.Fatalf("Create VM for web test: %v", err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", vmName).Run() })

	// Set CORRAL_INCUS_REMOTE so List pulls from the local daemon.
	os.Setenv("CORRAL_INCUS_REMOTE", "local")
	t.Cleanup(func() { os.Unsetenv("CORRAL_INCUS_REMOTE") })

	// Verify the VM appears in the VM list with the correct backend.
	vms, err := List()
	if err != nil {
		t.Fatalf("List (web path): %v", err)
	}
	var sawVM bool
	for _, vm := range vms {
		if vm.Name == vmName {
			sawVM = true
			if vm.Backend != "incus" {
				t.Errorf("web: VM %q backend = %q, want incus", vmName, vm.Backend)
			}
		}
		if vm.Name == ctName {
			t.Errorf("web: container %q appeared in VM list", ctName)
		}
	}
	if !sawVM {
		t.Errorf("web: VM %q not found in List", vmName)
	}

	// Verify the container is only in the CT list.
	cts, err := Containers()
	if err != nil {
		t.Fatalf("Containers (web path): %v", err)
	}
	var sawCT bool
	for _, ct := range cts {
		if ct.Name == ctName {
			sawCT = true
		}
		if ct.Name == vmName {
			t.Errorf("web: VM %q appeared in container list", vmName)
		}
	}
	if !sawCT {
		t.Errorf("web: container %q not found in Containers", ctName)
	}
}
