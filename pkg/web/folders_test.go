package web

// Folder API tests.
//
// The interesting assertions are about honesty rather than plumbing: that a
// folder listing shows a member whose context is down instead of hiding it, that
// removing a folder does not touch an instance, and that a bulk action reports
// per-member outcomes with skipped distinguished from failed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/types"
)

// scratchFolders points the API at an in-memory tree, so no test writes an
// operator's config file.
func scratchFolders(t *testing.T, folders ...folder.Folder) *folder.Store {
	t.Helper()
	store := folder.NewStore(folder.NewMemoryBackend(folders...))
	previous := folderStore
	SetFolderStore(store)
	t.Cleanup(func() { SetFolderStore(previous) })
	return store
}

func demoRef(t *testing.T, srv *httptest.Server, name string) types.InstanceRef {
	t.Helper()
	var inventory struct {
		VMs []types.VM `json:"vms"`
	}
	getJSON(t, srv, "/api/v1/inventory", &inventory)
	for _, vm := range inventory.VMs {
		if vm.Name == name {
			return vm.Ref()
		}
	}
	t.Fatalf("demo fleet has no VM named %q", name)
	return types.InstanceRef{}
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body any, out any) int {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling body: %v", err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func deleteReq(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

type foldersResponse struct {
	Folders []struct {
		Path    string     `json:"path"`
		Parent  string     `json:"parent"`
		Members []types.VM `json:"members"`
		Missing []string   `json:"missing"`
	} `json:"folders"`
	Unfoldered []types.VM `json:"unfoldered"`
}

func TestFolders_CreateNestedAndList(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	if code := postJSON(t, srv, "/api/folders", map[string]string{"path": "prod/web"}, nil); code != 200 {
		t.Fatalf("create returned %d", code)
	}

	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	paths := map[string]string{}
	for _, f := range got.Folders {
		paths[f.Path] = f.Parent
	}
	// The ancestor is created too, or the tree has an orphan.
	if _, ok := paths["prod"]; !ok {
		t.Errorf("creating prod/web did not create prod: %v", paths)
	}
	if paths["prod/web"] != "prod" {
		t.Errorf("prod/web's parent = %q, want prod", paths["prod/web"])
	}
	// Every instance starts unfoldered, which is what the tree's root shows.
	if len(got.Unfoldered) == 0 {
		t.Error("no unfoldered instances reported; the folder view would look empty")
	}
}

func TestFolders_RejectsAnUnusablePath(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	for _, path := range []string{"", "prod//web", "prod/../etc"} {
		var body map[string]string
		code := postJSON(t, srv, "/api/folders", map[string]string{"path": path}, &body)
		// A rejected path is the caller's mistake: retrying cannot help, so 400.
		if code != http.StatusBadRequest {
			t.Errorf("creating %q returned %d, want 400", path, code)
		}
		if body["error"] == "" {
			t.Errorf("creating %q returned no reason", path)
		}
	}
}

func TestFolders_AssignIsSingleParentAndResolvesMembers(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	ref := demoRef(t, srv, "web-prod")
	if code := postJSON(t, srv, "/api/folders/members",
		map[string]string{"path": "prod/web", "ref": ref.String()}, nil); code != 200 {
		t.Fatalf("assign returned %d", code)
	}

	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	found := false
	for _, f := range got.Folders {
		for _, member := range f.Members {
			if member.Name != "web-prod" {
				continue
			}
			found = true
			if f.Path != "prod/web" {
				t.Errorf("web-prod is in %q", f.Path)
			}
			// Members come back as instances, so a client does not have to
			// resolve selectors itself.
			if member.Backend == "" || member.Status == "" {
				t.Errorf("member returned unresolved: %+v", member)
			}
		}
	}
	if !found {
		t.Fatal("the assigned member is not in the listing")
	}
	for _, vm := range got.Unfoldered {
		if vm.Name == "web-prod" {
			t.Error("web-prod is both foldered and unfoldered")
		}
	}

	// Re-assigning moves it: one folder per instance.
	if code := postJSON(t, srv, "/api/folders/members",
		map[string]string{"path": "lab", "ref": ref.String()}, nil); code != 200 {
		t.Fatal("re-assign failed")
	}
	getJSON(t, srv, "/api/folders", &got)
	holders := []string{}
	for _, f := range got.Folders {
		for _, member := range f.Members {
			if member.Name == "web-prod" {
				holders = append(holders, f.Path)
			}
		}
	}
	if len(holders) != 1 || holders[0] != "lab" {
		t.Errorf("web-prod is in %v, want only lab", holders)
	}
}

// A member whose instance is gone (or whose context is down) is reported, not
// hidden — a partial fleet is normal, and a folder pointing at nothing is
// information.
func TestFolders_MissingMembersAreReported(t *testing.T) {
	srv := newDemoServer(t)
	ghost := types.InstanceRef{Backend: "kubevirt", Context: "gone", Namespace: "corral-vms", Name: "ghost"}
	scratchFolders(t, folder.Folder{Path: "prod", Members: []types.InstanceRef{ghost}})

	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	for _, f := range got.Folders {
		if f.Path != "prod" {
			continue
		}
		if len(f.Missing) != 1 || !strings.Contains(f.Missing[0], "ghost") {
			t.Errorf("missing = %v, want the absent member reported", f.Missing)
		}
		if len(f.Members) != 0 {
			t.Errorf("members = %v, want none resolvable", f.Members)
		}
		return
	}
	t.Fatal("the folder is not in the listing")
}

func TestFolders_MoveRetargetsTheSubtree(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	postJSON(t, srv, "/api/folders", map[string]string{"path": "prod/web/db"}, nil)
	postJSON(t, srv, "/api/folders", map[string]string{"path": "lab"}, nil)

	if code := postJSON(t, srv, "/api/folders/move",
		map[string]string{"from": "prod/web", "to": "lab/web"}, nil); code != 200 {
		t.Fatalf("move returned %d", code)
	}
	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	paths := map[string]bool{}
	for _, f := range got.Folders {
		paths[f.Path] = true
	}
	if !paths["lab/web"] || !paths["lab/web/db"] {
		t.Errorf("the subtree did not follow the move: %v", paths)
	}
	if paths["prod/web"] {
		t.Error("the old path is still present")
	}

	// The unrecoverable move is refused, and the tree survives.
	if code := postJSON(t, srv, "/api/folders/move",
		map[string]string{"from": "lab", "to": "lab/web/inner"}, nil); code != http.StatusBadRequest {
		t.Errorf("moving a folder into its own descendant returned %d, want 400", code)
	}
}

// Removing a folder must never remove an instance. The response says so too,
// because the operator clicking it deserves to know.
func TestFolders_DeleteUnfoldersRatherThanDeletes(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	ref := demoRef(t, srv, "web-prod")
	postJSON(t, srv, "/api/folders/members", map[string]string{"path": "prod", "ref": ref.String()}, nil)

	if code := deleteReq(t, srv, "/api/folders?path=prod"); code != 200 {
		t.Fatalf("delete returned %d", code)
	}

	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	if len(got.Folders) != 0 {
		t.Errorf("folders remain: %+v", got.Folders)
	}
	stillThere := false
	for _, vm := range got.Unfoldered {
		if vm.Name == "web-prod" {
			stillThere = true
		}
	}
	if !stillThere {
		t.Error("the instance vanished with its folder")
	}

	// And the VM is still in the fleet.
	var inventory struct {
		VMs []types.VM `json:"vms"`
	}
	getJSON(t, srv, "/api/v1/inventory", &inventory)
	for _, vm := range inventory.VMs {
		if vm.Name == "web-prod" {
			return
		}
	}
	t.Fatal("removing a folder deleted the VM")
}

func TestFolders_UnassignLeavesTheFolder(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	ref := demoRef(t, srv, "web-prod")
	postJSON(t, srv, "/api/folders/members", map[string]string{"path": "prod", "ref": ref.String()}, nil)

	if code := deleteReq(t, srv, "/api/folders/members?ref="+ref.String()); code != 200 {
		t.Fatalf("unassign returned %d", code)
	}
	var got foldersResponse
	getJSON(t, srv, "/api/folders", &got)
	if len(got.Folders) != 1 || got.Folders[0].Path != "prod" {
		t.Errorf("the folder should survive losing its last member: %+v", got.Folders)
	}
	if len(got.Folders[0].Members) != 0 {
		t.Errorf("members = %+v, want empty", got.Folders[0].Members)
	}
}

// ── bulk actions ──────────────────────────────────────────────────

type actionResponse struct {
	Path    string          `json:"path"`
	Action  string          `json:"action"`
	OK      int             `json:"ok"`
	Failed  int             `json:"failed"`
	Skipped int             `json:"skipped"`
	Members []MemberOutcome `json:"members"`
}

func TestFolderAction_StopsEveryRunningMember(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	for _, name := range []string{"web-prod", "db-prod"} {
		ref := demoRef(t, srv, name)
		postJSON(t, srv, "/api/folders/members",
			map[string]string{"path": "prod", "ref": ref.String()}, nil)
	}

	var got actionResponse
	code := postJSON(t, srv, "/api/folders/action",
		map[string]any{"path": "prod", "action": "stop"}, &got)
	if code != 200 {
		t.Fatalf("action returned %d", code)
	}
	if got.OK != 2 || got.Failed != 0 {
		t.Errorf("outcome = %d ok / %d failed / %d skipped: %+v",
			got.OK, got.Failed, got.Skipped, got.Members)
	}
	// Per-member results, not just a count: that is what makes a partial failure
	// diagnosable.
	if len(got.Members) != 2 {
		t.Fatalf("members = %+v, want one entry each", got.Members)
	}
	for _, member := range got.Members {
		if member.Name == "" || member.Backend == "" {
			t.Errorf("outcome does not identify the member: %+v", member)
		}
	}

	// And the fleet actually changed.
	var inventory struct {
		VMs []types.VM `json:"vms"`
	}
	getJSON(t, srv, "/api/v1/inventory", &inventory)
	for _, vm := range inventory.VMs {
		if (vm.Name == "web-prod" || vm.Name == "db-prod") && vm.Running {
			t.Errorf("%s is still running after the folder was stopped", vm.Name)
		}
	}
}

// An action that does not apply is skipped, not failed: asking a stopped VM to
// stop is a no-op, and counting it as a failure would make a healthy fan-out
// look broken.
func TestFolderAction_SkippedIsNotFailed(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	ref := demoRef(t, srv, "dev-fedora") // stopped in the demo fleet
	postJSON(t, srv, "/api/folders/members", map[string]string{"path": "lab", "ref": ref.String()}, nil)

	var got actionResponse
	postJSON(t, srv, "/api/folders/action", map[string]any{"path": "lab", "action": "stop"}, &got)
	if got.Skipped != 1 || got.Failed != 0 || got.OK != 0 {
		t.Errorf("outcome = %d ok / %d failed / %d skipped: %+v",
			got.OK, got.Failed, got.Skipped, got.Members)
	}
	if len(got.Members) != 1 || !got.Members[0].Skipped {
		t.Fatalf("members = %+v, want one skipped", got.Members)
	}
	if !strings.Contains(got.Members[0].Error, "not running") {
		t.Errorf("skip reason = %q, want it to say why", got.Members[0].Error)
	}
}

// A heterogeneous folder is the normal case, and an action valid for one backend
// may be unsupported on another. The response says which member, which backend,
// A pool spans backends, so a bulk action fans out to several at once and the
// response says what each member did.
//
// This used to assert a *refusal* — pause was KubeVirt-only, so a pool holding
// an Incus VM produced one ok and one "not supported". Every backend implements
// Power, Restarter and Suspender now, so the folder actions succeed across all
// of them and the honest assertion is that they do.
// TestFolderAction_ReportsAnUnreachableBackend keeps the refusal *reporting*
// covered, since that path is still reachable.
func TestFolderAction_FansOutAcrossBackends(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	for _, name := range []string{"web-prod", "incus-demo-vm"} {
		ref := demoRef(t, srv, name)
		postJSON(t, srv, "/api/folders/members",
			map[string]string{"path": "stack", "ref": ref.String()}, nil)
	}

	var got actionResponse
	postJSON(t, srv, "/api/folders/action", map[string]any{"path": "stack", "action": "pause"}, &got)
	if got.Failed != 0 {
		t.Errorf("outcome = %d ok / %d failed / %d skipped: %+v",
			got.OK, got.Failed, got.Skipped, got.Members)
	}
	backends := map[string]bool{}
	for _, member := range got.Members {
		backends[member.Backend] = true
	}
	for _, want := range []string{"kubevirt", "incus"} {
		if !backends[want] {
			t.Errorf("the fan-out missed the %s member: %+v", want, got.Members)
		}
	}
}

// The refusal path is still reachable: a pool can point at an instance whose
// backend is no longer configured — a peer that went away, or a context removed
// from the config. That member must report why rather than failing the whole
// fan-out or vanishing from the response.
func TestFolderAction_ReportsAnUnreachableBackend(t *testing.T) {
	ref := types.InstanceRef{Backend: "vmware", Name: "legacy"}
	live := map[string]types.VM{
		ref.String(): {Name: "legacy", Backend: "vmware", Running: true},
	}

	outcome := runFolderAction(ref, "pause", live)
	if outcome.OK {
		t.Fatal("an unregistered backend reported success")
	}
	if outcome.Error == "" {
		t.Fatal("the member failed with no reason attached")
	}
	if !strings.Contains(outcome.Error, "vmware") {
		t.Errorf("the reason does not name the backend: %q", outcome.Error)
	}
	if outcome.Name != "legacy" || outcome.Backend != "vmware" {
		t.Errorf("the outcome should identify the member: %+v", outcome)
	}
}

func TestFolderAction_RecursiveIncludesDescendants(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)

	postJSON(t, srv, "/api/folders/members",
		map[string]string{"path": "prod/web", "ref": demoRef(t, srv, "web-prod").String()}, nil)
	postJSON(t, srv, "/api/folders/members",
		map[string]string{"path": "prod/db", "ref": demoRef(t, srv, "db-prod").String()}, nil)

	var shallow actionResponse
	postJSON(t, srv, "/api/folders/action",
		map[string]any{"path": "prod", "action": "stop"}, &shallow)
	if shallow.OK+shallow.Failed+shallow.Skipped != 0 {
		// "prod" itself holds nothing; without recursive the fan-out must not
		// reach into its children.
		t.Errorf("a non-recursive action on an empty parent touched %+v", shallow.Members)
	}

	var deep actionResponse
	code := postJSON(t, srv, "/api/folders/action",
		map[string]any{"path": "prod", "action": "stop", "recursive": true}, &deep)
	if code != 200 {
		t.Fatalf("recursive action returned %d", code)
	}
	if len(deep.Members) != 2 {
		t.Errorf("recursive members = %+v, want both children's instances", deep.Members)
	}
}

func TestFolderAction_RefusesWhatIsNotAFolderAction(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)
	postJSON(t, srv, "/api/folders/members",
		map[string]string{"path": "prod", "ref": demoRef(t, srv, "web-prod").String()}, nil)

	// delete is deliberately absent: "delete all" behind one click over a group
	// whose membership was last checked a week ago is not a button to ship.
	for _, action := range []string{"delete", "migrate", "snapshot", ""} {
		var body map[string]any
		code := postJSON(t, srv, "/api/folders/action",
			map[string]any{"path": "prod", "action": action}, &body)
		if code != http.StatusBadRequest {
			t.Errorf("action %q returned %d, want 400", action, code)
		}
	}
}

func TestFolderAction_EmptyFolderIsRefused(t *testing.T) {
	srv := newDemoServer(t)
	scratchFolders(t)
	postJSON(t, srv, "/api/folders", map[string]string{"path": "empty"}, nil)

	var body map[string]any
	code := postJSON(t, srv, "/api/folders/action",
		map[string]any{"path": "empty", "action": "start"}, &body)
	if code != http.StatusBadRequest {
		t.Errorf("acting on an empty folder returned %d, want 400", code)
	}
}
