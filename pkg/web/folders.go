package web

// The folders API (ADR-0008).
//
// Two things here are worth reading rather than skimming.
//
// **Paths carry slashes.** The ADR sketched `/api/folders/{path}/{action}`, and
// that cannot work: `prod/web-stack` is one path segment's worth of meaning
// spread over two, so `{path}/{action}` is ambiguous the moment a folder nests.
// Paths therefore travel in the body or the query string, never in the route.
//
// **Bulk actions fan out server-side with per-member results.** Partial failure
// is the normal case in a heterogeneous folder — an action valid for a KubeVirt
// VM may be refused for an Incus container — so one response says what each
// member did. That is also why this waits for pkg/backend's contract rather than
// looping over the per-VM endpoints: the contract is what turns "this backend
// cannot do that" into a reportable outcome instead of silence.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/tuna-os/corral/pkg/backend"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/types"
)

// folderStore is the seam tests replace with an in-memory tree.
var folderStore = folder.NewStore(folder.ConfigBackend{})

// SetFolderStore overrides the folder store (for tests and for a surface that
// wants a scratch hierarchy).
func SetFolderStore(s *folder.Store) { folderStore = s }

func registerFolderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/folders", handleListFolders)
	mux.HandleFunc("POST /api/folders", handleCreateFolder)
	mux.HandleFunc("DELETE /api/folders", handleDeleteFolder)
	mux.HandleFunc("POST /api/folders/move", handleMoveFolder)
	mux.HandleFunc("POST /api/folders/members", handleAssignMember)
	mux.HandleFunc("DELETE /api/folders/members", handleUnassignMember)
	mux.HandleFunc("POST /api/folders/action", handleFolderAction)
}

// folderView is one folder as the UIs render it: the path, its members resolved
// against the live fleet, and the members that no longer resolve.
type folderView struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Members []types.VM `json:"members"`
	// Missing members are shown rather than hidden: a folder pointing at
	// something gone is information, and a partial fleet is normal here, so they
	// are never dropped on read.
	Missing []string `json:"missing,omitempty"`
}

// GET /api/folders
func handleListFolders(w http.ResponseWriter, r *http.Request) {
	tree, err := folderStore.Tree()
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}

	// The live fleet, so members can be returned as instances rather than as
	// selectors the client would have to resolve itself.
	result := fleet.List(r.Context())
	result.VMs = append(result.VMs, peerVMs()...)
	byRef := make(map[string]types.VM, len(result.VMs))
	for _, vm := range result.VMs {
		byRef[vm.Ref().String()] = vm
	}

	views := make([]folderView, 0, len(tree.Paths()))
	for _, path := range tree.Paths() {
		view := folderView{Path: path, Parent: folderParent(path), Members: []types.VM{}}
		for _, ref := range tree.Members(path, false) {
			if vm, ok := byRef[ref.String()]; ok {
				view.Members = append(view.Members, vm)
				continue
			}
			view.Missing = append(view.Missing, ref.String())
		}
		views = append(views, view)
	}

	// Unfoldered instances are what a "Folder View" tree puts at the root, so the
	// client does not have to diff the two lists itself.
	foldered := tree.PathsByRef()
	unfoldered := make([]types.VM, 0)
	for _, vm := range result.VMs {
		if _, ok := foldered[vm.Ref().String()]; !ok {
			unfoldered = append(unfoldered, vm)
		}
	}
	sort.Slice(unfoldered, func(i, j int) bool { return unfoldered[i].Name < unfoldered[j].Name })

	jsonResp(w, http.StatusOK, map[string]any{
		"folders":    views,
		"unfoldered": unfoldered,
		"errors":     result.Errors,
	})
}

// POST /api/folders  body: {path}
func handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	var created string
	err := folderStore.Update(func(tree *folder.Tree) error {
		path, err := tree.Ensure(body.Path)
		created = path
		return err
	})
	if err != nil {
		// A rejected path is the caller's mistake, not the server's: retrying
		// cannot help, so it is a 400.
		errResp(w, http.StatusBadRequest, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"path": created})
}

// DELETE /api/folders?path=prod/web
func handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if err := folderStore.Update(func(tree *folder.Tree) error { return tree.Remove(path) }); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	// Worth saying plainly in the response: a folder is a view, and removing it
	// never touches an instance.
	jsonResp(w, http.StatusOK, map[string]string{
		"status": "removed", "note": "members were unfoldered, not deleted"})
}

// POST /api/folders/move  body: {from, to}
func handleMoveFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if err := folderStore.Update(func(tree *folder.Tree) error {
		return tree.Move(body.From, body.To)
	}); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"from": body.From, "to": body.To})
}

