package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/sources"
	"github.com/tuna-os/corral/pkg/types"
)

// ── Doctor (cluster diagnostics) ──────────────────────────────────

// GET /api/doctor — run the diagnostics.
func handleDoctor(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, doctor.Run())
}

// POST /api/doctor/fix — reconcile the fixable issues.
// Body {"check": "<name>"} fixes just that check; empty body fixes all.
func handleDoctorFix(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Check string `json:"check"`
	}
	json.NewDecoder(r.Body).Decode(&b) // empty body = fix all
	if b.Check != "" {
		done := taskBegin("doctor fix", b.Check)
		if err := doctor.FixOne(b.Check); err != nil {
			done(err)
			errResp(w, http.StatusInternalServerError, err)
			return
		}
		done(nil)
		jsonResp(w, http.StatusOK, map[string]any{"fixed": []string{b.Check}})
		return
	}
	done := taskBegin("doctor fix", "all fixable")
	fixed, err := doctor.Fix()
	done(err)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"fixed": fixed})
}

// ── Extensions (plugins) store ────────────────────────────────────

type pluginItem struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Version           string   `json:"version"`
	Homepage          string   `json:"homepage,omitempty"`
	Installed         bool     `json:"installed"`
	InStore           bool     `json:"inStore"`
	Source            string   `json:"source,omitempty"`
	Publisher         string   `json:"publisher,omitempty"`
	License           string   `json:"license,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	SupportedBackends []string `json:"supportedBackends,omitempty"`
	Pinned            bool     `json:"pinned,omitempty"`
}

// GET /api/plugins — marketplace entries merged with installed state.
func handlePlugins(w http.ResponseWriter, r *http.Request) {
	installed := map[string]bool{}
	for _, p := range plugin.Installed() {
		installed[p.Name] = true
	}
	items := []pluginItem{}
	seen := map[string]bool{}
	if idx, _ := plugin.FetchAll(); idx != nil {
		for _, e := range idx.Plugins {
			state, _ := plugin.ReadState(e.Name)
			items = append(items, pluginItem{Name: e.Name, Description: e.Description, Version: e.Version, Homepage: e.Homepage, Installed: installed[e.Name], InStore: true, Source: e.Source, Publisher: e.Publisher.Name, License: e.License, Capabilities: e.Capabilities, Permissions: e.Permissions, SupportedBackends: e.SupportedBackends, Pinned: state.Pinned})
			seen[e.Name] = true
		}
	}
	for _, p := range plugin.Installed() {
		if !seen[p.Name] {
			items = append(items, pluginItem{Name: p.Name, Installed: true})
		}
	}
	jsonResp(w, http.StatusOK, items)
}

// POST /api/plugins/{name}/install
func handleInstallPlugin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	idx, errs := plugin.FetchAll()
	if len(idx.Plugins) == 0 && len(errs) > 0 {
		errResp(w, http.StatusBadGateway, errs[0])
		return
	}
	lookup := name
	if source := r.URL.Query().Get("source"); source != "" {
		lookup = source + "/" + name
	}
	e := idx.Find(lookup)
	if e == nil {
		errResp(w, http.StatusNotFound, fmt.Errorf("no plugin %q in the marketplace", name))
		return
	}
	var consent struct {
		AcceptPermissions bool `json:"acceptPermissions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&consent)
	if len(e.Permissions) > 0 && !consent.AcceptPermissions {
		errResp(w, http.StatusConflict, fmt.Errorf("plugin requests permissions %v; explicit acceptPermissions is required", e.Permissions))
		return
	}
	done := taskBegin("install plugin", name)
	if err := e.Install(); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"installed": name})
}

// DELETE /api/plugins/{name}
func handleRemovePlugin(w http.ResponseWriter, r *http.Request) {
	if err := plugin.Remove(r.PathValue("name")); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "removed"})
}

