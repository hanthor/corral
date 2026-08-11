package web

// Move API tests.
//
// The assertions that matter are about the split between deciding and doing:
// that preflight never runs anything, that committing re-checks rather than
// trusting the client, and that a refusal arrives as a readable list instead of
// a failed request.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/move"
	"github.com/tuna-os/corral/pkg/types"
)

type planResponse struct {
	OK          bool `json:"ok"`
	Source      types.InstanceRef
	Destination types.InstanceRef
	Steps       []move.Step    `json:"steps"`
	Warnings    []string       `json:"warnings"`
	Dropped     []string       `json:"dropped"`
	Refusals    []move.Refusal `json:"refusals"`
	StopFirst   bool           `json:"stopFirst"`
	Format      string         `json:"format"`
}

type moveResponse struct {
	Destination   types.InstanceRef `json:"destination"`
	SourceStopped bool              `json:"sourceStopped"`
	SourceDeleted bool              `json:"sourceDeleted"`
	Warnings      []string          `json:"warnings"`
	Folder        string            `json:"folder"`
	Note          string            `json:"note"`
}

// fakeMove replaces move.Execute and records what it was asked to do. Nothing
// in this package should be running a real export.
type fakeMove struct {
	calls  []move.Plan
	result move.Result
	err    error
}

func stubMove(t *testing.T) *fakeMove {
	t.Helper()
	f := &fakeMove{result: move.Result{SourceStopped: true}}
	previous := moveRunner
	moveRunner = func(_ context.Context, plan move.Plan, _ move.ProgressFunc) (move.Result, error) {
		f.calls = append(f.calls, plan)
		if f.err != nil {
			return move.Result{}, f.err
		}
		out := f.result
		out.Destination = plan.Destination
		return out, nil
	}
	t.Cleanup(func() { moveRunner = previous })
	return f
}

// stubResolver answers with a fixed VM, so a test about refusals does not
// depend on what the demo fleet happens to contain.
func stubResolver(t *testing.T, vm types.VM) {
	t.Helper()
	previous := moveResolver
	moveResolver = func(_ *http.Request, ref types.InstanceRef) (types.VM, error) {
		vm.Name = ref.Name
		return vm, nil
	}
	t.Cleanup(func() { moveResolver = previous })
}

func movableVM() types.VM {
	return types.VM{
		Name: "web-1", Backend: "kubevirt", Context: "prod", Namespace: "default",
		CPU: 2, Mem: "4Gi", Disk: "10Gi",
	}
}

func preflight(t *testing.T, srv *httptest.Server, body map[string]any) (planResponse, int) {
	t.Helper()
	var out planResponse
	code := postJSON(t, srv, "/api/move/preflight", body, &out)
	return out, code
}

func TestMovePreflight_ReturnsThePlanAndRunsNothing(t *testing.T) {
	srv := newDemoServer(t)
	runner := stubMove(t)
	stubResolver(t, movableVM())

	plan, code := preflight(t, srv, map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "qemu"})
	if code != 200 {
		t.Fatalf("preflight returned %d", code)
	}
	if !plan.OK {
		t.Fatalf("kubevirt → qemu should be allowed, refusals: %v", plan.Refusals)
	}
	if len(plan.Steps) == 0 || len(plan.Warnings) == 0 {
		t.Errorf("the plan should carry steps and warnings, got %+v", plan)
	}
	if len(runner.calls) != 0 {
		t.Fatal("preflight must not execute anything")
	}
}

func TestMovePreflight_RefusalIsA200WithReasons(t *testing.T) {
	srv := newDemoServer(t)
	stubMove(t)
	stubResolver(t, movableVM())

	plan, code := preflight(t, srv, map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "vsphere"})
	if code != 200 {
		t.Fatalf("a refusal is an answer, not a failed request; got %d", code)
	}
	if plan.OK || len(plan.Refusals) == 0 {
		t.Fatal("an unknown destination cannot receive a move, so the plan should be refused with a reason")
	}
	if plan.Refusals[0].Remedy == "" {
		t.Error("a refusal without an explanation is a dead end in the dialog")
	}
	// The warnings still apply and are still worth reading.
	if len(plan.Warnings) == 0 {
		t.Error("warnings should survive a refusal — fixing it does not remove the IP change")
	}
}

func TestMove_RunsThePlanAndReportsTheOutcome(t *testing.T) {
	srv := newDemoServer(t)
	runner := stubMove(t)
	stubResolver(t, movableVM())

	var out moveResponse
	code := postJSON(t, srv, "/api/move",
		map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "qemu"}, &out)
	if code != 200 {
		t.Fatalf("move returned %d", code)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one execution, got %d", len(runner.calls))
	}
	if out.Destination.Backend != "qemu" || out.Destination.Name != "web-1" {
		t.Errorf("destination = %+v", out.Destination)
	}
	if !strings.Contains(out.Note, "created stopped") {
		t.Errorf("the response should say the instance is not running yet, got %q", out.Note)
	}
}