// POST /api/folders/members  body: {path, ref}
//
// This is what a drag-and-drop lands on: one instance, one target folder. An
// instance belongs to at most one folder, so this moves it rather than adding a
// second membership.
func handleAssignMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	ref, err := types.ParseInstanceRef(body.Ref)
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if err := folderStore.Update(func(tree *folder.Tree) error {
		return tree.Assign(ref, body.Path)
	}); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"path": body.Path, "ref": ref.String()})
}

// DELETE /api/folders/members?ref=…
func handleUnassignMember(w http.ResponseWriter, r *http.Request) {
	ref, err := types.ParseInstanceRef(r.URL.Query().Get("ref"))
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if err := folderStore.Update(func(tree *folder.Tree) error {
		tree.Unassign(ref)
		return nil
	}); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "unfoldered", "ref": ref.String()})
}

// MemberOutcome is what one member did during a bulk action.
type MemberOutcome struct {
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	Backend string `json:"backend"`
	OK      bool   `json:"ok"`
	// Skipped marks a member the action does not apply to — a stopped VM asked to
	// stop, or a backend that cannot pause. Distinct from a failure, because
	// nothing went wrong and nothing needs retrying.
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// POST /api/folders/action  body: {path, action, recursive}
//
// The bulk fan-out. Actions are the power set only: delete is deliberately not a
// folder operation in this slice — "delete all" behind one click, over a group
// whose membership an operator may have last checked a week ago, is not a button
// worth shipping before per-member confirmation exists.
func handleFolderAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Action    string `json:"action"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	switch body.Action {
	case "start", "stop", "restart", "pause", "resume":
	default:
		errResp(w, http.StatusBadRequest,
			fmt.Errorf("%q is not a folder action; use start, stop, restart, pause, or resume",
				body.Action))
		return
	}

	tree, err := folderStore.Tree()
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	members := tree.Members(body.Path, body.Recursive)
	if len(members) == 0 {
		errResp(w, http.StatusBadRequest,
			fmt.Errorf("folder %q has no members", body.Path))
		return
	}

	// Live state, so an action that does not apply is reported as skipped rather
	// than attempted and failed.
	result := fleet.List(r.Context())
	result.VMs = append(result.VMs, peerVMs()...)
	live := make(map[string]types.VM, len(result.VMs))
	for _, vm := range result.VMs {
		live[vm.Ref().String()] = vm
	}

	outcomes := make([]MemberOutcome, 0, len(members))
	done := taskBegin("folder "+body.Action, body.Path)
	for _, ref := range members {
		outcomes = append(outcomes, runFolderAction(ref, body.Action, live))
	}
	done(nil)

	ok, failed, skipped := 0, 0, 0
	for _, outcome := range outcomes {
		switch {
		case outcome.Skipped:
			skipped++
		case outcome.OK:
			ok++
		default:
			failed++
		}
	}
	jsonResp(w, http.StatusOK, map[string]any{
		"path": body.Path, "action": body.Action,
		"ok": ok, "failed": failed, "skipped": skipped,
		"members": outcomes,
	})
}

// runFolderAction applies one action to one member through the operation
// contract. Every refusal it can give is a sentence an operator can act on.
func runFolderAction(ref types.InstanceRef, action string, live map[string]types.VM) MemberOutcome {
	outcome := MemberOutcome{Ref: ref.String(), Name: ref.Name, Backend: ref.Backend}

	vm, known := live[ref.String()]
	if !known {
		outcome.Skipped = true
		outcome.Error = "not in the current fleet (its context may be unreachable)"
		return outcome
	}

	// Power-state gating first: asking a stopped VM to stop is a no-op, and
	// reporting it as a failure would make a green fan-out look broken.
	switch action {
	case "start":
		if vm.Running {
			outcome.Skipped, outcome.Error = true, "already running"
			return outcome
		}
	case "stop", "restart", "pause":
		if !vm.Running {
			outcome.Skipped, outcome.Error = true, "not running"
			return outcome
		}
	}

	adapter, err := backend.For(ref)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}

	switch action {
	case "start", "stop":
		power, ok := adapter.(backend.Power)
		if !ok {
			outcome.Error = notSupported(adapter, action)
			return outcome
		}
		if action == "start" {
			err = power.Start(ref.Name)
		} else {
			err = power.Stop(ref.Name)
		}
	case "restart":
		restarter, ok := adapter.(backend.Restarter)
		if !ok {
			outcome.Error = notSupported(adapter, action)
			return outcome
		}
		err = restarter.Restart(ref.Name)
	case "pause", "resume":
		suspender, ok := adapter.(backend.Suspender)
		if !ok {
			outcome.Error = notSupported(adapter, action)
			return outcome
		}
		if action == "pause" {
			err = suspender.Pause(ref.Name)
		} else {
			err = suspender.Resume(ref.Name)
		}
	}

	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	outcome.OK = true
	return outcome
}

func notSupported(adapter backend.Adapter, action string) string {
	return fmt.Sprintf("the %s backend does not support %s yet", adapter.Backend(), action)
}

func folderParent(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}
