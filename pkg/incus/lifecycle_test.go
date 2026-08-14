package incus

// Tests for the Incus lifecycle surface (Start/Stop/Restart/Delete/Exists/
// Info/Pause) using the shell.Fake runner — pins the idempotency tolerances
// ("already running", "not running") and the force-stop fallback that keep
// the CLI usable against real Incus remotes.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
)

func TestStart_Success(t *testing.T) {
	f := fakeRemote(t, "[]")
	f.AddPrefixResponse("incus start", "started", nil)

	if err := NewClient("").Start("vm1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestStart_AlreadyRunningIsTolerated(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus start", "Error: The instance is already running", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if err := NewClient("").Start("vm1"); err != nil {
		t.Fatalf("Start with 'already running': %v", err)
	}
}

func TestStart_RealError(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus start", "Error: instance not found", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	err := NewClient("").Start("nope")
	if err == nil {
		t.Fatal("Start: expected error")
	}
	if !strings.Contains(err.Error(), "incus start nope") {
		t.Errorf("error = %v, want incus start wrap", err)
	}
}

func TestStop_AlreadyStoppedIsTolerated(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus stop", "Error: The instance is not running", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if err := NewClient("").Stop("vm1"); err != nil {
		t.Fatalf("Stop with 'not running': %v", err)
	}
}

func TestStop_FallsBackToForce(t *testing.T) {
	// First stop fails with a timeout-ish error; the --force retry succeeds.
	f := shell.NewFake()
	f.AddPrefixResponse("incus stop local:vm1", "operation timed out", fmt.Errorf("exit status 1"))
	f.AddPrefixResponse("incus stop local:vm1 --force", "", nil)
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if err := NewClient("").Stop("vm1"); err != nil {
		t.Fatalf("Stop with force fallback: %v", err)
	}
}

func TestStop_BothFailReturnsError(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus stop", "Error: some real failure", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	err := NewClient("").Stop("vm1")
	if err == nil {
		t.Fatal("Stop: expected error when stop and force-stop both fail")
	}
}

func TestRestart_SuccessAndError(t *testing.T) {
	f := fakeRemote(t, "[]")
	f.AddPrefixResponse("incus restart", "restarted", nil)
	if err := NewClient("").Restart("vm1"); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	f2 := shell.NewFake()
	f2.AddPrefixResponse("incus restart", "Error: no instance", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f2)
	t.Cleanup(func() { defaultRunner = old })
	if err := NewClient("").Restart("vm1"); err == nil {
		t.Fatal("Restart: expected error")
	}
}

func TestDelete_SuccessAndError(t *testing.T) {
	f := fakeRemote(t, "[]")
	f.AddPrefixResponse("incus delete", "deleted", nil)
	if err := NewClient("").Delete("vm1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	f2 := shell.NewFake()
	f2.AddPrefixResponse("incus delete", "Error: no instance", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f2)
	t.Cleanup(func() { defaultRunner = old })
	if err := NewClient("").Delete("vm1"); err == nil {
		t.Fatal("Delete: expected error")
	}
}

func TestExists_ToleratesMissing(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus info", "Error: instance not found", fmt.Errorf("exit status 1"))
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if NewClient("").Exists("nope") {
		t.Error("Exists(nope) = true, want false")
	}
}

func TestExists_Present(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus info", `{"name":"vm1"}`, nil)
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	if !NewClient("").Exists("vm1") {
		t.Error("Exists(vm1) = false, want true")
	}
}

func TestInfo_ReturnsRawOutput(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("incus info", `{"name":"vm1","status":"Running"}`, nil)
	old := defaultRunner
	SetRunner(f)
	t.Cleanup(func() { defaultRunner = old })

	out, err := NewClient("").Info("vm1")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !strings.Contains(string(out), "Running") {
		t.Errorf("Info output = %s, want status included", out)
	}
}

func TestPause_AndResume(t *testing.T) {
	f := fakeRemote(t, "[]")
	f.AddPrefixResponse("incus pause", "", nil)
	if err := NewClient("").Pause("vm1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Resume routes through Start.
	f.AddPrefixResponse("incus start", "", nil)
	if err := NewClient("").Resume("vm1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestPackageLifecycleWrappers(t *testing.T) {
	f := fakeRemote(t, "[]")
	f.AddPrefixResponse("incus start", "", nil)
	f.AddPrefixResponse("incus stop", "", nil)
	f.AddPrefixResponse("incus delete", "", nil)
	f.AddPrefixResponse("incus info", "", nil)

	if err := Start("vm1"); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := Stop("vm1"); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if err := Delete("vm1"); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if !Exists("vm1") {
		t.Error("Exists(): want true with successful info")
	}
	if _, err := Info("vm1"); err != nil {
		t.Fatalf("Info(): %v", err)
	}
}