// This file holds the HTTP handlers for the advanced VM operations
// (scale, volumes, expand, snapshots, clone, guest info, capabilities).
// Registered in server.go alongside the basic lifecycle routes.

// handleCapabilities reports cluster storage capabilities so the UI can
// enable/disable expand and snapshot controls.
func handleCapabilities(w http.ResponseWriter, r *http.Request) {
	c := kubevirt.ClusterCapabilities()
	// Installed plugins, so the UI can light up their features
	// (the bootc flag stays separate: it's compile-time, not a binary on disk).
	plugins := []string{}
	for _, p := range plugin.Installed() {
		plugins = append(plugins, p.Name)
	}
	jsonResp(w, http.StatusOK, map[string]any{
		"storageClass":     c.StorageClass,
		"canExpand":        c.CanExpand,
		"canSnapshot":      c.CanSnapshot,
		"bootc":            kubevirt.BootcAvailable(), // optional plugin
		"local":            qemu.Available(),          // this host can run QEMU VMs (#91)
		"plugins":          plugins,
		"defaultNamespace": kubevirt.DefaultNamespace,
	})
}

// POST /api/vms/{ns}/{name}/scale  body: {cpu, mem}
func handleScale(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		CPU int    `json:"cpu"`
		Mem string `json:"mem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	done := taskBegin("scale", ns+"/"+name)
	if err := kubeClient(r, ns).Scale(name, b.CPU, b.Mem); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/vms/{ns}/{name}/options  body: {runStrategy?, firmware?, machineType?, bootOrder?}
// Fields left unset (absent from the request body) are left unchanged — see
// kubevirt.VMOptions for which fields apply live vs. need a restart.
func handleSetOptions(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		RunStrategy *string        `json:"runStrategy"`
		Firmware    *string        `json:"firmware"`
		MachineType *string        `json:"machineType"`
		BootOrder   map[string]int `json:"bootOrder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	done := taskBegin("set options", ns+"/"+name)
	opts := kubevirt.VMOptions{
		RunStrategy: b.RunStrategy,
		Firmware:    b.Firmware,
		MachineType: b.MachineType,
		BootOrder:   b.BootOrder,
	}
	if err := kubeClient(r, ns).SetVMOptions(name, opts); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/vms/{ns}/{name}/volumes  body: {size}
func handleAddVolume(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		Size string `json:"size"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	done := taskBegin("add disk", ns+"/"+name)
	pvc, err := kubeClient(r, ns).AddVolume(name, b.Size)
	done(err)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"pvc": pvc})
}

// DELETE /api/vms/{ns}/{name}/volumes/{vol}
func handleRemoveVolume(w http.ResponseWriter, r *http.Request) {
	ns, name, vol := r.PathValue("ns"), r.PathValue("name"), r.PathValue("vol")
	done := taskBegin("detach disk", ns+"/"+name)
	if err := kubeClient(r, ns).RemoveVolume(name, vol); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "removed"})
}

// POST /api/vms/{ns}/{name}/expand  body: {pvc, size}
func handleExpand(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	var b struct {
		PVC  string `json:"pvc"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.PVC == "" || b.Size == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("pvc and size are required"))
		return
	}
	done := taskBegin("expand disk", ns+"/"+b.PVC)
	if err := kubeClient(r, ns).ExpandDisk(b.PVC, b.Size); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// snapshotRef builds the canonical instance reference the snapshot adapters
// dispatch on. The reserved namespaces name their backend; anything else is a
// KubeVirt namespace, and ?context= selects among several clusters exactly as
// it does for every other VM subresource.
func snapshotRef(r *http.Request) types.InstanceRef {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	context := r.URL.Query().Get("context")
	switch ns {
	case localNS:
		return types.InstanceRef{Backend: "qemu", Name: name}
	case "incus":
		return types.InstanceRef{Backend: "incus", Context: context, Name: name}
	case "libvirt":
		return types.InstanceRef{Backend: "libvirt", Context: context, Name: name}
	default:
		return types.InstanceRef{Backend: "kubevirt", Context: context, Namespace: ns, Name: name}
	}
}

