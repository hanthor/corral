// Package web serves the Corral web UI: a Proxmox-style dashboard over both
// backends — KubeVirt VMs/CTs and this host's local QEMU VMs (#91) — with
// in-browser VNC (noVNC) and serial TTY (xterm.js) consoles. It shares the
// registry and cluster state with the CLI/TUI, so both can be used in tandem.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/tuna-os/corral/pkg/qemu"
	"golang.org/x/net/websocket"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/proxmox"
	"github.com/tuna-os/corral/pkg/registry"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// defaultRunner is the command runner used by handlers that shell out
// (vmiIndex, handleNodes, handleExport). Defaults to shell.Real; set in tests.
var defaultRunner shell.Runner = shell.DefaultKubectl

//go:embed static
var staticFS embed.FS

var store *registry.Store

// Serve starts the web UI on addr and blocks.
func Serve(addr string) error {
	if s, err := registry.NewStore(); err == nil {
		store = s
	}
	kubevirt.ProxyTags = config.TailnetTags()

	// Assemble theme: config.yaml → CLI flags override → serve.
	if cfg, err := config.Load(""); err == nil {
		loadThemeFromConfig(cfg)
	}
	applyCLITheme() // CLI flags always win over config file

	mux, err := newMux()
	if err != nil {
		return err
	}
	startMetricSampler()
	fmt.Fprintf(os.Stderr, "Corral web UI listening on http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

// newMux builds the HTTP router (wrapped in the admin gate). Split out from
// Serve so tests can exercise the full route table with httptest.
func newMux() (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	// Static files (css, js, vendor/) served from the embedded FS.
	// Index.html is intercepted and theme-injected by the serveRoot wrapper.
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndex(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	// Proxmox API compatibility — expose /api2/json/… alongside the
	// corral REST API so ecosystem tools can manage VMs through the
	// same corral web deployment.
	mux.Handle("/api2/json/", proxmox.NewHandler(kubevirt.DefaultNamespace))

	mux.HandleFunc("GET /api/whoami", handleWhoami)
	mux.HandleFunc("GET /api/theme", handleGetTheme)
	mux.HandleFunc("PUT /api/theme", handlePutTheme)
	mux.HandleFunc("GET /api/vms", handleListVMs)
	mux.HandleFunc("GET /api/v1/inventory", handleInventory)
	mux.HandleFunc("GET /api/v1/meta", handleAPIMeta)
	mux.HandleFunc("GET /api/contexts", handleContexts)
	mux.HandleFunc("POST /api/vms", handleCreateVM)
	mux.HandleFunc("GET /api/cts", handleListCTs)
	mux.HandleFunc("POST /api/cts", handleCreateCT)
	mux.HandleFunc("POST /api/cts/{ns}/{name}/{action}", handleCTAction)
	mux.HandleFunc("DELETE /api/cts/{ns}/{name}", handleDeleteCT)
	mux.HandleFunc("PUT /api/cts/{ns}/{name}/scale", handleScaleCT)
	mux.HandleFunc("GET /api/nodes", handleNodes)
	mux.HandleFunc("GET /api/capabilities", handleCapabilities)
	mux.HandleFunc("GET /api/images", handleImages)
	mux.HandleFunc("GET /api/sources", handleListSources)
	mux.HandleFunc("POST /api/sources", handleAddSource)
	mux.HandleFunc("DELETE /api/sources/{name}", handleDeleteSource)
	mux.HandleFunc("GET /api/instancetypes", handleInstanceTypes)
	mux.HandleFunc("GET /api/nads", handleNADs)
	mux.HandleFunc("GET /api/gpus", handleListGPUs)
	mux.HandleFunc("GET /api/vms/{ns}/{name}/gpus", handleGetVMGPUs)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/gpus", handleAttachGPU)
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}/gpus/{gpu}", handleDetachGPU)
	mux.HandleFunc("GET /api/doctor", handleDoctor)
	mux.HandleFunc("POST /api/doctor/fix", handleDoctorFix)
	mux.HandleFunc("GET /api/plugins", handlePlugins)
	mux.HandleFunc("POST /api/plugins/{name}/install", handleInstallPlugin)
	mux.HandleFunc("DELETE /api/plugins/{name}", handleRemovePlugin)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/nics", handleAddNIC)
	mux.HandleFunc("GET /api/datavolumes", handleListDataVolumes)
	mux.HandleFunc("POST /api/datavolumes", handleImportDataVolume)
	mux.HandleFunc("POST /api/datavolumes/upload", handleUploadDataVolume)
	mux.HandleFunc("DELETE /api/datavolumes/{ns}/{name}", handleDeleteDataVolume)
	mux.HandleFunc("GET /api/tasks/{id}", handleTaskStatus)
	mux.HandleFunc("GET /api/tasklog", handleTaskLog)
	registerFolderRoutes(mux)
	registerMoveRoutes(mux)
	mux.HandleFunc("GET /api/vms/{ns}/{name}", handleVMInfo)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/{action}", handleVMAction)
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}", handleDeleteVM)

	// Cluster-only operations — no meaning for local QEMU VMs (#91), so the
	// reserved "local" namespace gets a uniform 400 instead of a confusing
	// kubectl error. More specific patterns win over {action} above.
	noLocal := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("ns") == localNS {
				errResp(w, http.StatusBadRequest,
					fmt.Errorf("this operation is not supported for local QEMU VMs"))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/vms/{ns}/{name}/migrate", noLocal(handleMigrate))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/scale", noLocal(handleScale))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/expand", noLocal(handleExpand))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/clone", noLocal(handleClone))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/template", noLocal(handleMarkTemplate))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/tags", noLocal(handleSetTag))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/options", noLocal(handleSetOptions))
	mux.HandleFunc("GET /api/vms/{ns}/{name}/guestinfo", handleGuestInfo)
	mux.HandleFunc("GET /api/vms/{ns}/{name}/rdp", handleRDPCheck)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/bootc/rebuild", noLocal(handleBootcRebuild))
	mux.HandleFunc("GET /api/vms/{ns}/{name}/events", handleEvents)
	mux.HandleFunc("GET /api/vms/{ns}/{name}/metrics", handleMetrics)
	mux.HandleFunc("GET /api/vms/{ns}/{name}/metrics/history", handleMetricsHistory)
	mux.HandleFunc("GET /api/vms/{ns}/{name}/export", noLocal(handleExport))
	mux.HandleFunc("POST /api/vms/{ns}/{name}/volumes", noLocal(handleAddVolume))
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}/volumes/{vol}", noLocal(handleRemoveVolume))
	mux.HandleFunc("GET /api/vms/{ns}/{name}/snapschedule", handleGetSnapSchedule)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/snapschedule", noLocal(handleSetSnapSchedule))
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}/snapschedule", noLocal(handleDeleteSnapSchedule))
	mux.HandleFunc("GET /api/vms/{ns}/{name}/powerschedule", handleGetPowerSchedule)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/powerschedule", noLocal(handleSetPowerSchedule))
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}/powerschedule", noLocal(handleDeletePowerSchedule))
	mux.HandleFunc("GET /api/vms/{ns}/{name}/snapshots", handleListSnapshots)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/snapshots", handleCreateSnapshot)
	mux.HandleFunc("DELETE /api/vms/{ns}/{name}/snapshots/{snap}", handleDeleteSnapshot)
	mux.HandleFunc("POST /api/vms/{ns}/{name}/snapshots/{snap}/restore", handleRestoreSnapshot)

	wsServer := func(h websocket.Handler) http.Handler {
		return websocket.Server{
			Handler: h,
			Handshake: func(cfg *websocket.Config, r *http.Request) error {
				if !authenticationRequired() {
					return nil
				}
				if r.Header.Get("Corral-Service-Identity") != "" {
					return nil
				}
				origin := r.Header.Get("Origin")
				if origin == "" {
					return fmt.Errorf("websocket origin is required")
				}
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					return fmt.Errorf("websocket origin %q does not match host", origin)
				}
				return nil
			},
		}
	}
	mux.Handle("GET /api/vnc/{ns}/{name}", wsServer(vncBridge))
	mux.Handle("GET /api/tty/{ns}/{name}", wsServer(ttyBridge))
	mux.Handle("GET /api/rdp/{ns}/{name}", wsServer(rdpBridge))

	// The admin gate lets safe (GET) requests through and rejects mutating
	// requests from non-admins when CORRAL_ADMINS is set.
	return peerServiceAuth(adminGate(peerGate(mux))), nil
}

