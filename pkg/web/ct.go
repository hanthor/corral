package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/kubevirt"
)

// GET /api/cts — list all Containers.
func handleListCTs(w http.ResponseWriter, r *http.Request) {
	cts, err := ct.ListCTs()
	if err != nil {
		errResp(w, http.StatusBadGateway, err)
		return
	}
	if cts == nil {
		cts = []ct.CT{}
	}
	jsonResp(w, http.StatusOK, cts)
}

type createCTRequest struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Image        string `json:"image"`
	CPU          int    `json:"cpu"`
	Mem          string `json:"mem"`
	Disk         string `json:"disk"`
	StorageClass string `json:"storageClass"`
	Privileged   bool   `json:"privileged"`
}

// POST /api/cts
func handleCreateCT(w http.ResponseWriter, r *http.Request) {
	var req createCTRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Image == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("name and image are required"))
		return
	}
	ns := req.Namespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}

	done := taskBegin("create ct", ns+"/"+req.Name)
	err := ct.Create(ct.CreateOpts{
		Name: req.Name, Namespace: ns, Image: req.Image,
		CPU: req.CPU, Mem: req.Mem, Disk: req.Disk,
		StorageClass: req.StorageClass, Privileged: req.Privileged,
	})
	if err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusCreated, map[string]string{"name": req.Name, "namespace": ns})
	// Tailnet-by-default, same as VM creation — best-effort and async so a
	// flaky proxy apply doesn't fail an otherwise-successful create.
	go func() {
		dp := taskBegin("tailnet expose", ns+"/"+req.Name)
		dp(ct.ApplyProxy(req.Name, ns, ct.ConsolePorts))
	}()
}

// POST /api/cts/{ns}/{name}/{action} — action is "start" or "stop".
func handleCTAction(w http.ResponseWriter, r *http.Request) {
	ns, name, action := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	var err error
	switch action {
	case "start":
		done := taskBegin("start ct", ns+"/"+name)
		err = ct.Start(name, ns)
		done(err)
	case "stop":
		done := taskBegin("stop ct", ns+"/"+name)
		err = ct.Stop(name, ns)
		done(err)
	default:
		errResp(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", action))
		return
	}
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DELETE /api/cts/{ns}/{name}
func handleDeleteCT(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	done := taskBegin("delete ct", ns+"/"+name)
	if err := ct.Delete(name, ns); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// PUT /api/cts/{ns}/{name}/scale
func handleScaleCT(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct {
		CPU int    `json:"cpu"`
		Mem string `json:"mem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if err := ct.Scale(name, ns, req.CPU, req.Mem); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "scaled"})
}