// snapshotError maps an adapter failure onto a status. A backend that cannot
// do this is a client-side 400 — retrying won't help and the message says why
// — where a backend that tried and failed is a 502.
func snapshotError(w http.ResponseWriter, err error) {
	var unsupported *snapshot.Unsupported
	if errors.As(err, &unsupported) {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	errResp(w, http.StatusBadGateway, err)
}

// GET /api/vms/{ns}/{name}/snapshots
func handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	ref := snapshotRef(r)
	adapter, err := snapshot.For(ref)
	if err != nil {
		snapshotError(w, err)
		return
	}
	snaps, err := adapter.List(ref)
	if err != nil {
		snapshotError(w, err)
		return
	}
	if snaps == nil {
		snaps = []snapshot.Snapshot{}
	}
	jsonResp(w, http.StatusOK, snaps)
}

// POST /api/vms/{ns}/{name}/snapshots  body: {name?}
func handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	ref := snapshotRef(r)
	var b struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	adapter, err := snapshot.For(ref)
	if err != nil {
		snapshotError(w, err)
		return
	}
	done := taskBegin("snapshot", ref.Namespace+"/"+ref.Name)
	snap, err := adapter.Create(ref, b.Name)
	done(err)
	if err != nil {
		snapshotError(w, err)
		return
	}
	// consistency travels with the response: the UI has to be able to tell a
	// frozen-filesystem capture from a crash-consistent one.
	jsonResp(w, http.StatusOK, snap)
}

// POST /api/vms/{ns}/{name}/snapshots/{snap}/restore
func handleRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	ref := snapshotRef(r)
	adapter, err := snapshot.For(ref)
	if err != nil {
		snapshotError(w, err)
		return
	}
	done := taskBegin("restore snapshot", ref.Namespace+"/"+ref.Name)
	if err := adapter.Restore(ref, r.PathValue("snap")); err != nil {
		done(err)
		snapshotError(w, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "restoring"})
}

// DELETE /api/vms/{ns}/{name}/snapshots/{snap}
func handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	ref, snap := snapshotRef(r), r.PathValue("snap")
	adapter, err := snapshot.For(ref)
	if err != nil {
		snapshotError(w, err)
		return
	}
	done := taskBegin("delete snapshot", ref.Namespace+"/"+snap)
	if err := adapter.Delete(ref, snap); err != nil {
		done(err)
		snapshotError(w, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/vms/{ns}/{name}/clone  body: {target}
func handleClone(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Target == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("target name is required"))
		return
	}
	done := taskBegin("clone", ns+"/"+name+" → "+b.Target)
	if err := kubeClient(r, ns).Clone(name, b.Target); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	if store != nil {
		store.Set(b.Target, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
	}
	jsonResp(w, http.StatusOK, map[string]string{"target": b.Target})
}

// GET /api/vms/{ns}/{name}/guestinfo
func handleGuestInfo(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	info, err := kubeClient(r, ns).GuestInfo(name)
	if err != nil {
		errResp(w, http.StatusServiceUnavailable, err)
		return
	}
	jsonResp(w, http.StatusOK, info)
}

// GET /api/vms/{ns}/{name}/events
func handleEvents(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	evs, err := kubeClient(r, ns).Events(name)
	if err != nil {
		errResp(w, http.StatusBadGateway, err)
		return
	}
	jsonResp(w, http.StatusOK, evs)
}

// GET /api/vms/{ns}/{name}/metrics
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	jsonResp(w, http.StatusOK, kubeClient(r, ns).Metrics(name))
}

// GET /api/vms/{ns}/{name}/metrics/history — retained CPU samples (millicores)
// for the summary sparkline. Empty array when metrics-server is absent.
func handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	samples := cpuHist.get(ns + "/" + name)
	if samples == nil {
		samples = []cpuSample{}
	}
	jsonResp(w, http.StatusOK, samples)
}