// serveIndex reads the embedded index.html, injects the active theme, and
// serves it. This is the only dynamic page — everything else is static.
func serveIndex(w http.ResponseWriter, r *http.Request) {
	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(injectTheme(string(html))))
}

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, code int, err error) {
	jsonResp(w, code, map[string]string{"error": err.Error()})
}

// httpError lets a function that doesn't touch http.ResponseWriter (e.g. the
// createBootc/createWindows/createGeneric strategy functions) still signal
// which status a validation failure should map to. Plain errors default to
// 500 — see statusFor.
type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

func badRequest(err error) error { return &httpError{status: http.StatusBadRequest, err: err} }

// statusFor returns the HTTP status a strategy-function error maps to.
func statusFor(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return http.StatusInternalServerError
}

// ── VM list ───────────────────────────────────────────────────────

func handleListVMs(w http.ResponseWriter, r *http.Request) {
	result := fleet.List(r.Context())
	vms := append(result.VMs, peerVMs()...)
	// Nothing to show and a remote backend that can't be reached is the "No
	// cluster connected" case the dashboard renders setup guidance for, so it
	// has to surface as an error rather than an empty fleet. A qemu-only
	// deployment has no remote contexts at all (see config.Contexts) and so can
	// never trip this; a failing local qemu listing alone never means "no
	// cluster", which is why only remote errors count.
	if len(vms) == 0 && len(config.Peers()) == 0 {
		if bad := remoteErrors(result.Errors); bad != "" {
			errResp(w, http.StatusBadGateway, fmt.Errorf("listing VMs: %s", bad))
			return
		}
	}
	if vms == nil {
		vms = []types.VM{}
	}
	if len(result.Errors) > 0 {
		w.Header().Set("X-Corral-Partial", "true")
	}
	jsonResp(w, http.StatusOK, vms)
}

