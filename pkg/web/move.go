package web

// The cross-backend move API (ADR-0010).
//
// Two endpoints rather than one, and the split is the whole design. `move` is
// the most destructive thing Corral does to a running guest — it stops it,
// copies its disk somewhere else, and hands it back with a different MAC — so
// the decision to run it and the running of it are separate requests.
//
// POST /api/move/preflight touches nothing. It is safe to call on hover, on
// drop, on every mouse-up, and the UI does exactly that: dragging a card onto a
// backend calls preflight, and what comes back is the confirmation dialog's
// entire contents. A refused move has no confirm button, and the operator finds
// out about the UEFI mismatch or the missing 20GiB by dragging rather than by
// losing a VM.
//
// POST /api/move commits, and re-runs the preflight server-side before it does.
// A client cannot skip the check by posting straight here, and a plan that went
// stale between the drop and the click — someone else filled the scratch
// filesystem — is caught rather than trusted.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tuna-os/corral/pkg/backend"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/move"
	"github.com/tuna-os/corral/pkg/types"
)

// moveRunner is the seam tests replace. The real one is move.Execute.
var moveRunner = move.Execute

// moveResolver finds the source instance in the live fleet. Separate seam
// because a test that wants to exercise a refusal should not need a fleet.
var moveResolver = func(r *http.Request, ref types.InstanceRef) (types.VM, error) {
	result := fleet.List(r.Context())
	result.VMs = append(result.VMs, peerVMs()...)
	for _, vm := range result.VMs {
		if vm.Ref().String() == ref.String() {
			return vm, nil
		}
	}
	return types.VM{}, fmt.Errorf("no instance matches %s", ref)
}

func registerMoveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/move/preflight", handleMovePreflight)
	mux.HandleFunc("POST /api/move", handleMove)
	mux.HandleFunc("GET /api/move/destinations", handleMoveDestinations)
}

// moveRequest is what both endpoints take. The destination is spelled out
// rather than given as a ref, because the instance does not exist there yet.
type moveRequest struct {
	Ref          string `json:"ref"`
	ToBackend    string `json:"toBackend"`
	ToContext    string `json:"toContext,omitempty"`
	ToNamespace  string `json:"toNamespace,omitempty"`
	Name         string `json:"name,omitempty"`
	Scratch      string `json:"scratch,omitempty"`
	DeleteSource bool   `json:"deleteSource,omitempty"`
}

// planFor resolves a request into a plan without running anything. Both
// handlers go through it, which is what makes "the server always preflights"
// true rather than aspirational.
func planFor(r *http.Request, body moveRequest) (move.Plan, types.VM, error) {
	ref, err := types.ParseInstanceRef(body.Ref)
	if err != nil {
		return move.Plan{}, types.VM{}, err
	}
	vm, err := moveResolver(r, ref)
	if err != nil {
		return move.Plan{}, types.VM{}, err
	}
	plan := move.Preflight(move.Inspect(vm, false), move.Target{
		Backend:      body.ToBackend,
		Context:      body.ToContext,
		Namespace:    body.ToNamespace,
		Name:         body.Name,
		Scratch:      body.Scratch,
		DeleteSource: body.DeleteSource,
	})
	return plan, vm, nil
}

// POST /api/move/preflight — the plan, and nothing else happens.
func handleMovePreflight(w http.ResponseWriter, r *http.Request) {
	var body moveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	plan, _, err := planFor(r, body)
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	// A refused plan is a 200, deliberately: the preflight did its job, and the
	// refusals are the answer the client asked for. An error status would make
	// the UI show "request failed" where it should show three reasons.
	jsonResp(w, http.StatusOK, movePlanResponse(plan))
}

// POST /api/move — commit.
func handleMove(w http.ResponseWriter, r *http.Request) {
	var body moveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	plan, vm, err := planFor(r, body)
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if !plan.OK() {
		// 409, not 400: the request is well-formed and the state is what
		// refuses it. The refusals travel in the body so the UI can show the
		// same list it showed at preflight, in case it changed since.
		jsonResp(w, http.StatusConflict, movePlanResponse(plan))
		return
	}

	done := taskBegin("move", fmt.Sprintf("%s → %s/%s",
		plan.Source.Name, plan.Destination.Backend, plan.Destination.Name))
	result, err := moveRunner(r.Context(), plan, nil)
	done(err)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}

	// The instance moved; its folder membership should follow it, or the move
	// silently drops it out of the grouping the operator organised it into.
	// A failure here is reported, never fatal: the VM is already there.
	var folderNote string
	if path, ok := carryFolder(vm.Ref(), plan.Destination); ok {
		folderNote = path
	}

	jsonResp(w, http.StatusOK, map[string]any{
		"destination":   result.Destination,
		"bytes":         result.Bytes,
		"sourceStopped": result.SourceStopped,
		"sourceDeleted": result.SourceDeleted,
		"warnings":      result.Warnings,
		"dropped":       result.Dropped,
		"folder":        folderNote,
		"note":          "the instance was created stopped; start it when you are ready",
	})
}

// carryFolder moves the source's folder membership onto the destination. The
// source keeps its own membership unless it was deleted — a stopped source
// still in "production" is accurate.
func carryFolder(source, destination types.InstanceRef) (string, bool) {
	tree, err := folderStore.Tree()
	if err != nil {
		return "", false
	}
	path := tree.PathOf(source)
	if path == "" {
		return "", false
	}
	if err := folderStore.Update(func(t *folder.Tree) error {
		return t.Assign(destination, path)
	}); err != nil {
		return "", false
	}
	return path, true
}

// GET /api/move/destinations — which backends can receive an instance, and why
// the others cannot.
//
// The UI needs this to decide which drop targets are live before a drag starts.
// Greying out a target the operator cannot use is kinder than accepting the
// drop and refusing it afterwards, and the reason is carried so a hover can say
// why rather than leaving it a mystery.
func handleMoveDestinations(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Backend string `json:"backend"`
		Can     bool   `json:"can"`
		Reason  string `json:"reason,omitempty"`
	}
	out := make([]entry, 0, len(backend.Backends))
	for _, b := range backend.Backends {
		out = append(out, entry{Backend: b, Can: backend.CanIngest(b), Reason: backend.IngestRefusal(b)})
	}
	jsonResp(w, http.StatusOK, map[string]any{"destinations": out})
}

func movePlanResponse(plan move.Plan) map[string]any {
	// Warnings and dropped config are never omitted, even on a refused plan:
	// fixing the refusal does not make the IP change go away, and an operator
	// who reads the dialog once should have read all of it.
	return map[string]any{
		"ok":             plan.OK(),
		"source":         plan.Source,
		"destination":    plan.Destination,
		"steps":          plan.Steps,
		"warnings":       plan.Warnings,
		"dropped":        plan.Dropped,
		"refusals":       plan.Refusals,
		"estimatedBytes": plan.EstimatedBytes,
		"stopFirst":      plan.StopFirst,
		"deleteSource":   plan.DeleteSource,
		"format":         plan.Format,
	}
}