// POST /api/vms/{ns}/{name}/template  body: {on: bool}  — mark/unmark template
func handleMarkTemplate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		On bool `json:"on"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	if err := kubeClient(r, ns).MarkTemplate(name, b.On); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]bool{"isTemplate": b.On})
}

// POST /api/vms/{ns}/{name}/tags  body: {tag, on}  — add/remove a tag label
func handleSetTag(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		Tag string `json:"tag"`
		On  bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || strings.TrimSpace(b.Tag) == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("tag is required"))
		return
	}
	if err := kubeClient(r, ns).SetTag(name, b.Tag, b.On); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"tag": b.Tag, "on": b.On})
}

// GET /api/nads — Multus NetworkAttachmentDefinitions for secondary NICs
func handleNADs(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, kubevirt.ListNADs())
}

// POST /api/vms/{ns}/{name}/nics  body: {nad, iface}
func handleAddNIC(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		NAD   string `json:"nad"`
		Iface string `json:"iface"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.NAD == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("nad is required"))
		return
	}
	if err := kubeClient(r, ns).AddNIC(name, b.NAD, b.Iface); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/vms/{ns}/{name}/bootc/rebuild  body: {image?}
// Rebuilds a bootc VM's disk (from its recorded image, or the given override
// to switch images) and restarts it. Runs as a background task with a live
// build log, like the initial bootc create.
func handleBootcRebuild(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !kubevirt.BootcAvailable() {
		errResp(w, http.StatusBadRequest,
			fmt.Errorf("bootc support is not enabled on this server (optional plugin)"))
		return
	}
	var b struct {
		Image string `json:"image"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	image := catalog.ResolveBootc(b.Image)
	if image == "" {
		// The durable corral.bootc/image annotation on the VM, not the local
		// registry (which is lost on server-pod restart and not the source
		// of truth for this — see kubevirt.BootcImageOf).
		image = kubevirt.BootcImageOf(name, ns)
	}
	if image == "" {
		errResp(w, http.StatusBadRequest,
			fmt.Errorf("no recorded bootc image for %q — pass an image to switch to", name))
		return
	}
	sshKey := kubevirt.LoadSSHPublicKey()

	id := fmt.Sprintf("bootc-rebuild-%s-%d", name, time.Now().UnixNano())
	task := newBuildTask()
	tasks.Store(id, task)
	done := taskBegin("bootc rebuild", ns+"/"+name)
	go func() {
		err := kubevirt.BootcRebuild(name, ns, image, sshKey, "", task)
		if err == nil && store != nil {
			store.Set(name, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
		}
		task.finish(err)
		done(err)
	}()
	jsonResp(w, http.StatusAccepted, map[string]string{"task": id})
}

// GET /api/images — the built-in OS image catalog, with any user-defined
// custom sources appended (flagged custom:true). ?type=bootc returns the
// curated bootc image catalog instead.
func handleImages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("type") == "bootc" {
		jsonResp(w, http.StatusOK, catalog.BootcImages)
		return
	}
	list := make([]sources.Source, 0, len(catalog.Images))
	for _, img := range catalog.Images {
		list = append(list, sources.Source{Image: img})
	}
	if custom, err := sources.Load(kubevirt.DefaultNamespace); err == nil {
		list = append(list, custom...)
	}
	jsonResp(w, http.StatusOK, list)
}

// GET /api/sources — just the user-defined custom sources (for management UI).
func handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := sources.Load(kubevirt.DefaultNamespace)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	if srcs == nil {
		srcs = []sources.Source{}
	}
	jsonResp(w, http.StatusOK, srcs)
}

// POST /api/sources — add/replace a custom source.
// Body: {name, description, kind: "containerDisk"|"url"|"iso", uri, defaultUser?}
func handleAddSource(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Kind        string `json:"kind"`
		URI         string `json:"uri"`
		DefaultUser string `json:"defaultUser"`
		Variant     string `json:"variant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	s := sources.Source{Image: catalog.Image{
		Name: b.Name, Description: b.Description, DefaultUser: b.DefaultUser, Variant: b.Variant,
	}}
	switch b.Kind {
	case "containerDisk":
		s.ContainerDisk = b.URI
	case "url":
		s.URL = b.URI
	case "iso":
		s.ISO = b.URI
	default:
		errResp(w, http.StatusBadRequest, fmt.Errorf("kind must be containerDisk, url, or iso"))
		return
	}
	if err := sources.Add(kubevirt.DefaultNamespace, s); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "name": s.Name})
}