// remoteErrors renders the listing errors that came from a remote (non-local)
// context as a stable, sorted "name: message" list, or "" when every failure
// was local. Sorted so the message doesn't churn across map iterations.
func remoteErrors(errs map[string]string) string {
	if len(errs) == 0 {
		return ""
	}
	var parts []string
	for _, c := range config.Contexts() {
		if c.Backend == "qemu" {
			continue
		}
		if msg, ok := errs[c.Name]; ok {
			parts = append(parts, c.Name+": "+msg)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func handleInventory(w http.ResponseWriter, r *http.Request) {
	result := fleet.List(r.Context())
	result.VMs = append(result.VMs, peerVMs()...)
	jsonResp(w, http.StatusOK, result)
}

func handleAPIMeta(w http.ResponseWriter, _ *http.Request) {
	jsonResp(w, http.StatusOK, map[string]any{
		"protocol": types.PeerProtocol,
		"features": []string{"inventory", "peer-relay", "service-token", "instance-capabilities"},
	})
}

func kubeClient(r *http.Request, ns string) *kubevirt.Client {
	return kubevirt.NewClientForContext(ns, r.URL.Query().Get("context"))
}

type vmiInfo struct {
	IP   string
	Node string
}

func vmiIndex() map[string]vmiInfo {
	out, err := defaultRunner.Run("kubectl", "get", "vmis", "-A", "-o", "json")
	if err != nil {
		return nil
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				NodeName   string `json:"nodeName"`
				Interfaces []struct {
					IPAddress string `json:"ipAddress"`
				} `json:"interfaces"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	idx := make(map[string]vmiInfo, len(res.Items))
	for _, it := range res.Items {
		info := vmiInfo{Node: it.Status.NodeName}
		if len(it.Status.Interfaces) > 0 {
			info.IP = it.Status.Interfaces[0].IPAddress
		}
		idx[it.Metadata.Namespace+"/"+it.Metadata.Name] = info
	}
	return idx
}

// ── Create / delete / actions ─────────────────────────────────────

type createRequest struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	CPU           int    `json:"cpu"`
	Mem           string `json:"mem"`
	Disk          string `json:"disk"`
	ContainerDisk string `json:"containerDisk"`
	Image         string `json:"image"`  // catalog name
	Import        string `json:"import"` // qcow2/raw URL
	ISO           string `json:"iso"`
	PVC           string `json:"pvc"`
	Bootc         string `json:"bootc"`
	SSHKey        string `json:"sshKey"`
	Node          string `json:"node"`
	CloudInit     string `json:"cloudInit"`
	InstanceType  string `json:"instancetype"`
	Preference    string `json:"preference"`
	Windows       bool   `json:"windows"`      // Windows installer flow (windows plugin)
	StorageClass  string `json:"storageClass"` // overrides the cluster-preferred StorageClass
	Target        string `json:"target"`       // named Corral context; legacy "cluster" and "local" remain accepted
}

// buildTask tracks a long-running bootc build kicked off from the UI.
type buildTask struct {
	mu     sync.Mutex
	log    strings.Builder
	status string // "running", "done", "error"
	errMsg string
	done   chan struct{}
}

// newBuildTask creates a running task. Always use this over a bare struct
// literal — done must be initialized or wait() blocks forever.
func newBuildTask() *buildTask {
	return &buildTask{status: "running", done: make(chan struct{})}
}

func (t *buildTask) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.log.Write(p)
}

func (t *buildTask) finish(err error) {
	t.mu.Lock()
	if err != nil {
		t.status = "error"
		t.errMsg = err.Error()
	} else {
		t.status = "done"
	}
	t.mu.Unlock()
	close(t.done)
}

// wait blocks until finish has been called — lets tests synchronize on a
// background task instead of racing its goroutine.
func (t *buildTask) wait() {
	<-t.done
}

func (t *buildTask) snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]string{"status": t.status, "log": t.log.String(), "error": t.errMsg}
}

var tasks sync.Map // id → *buildTask

func handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	v, ok := tasks.Load(r.PathValue("id"))
	if !ok {
		errResp(w, http.StatusNotFound, fmt.Errorf("no such task"))
		return
	}
	jsonResp(w, http.StatusOK, v.(*buildTask).snapshot())
}

// handleCreateVM dispatches to the strategy matching the request shape —
// bootc build, Windows guided install, or everything else (catalog images,
// container disks, import URLs, ISO installs, PVC-backed) — and translates
// the result into the right HTTP response. The strategies themselves
// (createBootc/createWindows/createGeneric) don't touch http.ResponseWriter,
// so tests can call them directly instead of racing a goroutine through a
// full HTTP round-trip.
func handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	ns := req.Namespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}

	target, err := resolveCreateTarget(req.Target, req)
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	if target.Backend == "qemu" {
		createLocalVM(w, req)
		return
	}
	if target.Backend != "kubevirt" {
		createBackendVM(w, req, target)
		return
	}

	switch {
	case req.Bootc != "":
		id, _, err := createBootcInContext(req, ns, target.Context)
		if err != nil {
			errResp(w, statusFor(err), err)
			return
		}
		jsonResp(w, http.StatusAccepted, map[string]string{"task": id})

	case req.Windows:
		if err := createWindowsInContext(req, ns, target.Context); err != nil {
			errResp(w, statusFor(err), err)
			return
		}
		jsonResp(w, http.StatusCreated, map[string]string{"name": req.Name, "namespace": ns})

	default:
		if err := createGenericInContext(req, ns, target.Context); err != nil {
			errResp(w, statusFor(err), err)
			return
		}
		jsonResp(w, http.StatusCreated, map[string]string{"name": req.Name, "namespace": ns})
	}
}

func handleContexts(w http.ResponseWriter, _ *http.Request) {
	jsonResp(w, http.StatusOK, map[string]any{"default": config.DefaultContext().Name, "contexts": config.Contexts()})
}

func resolveCreateTarget(name string, req createRequest) (config.ContextConfig, error) {
	switch name {
	case "":
		// Backward compatibility for API clients predating named targets. The
		// default context is the right answer for a request the local backend
		// can actually serve; a KubeVirt-shaped one still means the cluster,
		// because createLocalVM silently ignores those fields and would fail
		// with an unrelated "needs an ISO" message.
		target := config.DefaultContext()
		if target.Backend != "qemu" || !needsCluster(req) {
			return target, nil
		}
		return clusterContext()
	case "local":
		return config.ContextConfig{Name: "local", Backend: "qemu"}, nil
	case "cluster":
		// Legacy alias, and the fallback the dashboard hardcodes when
		// /api/contexts is unreachable: it has always meant the KubeVirt
		// cluster and must never resolve to the local backend.
		return clusterContext()
	default:
		if target, ok := config.FindContext(name); ok {
			return target, nil
		}
		return config.ContextConfig{}, fmt.Errorf("unknown context %q", name)
	}
}

// needsCluster reports whether a request uses fields only the KubeVirt backend
// implements. createLocalVM reads ISO and Import and nothing else, so anything
// listed here would otherwise be dropped on the floor.
func needsCluster(req createRequest) bool {
	return req.ContainerDisk != "" || req.PVC != "" || req.Bootc != "" ||
		req.Image != "" || req.Windows
}

// clusterContext returns the KubeVirt context to create in.
func clusterContext() (config.ContextConfig, error) {
	if target, ok := config.FindContext("kubevirt"); ok && target.Backend == "kubevirt" {
		return target, nil
	}
	for _, c := range config.Contexts() {
		if c.Backend == "kubevirt" {
			return c, nil
		}
	}
	return config.ContextConfig{}, fmt.Errorf("no KubeVirt context is configured — name a target explicitly (see corral context ls)")
}

func handleVMAction(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if peer, remoteNS, ok := parsePeerNamespace(ns); ok {
		proxyPeerVM(w, r, peer, remoteNS)
		return
	}
	action := r.PathValue("action")
	if ns == "incus" {
		c := incus.NewClient(r.URL.Query().Get("context"))
		var err error
		switch action {
		case "start":
			err = c.Start(name)
		case "stop":
			err = c.Stop(name)
		case "restart":
			if err = c.Stop(name); err == nil {
				err = c.Start(name)
			}
		default:
			errResp(w, 400, fmt.Errorf("%q is not supported for Incus instances", action))
			return
		}
		if err != nil {
			errResp(w, 500, err)
			return
		}
		jsonResp(w, 200, map[string]string{"status": "ok"})
		return
	}
	if ns == "libvirt" {
		c := libvirt.NewClient(r.URL.Query().Get("context"))
		var err error
		switch action {
		case "start":
			err = c.Start(name)
		case "stop":
			err = c.Stop(name)
		case "restart":
			if err = c.Stop(name); err == nil {
				err = c.Start(name)
			}
		default:
			errResp(w, 400, fmt.Errorf("%q is not supported for libvirt domains", action))
			return
		}
		if err != nil {
			errResp(w, 500, err)
			return
		}
		jsonResp(w, 200, map[string]string{"status": "ok"})
		return
	}
	if ns == localNS {
		localVMAction(w, name, action)
		return
	}

	c := kubeClient(r, ns)
	var err error
	done := taskBegin(action, ns+"/"+name)
	switch action {
	case "start":
		err = c.StartVM(name)
	case "stop":
		err = c.StopVM(name)
	case "restart":
		err = c.RestartVM(name)
	case "pause":
		err = c.PauseVM(name)
	case "unpause":
		err = c.UnpauseVM(name)
	case "migrate":
		var b struct {
			TargetNode string `json:"targetNode"`
		}
		json.NewDecoder(r.Body).Decode(&b) // empty body is fine
		err = c.Migrate(name, b.TargetNode)
	default:
		done(fmt.Errorf("unknown action"))
		errResp(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", action))
		return
	}
	done(err)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMigrate triggers a live migration and tracks it as a background task
// with live progress, polling the VMI's migrationState until it completes or
// fails. The trigger itself runs synchronously so "not migratable" errors
// surface immediately as a 4xx/5xx; the watch then streams progress.
func handleMigrate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var b struct {
		TargetNode string `json:"targetNode"`
	}
	json.NewDecoder(r.Body).Decode(&b) // empty body = let the scheduler choose

	c := kubeClient(r, ns)
	if err := c.Migrate(name, b.TargetNode); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}

	id := fmt.Sprintf("migrate-%s-%d", name, time.Now().UnixNano())
	task := newBuildTask()
	tasks.Store(id, task)
	done := taskBegin("migrate", ns+"/"+name)
	if b.TargetNode != "" {
		fmt.Fprintf(task, "Live migration of %s requested → %s\n", name, b.TargetNode)
	} else {
		fmt.Fprintf(task, "Live migration of %s requested (scheduler picks target)\n", name)
	}

	go func() {
		err := watchMigration(c, name, task)
		task.finish(err)
		done(err)
	}()
	jsonResp(w, http.StatusAccepted, map[string]string{"task": id})
}

// Timings for watchMigration — package vars so tests can shrink them.
var (
	migrationTimeout      = 10 * time.Minute
	migrationSettle       = 2 * time.Second // wait for the VMIM to register
	migrationPollInterval = 2 * time.Second
)

// watchMigration polls a VM's migrationState, writing a line on each phase
// change, until it completes (nil), fails, or times out.
func watchMigration(c kubevirt.VMAdvanced, name string, w io.Writer) error {
	time.Sleep(migrationSettle) // avoid reading a stale state from a prior migration
	deadline := time.Now().Add(migrationTimeout)
	last := ""
	for time.Now().Before(deadline) {
		st, err := c.MigrationState(name)
		if err == nil && st.Present {
			phase := "pending"
			switch {
			case st.Completed:
				phase = "completed"
			case st.Failed:
				phase = "failed"
			case st.Active:
				phase = "migrating"
			}
			if phase != last {
				fmt.Fprintf(w, "%s → %s: %s\n", or(st.SourceNode, "?"), or(st.TargetNode, "?"), phase)
				last = phase
			}
			if st.Completed {
				fmt.Fprintf(w, "Migration complete — now running on %s\n", st.TargetNode)
				return nil
			}
			if st.Failed {
				return fmt.Errorf("live migration of %s failed", name)
			}
		}
		time.Sleep(migrationPollInterval)
	}
	return fmt.Errorf("live migration of %s timed out after %s", name, migrationTimeout)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// handleExport streams a VM disk backup (gzip) to the browser. The VM must be
// stopped (its RWO disk can't be read while running). The disk is exported to a
// pod-local temp file via virtctl, then streamed and removed.
func handleExport(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if _, err := defaultRunner.Run("kubectl", "get", "vmi", name, "-n", ns); err == nil {
		errResp(w, http.StatusConflict, fmt.Errorf("stop %s before exporting (its disk is in use while running)", name))
		return
	}
	if r.URL.Query().Get("format") == "qcow2" {
		exportQcow2(w, r, ns, name)
		return
	}
	tmp, err := os.CreateTemp("", name+"-*.img.gz")
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)

	if _, err := kubeClient(r, ns).Export(name, "", tmpName); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	f, err := os.Open(tmpName)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".img.gz"))
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	io.Copy(w, f)
}

// exportQcow2 downloads the VM disk raw, converts it to compressed qcow2 with
// qemu-img, and streams the result. Needs qemu-img on the server and scratch
// space sized to the raw disk; degrades to a clear error otherwise (the default
// raw.gz export still works without either).
func exportQcow2(w http.ResponseWriter, r *http.Request, ns, name string) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		errResp(w, http.StatusNotImplemented,
			fmt.Errorf("qcow2 export needs qemu-img on the server (not installed); use the default raw.gz download instead"))
		return
	}
	dir, err := os.MkdirTemp("", "corral-export-"+name+"-*")
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	rawPath := filepath.Join(dir, name+".raw")
	qcowPath := filepath.Join(dir, name+".qcow2")

	if _, err := kubeClient(r, ns).ExportRaw(name, "", rawPath); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	if err := convertRawToQcow2(qemuImg, rawPath, qcowPath); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	os.Remove(rawPath) // free the raw before streaming the qcow2

	f, err := os.Open(qcowPath)
	if err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".qcow2"))
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	io.Copy(w, f)
}

// convertRawToQcow2 runs qemu-img to turn a raw disk into a zlib-compressed
// qcow2 (-c): compact, seekable, and re-importable by CDI/qemu. Extracted so the
// conversion (and its exact flags) is unit-testable with a real qemu-img.
func convertRawToQcow2(qemuImg, rawPath, qcowPath string) error {
	cmd := exec.Command(qemuImg, "convert", "-f", "raw", "-O", "qcow2", "-c", rawPath, qcowPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if peer, remoteNS, ok := parsePeerNamespace(ns); ok {
		proxyPeerVM(w, r, peer, remoteNS)
		return
	}
	if ns == "incus" {
		if err := incus.NewClient(r.URL.Query().Get("context")).Delete(name); err != nil {
			errResp(w, 500, err)
			return
		}
		jsonResp(w, 200, map[string]string{"status": "deleted"})
		return
	}
	if ns == "libvirt" {
		if r.URL.Query().Get("destroyStorage") != "true" {
			errResp(w, http.StatusBadRequest, fmt.Errorf("libvirt deletion requires destroyStorage=true because it permanently removes domain disks"))
			return
		}
		if err := libvirt.NewClient(r.URL.Query().Get("context")).Delete(name); err != nil {
			errResp(w, 500, err)
			return
		}
		jsonResp(w, 200, map[string]string{"status": "deleted"})
		return
	}
	done := taskBegin("delete", ns+"/"+name)
	if ns == localNS {
		if err := qemu.Delete(name); err != nil {
			done(err)
			errResp(w, http.StatusInternalServerError, err)
			return
		}
		done(nil)
		if store != nil {
			store.Remove(name)
		}
		jsonResp(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if err := kubeClient(r, ns).DeleteVM(name); err != nil {
		done(err)
		errResp(w, http.StatusInternalServerError, err)
		return
	}
	done(nil)
	if store != nil {
		store.Remove(name)
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func handleVMInfo(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if peer, remoteNS, ok := parsePeerNamespace(ns); ok {
		proxyPeerVM(w, r, peer, remoteNS)
		return
	}
	if ns == "incus" {
		data, err := incus.NewClient(r.URL.Query().Get("context")).Info(name)
		if err != nil {
			errResp(w, 404, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
		return
	}
	if ns == "libvirt" {
		data, err := libvirt.NewClient(r.URL.Query().Get("context")).Info(name)
		if err != nil {
			errResp(w, 404, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
		return
	}
	var data []byte
	var err error
	if ns == localNS {
		data, err = qemu.Info(name)
	} else {
		data, err = kubeClient(r, ns).VMInfo(name)
	}
	if err != nil {
		errResp(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// ── Nodes ─────────────────────────────────────────────────────────

type nodeResp struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Roles   string `json:"roles"`
	Kubelet string `json:"kubelet"`
	Arch    string `json:"arch"`
}

func handleNodes(w http.ResponseWriter, r *http.Request) {
	// This host appears as a synthetic node whenever it has local QEMU VMs,
	// so they group under one tree entry alongside the cluster nodes.
	var localNode []nodeResp
	if len(localVMs()) > 0 {
		localNode = []nodeResp{{Name: "local", Ready: true, Roles: "this host (qemu)"}}
	}

	out, err := defaultRunner.Run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		if localNode != nil {
			jsonResp(w, http.StatusOK, localNode)
			return
		}
		errResp(w, http.StatusBadGateway, fmt.Errorf("listing nodes: %w", err))
		return
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
					Architecture   string `json:"architecture"`
				} `json:"nodeInfo"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		errResp(w, http.StatusInternalServerError, err)
		return
	}

	nodes := []nodeResp{}
	for _, it := range res.Items {
		n := nodeResp{
			Name:    it.Metadata.Name,
			Kubelet: it.Status.NodeInfo.KubeletVersion,
			Arch:    it.Status.NodeInfo.Architecture,
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				n.Ready = true
			}
		}
		var roles []string
		for label := range it.Metadata.Labels {
			if rest, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok {
				roles = append(roles, rest)
			}
		}
		n.Roles = strings.Join(roles, ",")
		nodes = append(nodes, n)
	}
	nodes = append(nodes, localNode...)
	jsonResp(w, http.StatusOK, nodes)
}

// ── Consoles ──────────────────────────────────────────────────────

// consoleDialer is the seam vncBridge/rdpBridge open their connections
// through — swapped for a fake in tests.
var consoleDialer kubevirt.ConsoleDialer = kubevirt.RealConsoleDialer{}

// vncBridge proxies a binary websocket (noVNC) to the VM's VNC console.
func vncBridge(ws *websocket.Conn) {
	defer ws.Close()
	ns, name := ws.Request().PathValue("ns"), ws.Request().PathValue("name")
	if ns == "" || name == "" {
		return
	}
	if bridgePeerConsole(ws, "vnc") {
		return
	}

	// Local QEMU VMs (#91 Phase 2): their VNC server is a plain TCP listener
	// on this host — dial it directly instead of the virtctl proxy.
	var conn io.ReadWriteCloser
	var err error
	if ns == localNS {
		conn, err = dialLocalVNC(name)
	} else if ns == "libvirt" {
		conn, err = libvirt.NewClient(ws.Request().URL.Query().Get("context")).DialVNC(name)
	} else if context := ws.Request().URL.Query().Get("context"); context != "" {
		conn, err = (kubevirt.RealConsoleDialer{Context: context}).Dial(ns, name, kubevirt.VNC)
	} else {
		conn, err = consoleDialer.Dial(ns, name, kubevirt.VNC)
	}
	if err != nil {
		return
	}
	defer conn.Close()

	ws.PayloadType = websocket.BinaryFrame
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, conn); done <- struct{}{} }()
	<-done
}