func TestMove_RefusesServerSideEvenIfTheClientSkippedPreflight(t *testing.T) {
	srv := newDemoServer(t)
	runner := stubMove(t)
	stubResolver(t, movableVM())

	var plan planResponse
	code := postJSON(t, srv, "/api/move",
		map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "vsphere"}, &plan)
	if code != 409 {
		t.Fatalf("a refused commit should be 409, got %d", code)
	}
	if len(runner.calls) != 0 {
		t.Fatal("posting straight to /api/move must not bypass the preflight")
	}
	if len(plan.Refusals) == 0 {
		t.Error("the 409 body should carry the same refusals the dialog showed")
	}
}

func TestMove_RecordsATaskLogEntry(t *testing.T) {
	srv := newDemoServer(t)
	stubMove(t)
	stubResolver(t, movableVM())

	postJSON(t, srv, "/api/move", map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "qemu"}, nil)

	var log []TaskEntry
	getJSON(t, srv, "/api/tasklog", &log)
	var found bool
	for _, e := range log {
		if e.Action == "move" && strings.Contains(e.Target, "qemu/web-1") {
			found = true
			if e.Status != "ok" {
				t.Errorf("task status = %q, want ok", e.Status)
			}
		}
	}
	if !found {
		t.Fatalf("a move should appear in the task log, got %+v", log)
	}
}

func TestMove_CarriesFolderMembershipToTheDestination(t *testing.T) {
	srv := newDemoServer(t)
	stubMove(t)
	stubResolver(t, movableVM())
	source := types.InstanceRef{Backend: "kubevirt", Context: "prod", Namespace: "default", Name: "web-1"}
	store := scratchFolders(t, folder.Folder{Path: "prod/web", Members: []types.InstanceRef{source}})

	var out moveResponse
	postJSON(t, srv, "/api/move", map[string]any{"ref": source.String(), "toBackend": "qemu"}, &out)
	if out.Folder != "prod/web" {
		t.Fatalf("the destination should land in the source's folder, got %q", out.Folder)
	}

	tree, err := store.Tree()
	if err != nil {
		t.Fatalf("reading the tree: %v", err)
	}
	dst := types.InstanceRef{Backend: "qemu", Name: "web-1"}
	if got := tree.PathOf(dst); got != "prod/web" {
		t.Fatalf("destination folder = %q, want prod/web", got)
	}
	// The source is stopped, not deleted, so leaving it in the folder is
	// accurate rather than stale.
	if got := tree.PathOf(source); got != "prod/web" {
		t.Errorf("the source should stay in its folder while it still exists, got %q", got)
	}
}

func TestMove_UnfolderedInstanceStaysUnfoldered(t *testing.T) {
	srv := newDemoServer(t)
	stubMove(t)
	stubResolver(t, movableVM())
	scratchFolders(t)

	var out moveResponse
	postJSON(t, srv, "/api/move",
		map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "qemu"}, &out)
	if out.Folder != "" {
		t.Fatalf("nothing to carry, got %q", out.Folder)
	}
}

func TestMove_SurfacesAnExecutionFailure(t *testing.T) {
	srv := newDemoServer(t)
	runner := stubMove(t)
	runner.err = errString("the pool is full")
	stubResolver(t, movableVM())

	code := postJSON(t, srv, "/api/move",
		map[string]any{"ref": "kubevirt/prod/default/web-1", "toBackend": "qemu"}, nil)
	if code != 500 {
		t.Fatalf("a failed move should be a 500, got %d", code)
	}
}

func TestMove_RejectsAnUnparseableRef(t *testing.T) {
	srv := newDemoServer(t)
	stubMove(t)

	if code := postJSON(t, srv, "/api/move/preflight", map[string]any{"ref": "", "toBackend": "qemu"}, nil); code != 400 {
		t.Fatalf("an empty ref should be a 400, got %d", code)
	}
}

func TestMoveDestinations_SaysWhichBackendsCanReceiveAndWhy(t *testing.T) {
	srv := newDemoServer(t)

	var out struct {
		Destinations []struct {
			Backend string `json:"backend"`
			Can     bool   `json:"can"`
			Reason  string `json:"reason"`
		} `json:"destinations"`
	}
	getJSON(t, srv, "/api/move/destinations", &out)

	byName := map[string]struct {
		can    bool
		reason string
	}{}
	for _, d := range out.Destinations {
		byName[d.Backend] = struct {
			can    bool
			reason string
		}{d.Can, d.Reason}
	}
	for _, want := range []string{"qemu", "libvirt", "kubevirt", "proxmox", "incus"} {
		if !byName[want].can {
			t.Errorf("%s implements Ingester and should be a live drop target", want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