// DELETE /api/sources/{name} — remove a custom source.
func handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if err := sources.Remove(kubevirt.DefaultNamespace, r.PathValue("name")); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "removed"})
}

// GET /api/instancetypes — cluster instancetypes + preferences for the create wizard
func handleInstanceTypes(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, map[string][]string{
		"instancetypes": kubevirt.ListInstanceTypes(),
		"preferences":   kubevirt.ListPreferences(),
	})
}

// GET /api/datavolumes — image/ISO library
func handleListDataVolumes(w http.ResponseWriter, r *http.Request) {
	dvs, err := kubevirt.ListDataVolumes()
	if err != nil {
		errResp(w, http.StatusBadGateway, err)
		return
	}
	jsonResp(w, http.StatusOK, dvs)
}

// POST /api/datavolumes  body: {name, namespace, url, size}
func handleImportDataVolume(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name, Namespace, URL, Size string
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Name == "" || b.URL == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("name and url are required"))
		return
	}
	done := taskBegin("import image", b.Namespace+"/"+b.Name)
	if err := kubevirt.ImportDataVolume(b.Name, b.Namespace, b.URL, b.Size); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"name": b.Name})
}

// POST /api/datavolumes/upload?name=X&namespace=Y&size=Z&storageClass=W
// Request body is the raw image bytes (fetch(url, {body: file}) from the
// browser — no multipart parsing, the whole body is the file). Streams to a
// temp file on this server's disk (io.Copy, not buffered in memory — images
// are routinely multi-GB), then hands that path to virtctl image-upload as
// a background task so the browser gets an immediate response and progress
// shows in the task panel like a bootc build.
func handleUploadDataVolume(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	ns := r.URL.Query().Get("namespace")
	size := r.URL.Query().Get("size")
	sc := r.URL.Query().Get("storageClass")
	if name == "" || size == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("name and size are required"))
		return
	}
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}

	tmp, err := os.CreateTemp("", "corral-upload-*.img")
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	tmpPath := tmp.Name()
	written, err := io.Copy(tmp, r.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmpPath)
		errResp(w, http.StatusBadRequest, fmt.Errorf("receiving upload: %w", err))
		return
	}
	if written == 0 {
		os.Remove(tmpPath)
		errResp(w, http.StatusBadRequest, fmt.Errorf("empty upload"))
		return
	}

	id := fmt.Sprintf("upload-%s-%d", name, time.Now().UnixNano())
	task := newBuildTask()
	tasks.Store(id, task)
	done := taskBegin("upload image", ns+"/"+name)

	go func() {
		defer os.Remove(tmpPath)
		err := kubevirt.UploadDataVolume(name, ns, tmpPath, size, sc, task)
		task.finish(err)
		done(err)
	}()
	jsonResp(w, http.StatusAccepted, map[string]string{"task": id})
}

// DELETE /api/datavolumes/{ns}/{name}
func handleDeleteDataVolume(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	done := taskBegin("delete image", ns+"/"+name)
	if err := kubevirt.DeleteDataVolume(ns, name); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	jsonResp(w, http.StatusOK, map[string]string{"status": "deleted"})
}