// ttyBridge proxies a binary websocket (xterm.js) to a terminal — `virtctl
// console` for a VM, or `kubectl exec` for a CT (#50: CTs have no
// framebuffer/serial console, only exec/attach). Tries the VM path first
// since that's the common case; falls back to CT only when no VM by that
// name exists.
func ttyBridge(ws *websocket.Conn) {
	defer ws.Close()
	ns, name := ws.Request().PathValue("ns"), ws.Request().PathValue("name")
	if ns == "" || name == "" {
		return
	}
	if bridgePeerConsole(ws, "tty") {
		return
	}
	if ns == "incus" {
		remote := rURLContext(ws.Request())
		if remote == "" {
			remote = config.IncusRemote()
		}
		bridgeConsolePTY(ws, exec.Command("incus", "exec", remote+":"+name, "--", "bash"))
		return
	}

	isVM := kubevirt.NewClientForContext(ns, ws.Request().URL.Query().Get("context")).VMExists(name)

	ws.PayloadType = websocket.BinaryFrame
	if isVM {
		bridgeConsolePipes(ws, shell.CommandForContext(ws.Request().URL.Query().Get("context"), "virtctl", "console", name, "-n", ns))
		return
	}
	// kubectl exec's -t requires a real local TTY on its end — it checks
	// isatty on this process's stdin, which a bare os/exec pipe fails (kubectl
	// errors "Unable to use a TTY - input is not a terminal or the right kind
	// of file"). virtctl console has no such check (it's not an exec session,
	// it's a virtio-serial device), which is why the VM path above can use
	// plain pipes but this one needs a real pty.
	shellCmd, err := ct.ExecCommand(name, ns)
	if err != nil {
		return
	}
	args := append([]string{"exec", "-i", "-t", name, "-n", ns, "--"}, shellCmd...)
	bridgeConsolePTY(ws, shell.CommandForContext(ws.Request().URL.Query().Get("context"), "kubectl", args...))
}

func rURLContext(r *http.Request) string { return r.URL.Query().Get("context") }

// bridgeConsolePipes wires cmd's stdin/stdout to ws via plain OS pipes —
// fine for commands (like virtctl console) that don't check isatty.
func bridgeConsolePipes(ws *websocket.Conn, cmd *exec.Cmd) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	done := make(chan struct{}, 2)
	go func() { io.Copy(stdin, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, stdout); done <- struct{}{} }()
	<-done
}

// bridgeConsolePTY wires cmd to a real pseudo-terminal — needed for
// commands (like kubectl exec -t) that check isatty on their own stdin.
func bridgeConsolePTY(ws *websocket.Conn, cmd *exec.Cmd) {
	f, err := pty.Start(cmd)
	if err != nil {
		return
	}
	defer func() {
		f.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	done := make(chan struct{}, 2)
	go func() { io.Copy(f, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, f); done <- struct{}{} }()
	<-done
}
