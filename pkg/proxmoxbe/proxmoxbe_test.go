package proxmoxbe

// Tests against a fake PVE.
//
// There is no shell.Runner seam here because there is no command to fake — the
// seam is HTTP, so these run a httptest server that answers with recorded PVE
// payload shapes and records what was asked of it. The assertions are about the
// request Corral makes (path, method, parameters) and what it does with the
// answer, which is the same contract the fake-runner tests hold the other
// backends to.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "corral@pve!ci=00000000-0000-0000-0000-000000000000"

// fakePVE is a scriptable Proxmox. Handlers are matched by "METHOD /path"; the
// query string is available to the handler and recorded for assertions.
type fakePVE struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]func(r *http.Request) (any, int)
	requests []recordedRequest
	server   *httptest.Server
}

type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	Auth   string
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{t: t, handlers: map[string]func(*http.Request) (any, int){}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	// Every cluster answers these; individual tests override as needed.
	f.on("GET /version", map[string]any{"version": "8.3.0", "release": "8.3"})
	f.on("GET /nodes", []map[string]any{
		{"node": "pve1", "status": "online", "maxcpu": 8, "maxmem": 34359738368},
		{"node": "pve2", "status": "online", "maxcpu": 8, "maxmem": 34359738368},
	})
	f.on("GET /cluster/resources", defaultResources())
	f.on("GET /cluster/nextid", "131")
	return f
}

// defaultResources is a small mixed cluster: a running VM, a stopped VM, a
// template, and an LXC container.
func defaultResources() []map[string]any {
	return []map[string]any{
		{"vmid": 100, "name": "web-prod", "node": "pve1", "type": "qemu", "status": "running",
			"maxcpu": 4, "maxmem": 8589934592, "maxdisk": 34359738368, "cpu": 0.25, "mem": 4294967296,
			"tags": "prod,web"},
		{"vmid": 101, "name": "db-prod", "node": "pve2", "type": "qemu", "status": "stopped",
			"maxcpu": 8, "maxmem": 17179869184, "maxdisk": 107374182400},
		{"vmid": 900, "name": "debian-template", "node": "pve1", "type": "qemu", "status": "stopped",
			"template": 1, "maxcpu": 2, "maxmem": 2147483648},
		{"vmid": 200, "name": "files-ct", "node": "pve1", "type": "lxc", "status": "running",
			"maxcpu": 1, "maxmem": 536870912, "maxdisk": 8589934592},
		// A storage row: /cluster/resources?type=vm is a loose filter, and a
		// non-guest resource must not become a VM.
		{"id": "storage/pve1/local", "type": "storage", "node": "pve1", "status": "available"},
	}
}

func (f *fakePVE) on(route string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = func(*http.Request) (any, int) { return data, http.StatusOK }
}

func (f *fakePVE) onFunc(route string, h func(r *http.Request) (any, int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

func (f *fakePVE) fail(route string, status int, message string) {
	f.onFunc(route, func(*http.Request) (any, int) {
		return map[string]any{"message": message}, status
	})
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: path, Query: r.URL.Query(),
		Form: r.PostForm, Auth: r.Header.Get("Authorization"),
	})
	handler, ok := f.handlers[r.Method+" "+path]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": fmt.Sprintf("no fake handler for %s %s", r.Method, path)})
		return
	}
	data, status := handler(r)
	w.WriteHeader(status)
	if status >= 400 {
		_ = json.NewEncoder(w).Encode(data)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakePVE) client() *Client {
	f.t.Helper()
	client, err := New(Config{Host: strings.TrimPrefix(f.server.URL, "http://"), Token: testToken})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	// The httptest server is plain HTTP, so the base URL is rewritten rather
	// than the TLS config relaxed — nothing here should exercise the insecure
	// path by accident.
	client.base = f.server.URL + "/api2/json"
	return client.WithHTTPClient(f.server.Client())
}

func (f *fakePVE) calls(method, path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakePVE) lastCall(method, path string) recordedRequest {
	f.t.Helper()
	calls := f.calls(method, path)
	if len(calls) == 0 {
		f.mu.Lock()
		var seen []string
		for _, r := range f.requests {
			seen = append(seen, r.Method+" "+r.Path)
		}
		f.mu.Unlock()
		f.t.Fatalf("no %s %s call; saw %v", method, path, seen)
	}
	return calls[len(calls)-1]
}

// ── configuration ─────────────────────────────────────────────────

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no host", Config{Token: testToken}, "host is required"},
		{"no token", Config{Host: "pve.example.com"}, "API token is required"},
		{"token not a token", Config{Host: "pve.example.com", Token: "hunter2"}, "USER@REALM"},
		{"short fingerprint", Config{Host: "h", Token: testToken, Fingerprint: "ab:cd"}, "SHA-256"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg)
			if err == nil {
				t.Fatal("expected a configuration error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// A token must never appear in an error message, even when the error is about
// the token.
func TestTokenIsRedactedInErrors(t *testing.T) {
	secret := "corral@pve!ci=super-secret-uuid"
	_, err := New(Config{Host: "pve.example.com", Token: "no-bang-or-equals"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, err := New(Config{Host: "h", Token: secret + "!"}); err != nil {
		if strings.Contains(err.Error(), "super-secret-uuid") {
			t.Errorf("the token leaked into %q", err)
		}
	}
	if got := redactToken(secret); strings.Contains(got, "super-secret-uuid") {
		t.Errorf("redactToken kept the secret: %q", got)
	}
}

func TestHostNormalisation(t *testing.T) {
	for _, host := range []string{"pve.example.com", "pve.example.com:8006", "https://pve.example.com"} {
		client, err := New(Config{Host: host, Token: testToken})
		if err != nil {
			t.Fatalf("New(%q): %v", host, err)
		}
		if !strings.HasPrefix(client.base, "https://pve.example.com:8006/api2/json") {
			t.Errorf("host %q became base %q", host, client.base)
		}
	}
}

func TestEveryRequestCarriesTheToken(t *testing.T) {
	f := newFakePVE(t)
	if _, err := f.client().List(); err != nil {
		t.Fatalf("List: %v", err)
	}
	call := f.lastCall("GET", "/cluster/resources")
	if call.Auth != "PVEAPIToken="+testToken {
		t.Errorf("Authorization = %q", call.Auth)
	}
}

// ── inventory ─────────────────────────────────────────────────────

func TestListReturnsVMsNotContainers(t *testing.T) {
	f := newFakePVE(t)

	vms, err := f.client().List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != 3 {
		t.Fatalf("List returned %d, want the three qemu guests: %+v", len(vms), vms)
	}
	names := map[string]bool{}
	for _, vm := range vms {
		names[vm.Name] = true
		if vm.Backend != "proxmox" {
			t.Errorf("%s has backend %q", vm.Name, vm.Backend)
		}
		// PVE has no namespace, and repurposing the field would make identity
		// mean two things.
		if vm.Namespace != "" {
			t.Errorf("%s has namespace %q, want it empty", vm.Name, vm.Namespace)
		}
		if vm.ID == "" {
			t.Errorf("%s has no canonical identity", vm.Name)
		}
	}
	if names["files-ct"] {
		t.Error("an LXC container was listed as a VM")
	}
	if names["local"] {
		t.Error("a storage resource was listed as a VM")
	}
}

func TestListMapsShapeAndState(t *testing.T) {
	f := newFakePVE(t)
	vms, _ := f.client().List()

	byName := map[string]struct {
		cpu     int
		mem     string
		disk    string
		node    string
		status  string
		running bool
		tags    int
		tmpl    bool
	}{}
	for _, vm := range vms {
		byName[vm.Name] = struct {
			cpu     int
			mem     string
			disk    string
			node    string
			status  string
			running bool
			tags    int
			tmpl    bool
		}{vm.CPU, vm.Mem, vm.Disk, vm.Node, vm.Status, vm.Running, len(vm.Tags), vm.IsTemplate}
	}

	web := byName["web-prod"]
	if web.cpu != 4 || web.mem != "8Gi" || web.disk != "32Gi" {
		t.Errorf("web-prod shape = %d CPU / %s / %s", web.cpu, web.mem, web.disk)
	}
	if web.node != "pve1" || web.status != "Running" || !web.running {
		t.Errorf("web-prod state = %s on %s (running %v)", web.status, web.node, web.running)
	}
	// PVE has tags natively, so they map across rather than being emulated.
	if web.tags != 2 {
		t.Errorf("web-prod has %d tags, want 2", web.tags)
	}
	if db := byName["db-prod"]; db.running || db.status != "Stopped" {
		t.Errorf("db-prod state = %s (running %v)", db.status, db.running)
	}
	if tmpl := byName["debian-template"]; !tmpl.tmpl {
		t.Error("the template's own PVE flag was lost")
	}
}

// A lock is not a status Corral can ignore: a guest mid-migration is busy, and
// the TUI's state classification keys off exactly these words.
func TestLockedGuestReportsWhatItIsDoing(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /cluster/resources", []map[string]any{
		{"vmid": 100, "name": "web-prod", "node": "pve1", "type": "qemu", "status": "running",
			"lock": "migrate", "maxcpu": 2, "maxmem": 2147483648},
	})
	vms, _ := f.client().List()
	if vms[0].Status != "Migrating" {
		t.Errorf("status = %q, want Migrating", vms[0].Status)
	}
	if vms[0].Ready {
		t.Error("a locked guest should not read as ready")
	}
}

func TestContainersReturnsLXCOnly(t *testing.T) {
	f := newFakePVE(t)
	cts, err := f.client().Containers()
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(cts) != 1 || cts[0].Name != "files-ct" {
		t.Fatalf("Containers = %+v, want just files-ct", cts)
	}
	if cts[0].Kind() != KindLXC {
		t.Errorf("kind = %q", cts[0].Kind())
	}
}

func TestResolveByNameAndVMID(t *testing.T) {
	f := newFakePVE(t)
	client := f.client()

	guest, err := client.Resolve("web-prod")
	if err != nil {
		t.Fatalf("Resolve by name: %v", err)
	}
	if guest.VMID != 100 || guest.Node != "pve1" || guest.Kind != KindQemu {
		t.Errorf("resolved to %+v", guest)
	}
	if got := guest.path(); got != "/nodes/pve1/qemu/100" {
		t.Errorf("guest path = %q", got)
	}

	// PVE users think in vmids; refusing them would make the backend feel
	// foreign from that side.
	byID, err := client.Resolve("200")
	if err != nil {
		t.Fatalf("Resolve by vmid: %v", err)
	}
	if byID.Name != "files-ct" || byID.Kind != KindLXC {
		t.Errorf("vmid 200 resolved to %+v", byID)
	}
	if got := byID.path(); got != "/nodes/pve1/lxc/200" {
		t.Errorf("container path = %q, want the lxc endpoint", got)
	}

	if _, err := client.Resolve("nope"); err == nil {
		t.Error("resolving an unknown name should fail")
	}
	if _, err := client.Resolve("999"); err == nil {
		t.Error("resolving an unknown vmid should fail")
	}
}

// PVE allows duplicate names across nodes. Acting on a guess would be worse than
// refusing with the vmids.
func TestResolveRefusesAmbiguity(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /cluster/resources", []map[string]any{
		{"vmid": 100, "name": "twin", "node": "pve1", "type": "qemu", "status": "running"},
		{"vmid": 101, "name": "twin", "node": "pve2", "type": "qemu", "status": "running"},
	})
	_, err := f.client().Resolve("twin")
	if err == nil {
		t.Fatal("expected an ambiguity refusal")
	}
	for _, want := range []string{"ambiguous", "100", "101", "vmid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestExistsAndVMIDMap(t *testing.T) {
	f := newFakePVE(t)
	client := f.client()
	if !client.Exists("web-prod") {
		t.Error("web-prod should exist")
	}
	if client.Exists("ghost") {
		t.Error("ghost should not exist")
	}
	ids, err := client.VMIDMap()
	if err != nil {
		t.Fatalf("VMIDMap: %v", err)
	}
	if ids["web-prod"] != 100 || ids["files-ct"] != 200 {
		t.Errorf("VMIDMap = %v", ids)
	}
}

// ── lifecycle ─────────────────────────────────────────────────────

func TestPowerActionsHitTheRightEndpoints(t *testing.T) {
	f := newFakePVE(t)
	client := f.client()
	upid := "UPID:pve1:0000ABCD:00000001:6900:qmstart:100:corral@pve:"

	for _, c := range []struct {
		name   string
		call   func() (Task, error)
		path   string
		reason string
	}{
		{"start", func() (Task, error) { return client.Start("web-prod") }, "/nodes/pve1/qemu/100/status/start", ""},
		{"stop", func() (Task, error) { return client.Stop("web-prod") }, "/nodes/pve1/qemu/100/status/shutdown",
			"Stop must ask the guest, not pull the plug"},
		{"kill", func() (Task, error) { return client.Kill("web-prod") }, "/nodes/pve1/qemu/100/status/stop",
			"the forced power cut has to be chosen by name"},
		{"restart", func() (Task, error) { return client.Restart("web-prod") }, "/nodes/pve1/qemu/100/status/reboot",
			"a real reboot, not stop-then-start"},
		{"pause", func() (Task, error) { return client.Pause("web-prod") }, "/nodes/pve1/qemu/100/status/suspend", ""},
		{"resume", func() (Task, error) { return client.Resume("web-prod") }, "/nodes/pve1/qemu/100/status/resume", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.on("POST "+c.path, upid)
			task, err := c.call()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			f.lastCall("POST", c.path)
			if !task.Valid() || task.Node != "pve1" {
				t.Errorf("task = %+v, want a UPID on pve1 — %s", task, c.reason)
			}
		})
	}
}

func TestDeletePurges(t *testing.T) {
	f := newFakePVE(t)
	f.on("DELETE /nodes/pve1/qemu/100", "UPID:pve1:0:0:0:qmdestroy:100:corral@pve:")

	if _, err := f.client().Delete("web-prod"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	call := f.lastCall("DELETE", "/nodes/pve1/qemu/100")
	// Without purge, backup jobs outlive the guest and start capturing whatever
	// reuses the vmid.
	if call.Query.Get("purge") != "1" {
		t.Errorf("delete query = %v, want purge=1", call.Query)
	}
}

// ── shape ─────────────────────────────────────────────────────────

func TestScaleSendsCoresAndMiB(t *testing.T) {
	f := newFakePVE(t)
	f.on("PUT /nodes/pve1/qemu/100/config", nil)

	if err := f.client().Scale("web-prod", 8, "16Gi"); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	call := f.lastCall("PUT", "/nodes/pve1/qemu/100/config")
	if got := call.Form.Get("cores"); got != "8" {
		t.Errorf("cores = %q", got)
	}
	// PVE's memory is MiB, always.
	if got := call.Form.Get("memory"); got != "16384" {
		t.Errorf("memory = %q, want 16384 MiB", got)
	}
}

func TestMemoryMiB(t *testing.T) {
	cases := map[string]int{
		"4Gi": 4096, "4G": 4096, "4gb": 4096,
		"512Mi": 512, "512M": 512, "2048": 2048,
		"1.5Gi": 1536,
	}
	for in, want := range cases {
		got, err := memoryMiB(in)
		if err != nil {
			t.Errorf("memoryMiB(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("memoryMiB(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "lots", "0", "-4Gi"} {
		if _, err := memoryMiB(bad); err == nil {
			t.Errorf("memoryMiB(%q) should fail", bad)
		}
	}
}

func TestExpandDiskAlwaysGrows(t *testing.T) {
	f := newFakePVE(t)
	f.on("PUT /nodes/pve1/qemu/100/resize", nil)

	if err := f.client().ExpandDisk("web-prod", "scsi0", "10G"); err != nil {
		t.Fatalf("ExpandDisk: %v", err)
	}
	call := f.lastCall("PUT", "/nodes/pve1/qemu/100/resize")
	// A bare size reads as an absolute target and is refused for anything
	// smaller than the current disk; growth is the only operation offered.
	if got := call.Form.Get("size"); got != "+10G" {
		t.Errorf("size = %q, want +10G", got)
	}
}

func TestSetTagPreservesTheOthers(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/config", map[string]any{
		"name": "web-prod", "cores": 4, "memory": 8192, "tags": "prod,web"})
	f.on("PUT /nodes/pve1/qemu/100/config", nil)

	if err := f.client().SetTag("web-prod", "sla-gold", true); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	got := f.lastCall("PUT", "/nodes/pve1/qemu/100/config").Form.Get("tags")
	// PVE stores tags as one string, so a blind write erases the others.
	if got != "prod,sla-gold,web" {
		t.Errorf("tags = %q, want the existing tags kept", got)
	}

	if err := f.client().SetTag("web-prod", "web", false); err != nil {
		t.Fatalf("SetTag remove: %v", err)
	}
	if got := f.lastCall("PUT", "/nodes/pve1/qemu/100/config").Form.Get("tags"); got != "prod" {
		t.Errorf("tags after removal = %q, want prod", got)
	}
}

func TestRemovingTheLastTagClearsTheField(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/config", map[string]any{"tags": "only"})
	f.on("PUT /nodes/pve1/qemu/100/config", nil)

	if err := f.client().SetTag("web-prod", "only", false); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	call := f.lastCall("PUT", "/nodes/pve1/qemu/100/config")
	if _, present := call.Form["tags"]; !present {
		t.Error("removing the last tag must send an empty tags field, not omit it")
	}
	if got := call.Form.Get("tags"); got != "" {
		t.Errorf("tags = %q, want empty", got)
	}
}

// PVE templates are one-way. Failing with a parameter error would be worse than
// saying so.
func TestTemplateIsOneWay(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu/100/template", nil)

	if err := f.client().MarkTemplate("web-prod", true); err != nil {
		t.Fatalf("MarkTemplate: %v", err)
	}
	f.lastCall("POST", "/nodes/pve1/qemu/100/template")

	err := f.client().MarkTemplate("web-prod", false)
	if err == nil {
		t.Fatal("unmarking a template should be refused")
	}
	if !strings.Contains(err.Error(), "clone it") {
		t.Errorf("refusal %q should say what to do instead", err)
	}
}

// ── movement ──────────────────────────────────────────────────────

func TestMigrateUsesPreconditionsAndGoesOnline(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/status/current", map[string]any{
		"status": "running", "cpus": 4, "cpu": 0.25, "mem": 4294967296, "qmpstatus": "running", "agent": 1})
	f.on("GET /nodes/pve1/qemu/100/migrate", map[string]any{
		"running": 1, "allowed_nodes": []string{"pve2"}, "local_resources": []string{}})
	f.on("POST /nodes/pve1/qemu/100/migrate", "UPID:pve1:0:0:0:qmigrate:100:corral@pve:")

	task, err := f.client().Migrate("web-prod", "")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !task.Valid() {
		t.Error("migration should return a task to follow")
	}
	call := f.lastCall("POST", "/nodes/pve1/qemu/100/migrate")
	if got := call.Form.Get("target"); got != "pve2" {
		t.Errorf("target = %q, want the node PVE said would accept it", got)
	}
	if call.Form.Get("online") != "1" {
		t.Errorf("a running guest should migrate online: %v", call.Form)
	}
}

// PVE tells us why a migration cannot happen. Corral should relay that instead
// of attempting it and reporting a task failure.
func TestMigrateRelaysThePreconditionRefusal(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/status/current", map[string]any{"status": "running", "qmpstatus": "running"})
	f.on("GET /nodes/pve1/qemu/100/migrate", map[string]any{
		"running": 1, "allowed_nodes": []string{}, "local_resources": []string{"hostpci0"}})

	_, err := f.client().Migrate("web-prod", "")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "hostpci0") {
		t.Errorf("refusal %q should name the blocking resource", err)
	}
	if len(f.calls("POST", "/nodes/pve1/qemu/100/migrate")) != 0 {
		t.Error("the migration was attempted anyway")
	}
}

// A container cannot live-migrate; PVE restarts it on the target instead, and
// Corral must ask for that rather than for an online migration PVE would reject.
func TestMigrateContainerRestarts(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/lxc/200/status/current", map[string]any{"status": "running"})
	f.on("POST /nodes/pve1/lxc/200/migrate", "UPID:pve1:0:0:0:vzmigrate:200:corral@pve:")

	if _, err := f.client().Migrate("files-ct", "pve2"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/lxc/200/migrate")
	if call.Form.Get("restart") != "1" {
		t.Errorf("a running container should migrate with restart=1: %v", call.Form)
	}
	if call.Form.Get("online") != "" {
		t.Errorf("online migration is not available for a container: %v", call.Form)
	}
}

func TestCloneIsFullByDefaultAndRefusesCollisions(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu/900/clone", "UPID:pve1:0:0:0:qmclone:900:corral@pve:")

	if _, err := f.client().Clone("debian-template", "new-vm", true); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/qemu/900/clone")
	if call.Form.Get("newid") != "131" {
		t.Errorf("newid = %q, want the cluster's next free id", call.Form.Get("newid"))
	}
	if call.Form.Get("name") != "new-vm" || call.Form.Get("full") != "1" {
		t.Errorf("clone params = %v", call.Form)
	}

	if _, err := f.client().Clone("debian-template", "web-prod", true); err == nil {
		t.Error("cloning onto an existing name should be refused")
	}
}

// ── snapshots ─────────────────────────────────────────────────────

func TestSnapshotAsksForMemoryWhenRunning(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu/100/snapshot", "UPID:pve1:0:0:0:qmsnapshot:100:corral@pve:")

	if _, err := f.client().Snapshot("web-prod", "before-upgrade", true); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/qemu/100/snapshot")
	if call.Form.Get("snapname") != "before-upgrade" {
		t.Errorf("snapname = %q", call.Form.Get("snapname"))
	}
	// vmstate is what makes a running guest's capture consistent rather than
	// crash-consistent.
	if call.Form.Get("vmstate") != "1" {
		t.Errorf("vmstate = %q, want 1 for a running guest", call.Form.Get("vmstate"))
	}
}

func TestSnapshotDoesNotAskForMemoryOnAContainer(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/lxc/200/snapshot", "UPID:pve1:0:0:0:vzsnapshot:200:corral@pve:")

	if _, err := f.client().Snapshot("files-ct", "snap", true); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := f.lastCall("POST", "/nodes/pve1/lxc/200/snapshot").Form.Get("vmstate"); got != "" {
		t.Errorf("vmstate = %q; a container has no memory snapshot", got)
	}
}

func TestListSnapshotsDropsTheCurrentPseudoEntry(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/snapshot", []map[string]any{
		{"name": "before-upgrade", "snaptime": 1750000000, "vmstate": 1, "running": 1},
		{"name": "clean", "snaptime": 1740000000, "vmstate": 0},
		{"name": "current", "description": "You are here!"},
	})

	snaps, err := f.client().ListSnapshots("web-prod")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2 (PVE's synthetic 'current' is not one)", len(snaps))
	}
	if !snaps[0].WithMemory() {
		t.Error("the memory-inclusive snapshot lost its vmstate")
	}
	if got := snaps[0].Created(); !strings.HasPrefix(got, "2025-") && !strings.HasPrefix(got, "2026-") {
		t.Errorf("created = %q, want an RFC3339 timestamp", got)
	}
}

func TestSnapshotNameValidation(t *testing.T) {
	for _, bad := range []string{"", "current", "has space", "has/slash", "_leading", strings.Repeat("x", 41)} {
		if err := validSnapshotName(bad); err == nil {
			t.Errorf("validSnapshotName(%q) should fail", bad)
		}
	}
	for _, good := range []string{"snap", "before-upgrade", "v1_2", "a"} {
		if err := validSnapshotName(good); err != nil {
			t.Errorf("validSnapshotName(%q) = %v", good, err)
		}
	}
}

// ── tasks ─────────────────────────────────────────────────────────

func TestWaitTaskSucceeds(t *testing.T) {
	f := newFakePVE(t)
	upid := "UPID:pve1:0:0:0:qmstart:100:corral@pve:"
	calls := 0
	f.onFunc("GET /nodes/pve1/tasks/"+upid+"/status", func(*http.Request) (any, int) {
		calls++
		if calls < 2 {
			return map[string]any{"status": "running", "type": "qmstart"}, http.StatusOK
		}
		return map[string]any{"status": "stopped", "exitstatus": "OK", "type": "qmstart"}, http.StatusOK
	})

	if err := f.client().WaitTask(Task{UPID: upid, Node: "pve1"}, 5*time.Second); err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if calls < 2 {
		t.Errorf("WaitTask polled %d times, want it to have waited", calls)
	}
}

// A failed task must carry its log tail: "task failed" without the reason is the
// least useful error a backend can produce.
func TestWaitTaskReportsTheReason(t *testing.T) {
	f := newFakePVE(t)
	upid := "UPID:pve1:0:0:0:qmigrate:100:corral@pve:"
	f.on("GET /nodes/pve1/tasks/"+upid+"/status", map[string]any{
		"status": "stopped", "exitstatus": "migration aborted", "type": "qmigrate"})
	f.on("GET /nodes/pve1/tasks/"+upid+"/log", []map[string]any{
		{"n": 1, "t": "starting migration"},
		{"n": 2, "t": "ERROR: local disk 'scsi1' cannot be migrated"},
	})

	err := f.client().WaitTask(Task{UPID: upid, Node: "pve1"}, time.Second)
	if err == nil {
		t.Fatal("expected the task failure")
	}
	if !strings.Contains(err.Error(), "cannot be migrated") {
		t.Errorf("error %q should carry the task log tail", err)
	}
}

func TestWaitTaskOnASynchronousResponseIsANoOp(t *testing.T) {
	f := newFakePVE(t)
	if err := f.client().WaitTask(Task{}, time.Second); err != nil {
		t.Errorf("waiting on a non-task = %v, want nil", err)
	}
}

func TestWaitTaskTimesOutWithoutLosingTheUPID(t *testing.T) {
	f := newFakePVE(t)
	upid := "UPID:pve1:0:0:0:vzdump:100:corral@pve:"
	f.on("GET /nodes/pve1/tasks/"+upid+"/status", map[string]any{"status": "running", "type": "vzdump"})

	err := f.client().WaitTask(Task{UPID: upid, Node: "pve1"}, 900*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), upid) {
		t.Errorf("timeout %q should name the task so it can be followed", err)
	}
}

// ── metrics, address, backup, events ──────────────────────────────

func TestMetricsAndCPUHistory(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/status/current", map[string]any{
		"status": "running", "cpus": 4, "cpu": 0.25, "mem": 4294967296, "qmpstatus": "running", "agent": 1})
	f.on("GET /nodes/pve1/qemu/100/rrddata", []map[string]any{
		{"time": 1750000000, "cpu": 0.1},
		{"time": 1750000060, "cpu": 0.5},
	})

	client := f.client()
	metrics, err := client.Metrics("web-prod")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// 25% of four cores is one core: 1000 millicores, in the shape the web UI's
	// live-usage row already formats.
	if metrics["cpu"] != "1000m" {
		t.Errorf("cpu = %q, want 1000m", metrics["cpu"])
	}
	if metrics["mem"] != "4Gi" {
		t.Errorf("mem = %q, want 4Gi", metrics["mem"])
	}

	history, err := client.CPUHistory("web-prod")
	if err != nil {
		t.Fatalf("CPUHistory: %v", err)
	}
	if len(history) != 2 || history[1].CPU != 50 {
		t.Errorf("history = %+v, want percentages", history)
	}
}

func TestAddressFromTheGuestAgent(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/agent/network-get-interfaces", map[string]any{
		"result": []map[string]any{
			{"name": "lo", "ip-addresses": []map[string]any{
				{"ip-address-type": "ipv4", "ip-address": "127.0.0.1"}}},
			{"name": "eth0", "ip-addresses": []map[string]any{
				{"ip-address-type": "ipv6", "ip-address": "fe80::1"},
				{"ip-address-type": "ipv4", "ip-address": "10.0.0.5"}}},
		}})

	got, err := f.client().Address("web-prod")
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if got != "10.0.0.5" {
		t.Errorf("address = %q, want 10.0.0.5 (not loopback, not IPv6)", got)
	}
}

// No guest agent is a normal state, not a listing failure.
func TestAddressWithoutAnAgentIsEmptyNotAnError(t *testing.T) {
	f := newFakePVE(t)
	f.fail("GET /nodes/pve1/qemu/100/agent/network-get-interfaces", http.StatusInternalServerError,
		"QEMU guest agent is not running")

	got, err := f.client().Address("web-prod")
	if err != nil {
		t.Errorf("Address without an agent = %v, want no error", err)
	}
	if got != "" {
		t.Errorf("address = %q, want empty", got)
	}
}

func TestAddressOfAContainer(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/lxc/200/interfaces", []map[string]any{
		{"name": "lo", "inet": "127.0.0.1/8"},
		{"name": "eth0", "inet": "10.0.0.9/24"},
	})
	got, err := f.client().Address("files-ct")
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if got != "10.0.0.9" {
		t.Errorf("container address = %q", got)
	}
}

func TestBackupRunsVzdumpInSnapshotMode(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/vzdump", "UPID:pve1:0:0:0:vzdump:100:corral@pve:")

	if _, err := f.client().Backup("web-prod", "backup-nfs", ""); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/vzdump")
	if call.Form.Get("vmid") != "100" || call.Form.Get("storage") != "backup-nfs" {
		t.Errorf("vzdump params = %v", call.Form)
	}
	// snapshot mode is the only one that costs no downtime.
	if call.Form.Get("mode") != "snapshot" {
		t.Errorf("mode = %q, want snapshot", call.Form.Get("mode"))
	}
}

func TestEventsAreTheGuestsTasks(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/tasks", []map[string]any{
		{"upid": "UPID:pve1:1", "type": "qmstart", "status": "stopped", "exitstatus": "OK", "user": "root@pam"},
		{"upid": "UPID:pve1:2", "type": "qmigrate", "status": "stopped", "exitstatus": "aborted", "user": "root@pam"},
	})

	events, err := f.client().Events("web-prod")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if !events[1].Failed() {
		t.Error("an aborted task should read as failed")
	}
	if events[0].Failed() {
		t.Error("an OK task should not read as failed")
	}
	// Filtered to this guest, or the view is the node's whole history.
	if got := f.lastCall("GET", "/nodes/pve1/tasks").Query.Get("vmid"); got != "100" {
		t.Errorf("tasks query vmid = %q, want 100", got)
	}
}

// ── create ────────────────────────────────────────────────────────

func TestCreateVM(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu", "UPID:pve1:0:0:0:qmcreate:131:corral@pve:")

	_, err := f.client().Create(CreateOpts{
		Name: "new-vm", Cores: 4, Mem: "8Gi", Disk: "40G", Storage: "local-lvm",
		ISO: "local:iso/debian-12.iso", Password: "seed", User: "debian", Tags: []string{"dev"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/qemu")
	want := map[string]string{
		"vmid": "131", "name": "new-vm", "cores": "4", "memory": "8192",
		"scsi0": "local-lvm:40", "ide2": "local:iso/debian-12.iso,media=cdrom",
		"cipassword": "seed", "ciuser": "debian", "tags": "dev", "agent": "1",
	}
	for key, value := range want {
		if got := call.Form.Get(key); got != value {
			t.Errorf("create param %s = %q, want %q", key, got, value)
		}
	}
	if !strings.Contains(call.Form.Get("boot"), "ide2") {
		t.Errorf("an ISO install must boot from the CD: boot=%q", call.Form.Get("boot"))
	}
}

func TestCreateContainerIsUnprivilegedByDefault(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/lxc", "UPID:pve1:0:0:0:vzcreate:131:corral@pve:")

	_, err := f.client().Create(CreateOpts{
		Name: "new-ct", Container: true, Cores: 1, Mem: "512Mi", Disk: "8G",
		Template: "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := f.lastCall("POST", "/nodes/pve1/lxc")
	if got := call.Form.Get("unprivileged"); got != "1" {
		t.Errorf("unprivileged = %q, want 1 — PVE's own default and the safer one", got)
	}
	if got := call.Form.Get("hostname"); got != "new-ct" {
		t.Errorf("hostname = %q", got)
	}
	if got := call.Form.Get("ostemplate"); !strings.HasPrefix(got, "local:vztmpl/") {
		t.Errorf("ostemplate = %q", got)
	}
}

func TestCreateRefusesWhatCannotWork(t *testing.T) {
	f := newFakePVE(t)
	client := f.client()

	if _, err := client.Create(CreateOpts{}); err == nil {
		t.Error("a create with no name should be refused")
	}
	if _, err := client.Create(CreateOpts{Name: "web-prod"}); err == nil {
		t.Error("a create onto an existing name should be refused")
	}
	_, err := client.Create(CreateOpts{Name: "new-ct", Container: true})
	if err == nil || !strings.Contains(err.Error(), "ostemplate") {
		t.Errorf("a container with no template = %v, want a refusal naming ostemplate", err)
	}
}

// ── errors ────────────────────────────────────────────────────────

func TestAPIErrorsCarryTheReason(t *testing.T) {
	f := newFakePVE(t)
	f.onFunc("POST /nodes/pve1/qemu/100/status/start", func(*http.Request) (any, int) {
		return map[string]any{
			"message": "Parameter verification failed.",
			"errors":  map[string]string{"cores": "value must be an integer"},
		}, http.StatusBadRequest
	})

	_, err := f.client().Start("web-prod")
	if err == nil {
		t.Fatal("expected an error")
	}
	// PVE's per-parameter errors are the useful ones.
	if !strings.Contains(err.Error(), "cores: value must be an integer") {
		t.Errorf("error %q should carry PVE's parameter error", err)
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error %T is not an *APIError; callers cannot tell a refusal from a failure", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d", apiErr.Status)
	}
	if apiErr.Unauthorized() {
		t.Error("a 400 is not an authorization problem")
	}
}

func TestUnauthorizedIsDistinguishable(t *testing.T) {
	f := newFakePVE(t)
	f.fail("GET /cluster/resources", http.StatusUnauthorized, "authentication failure")

	_, err := f.client().List()
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if !apiErr.Unauthorized() {
		t.Error("a 401 should read as an authorization problem so the doctor can say so")
	}
}

func TestVersionProvesReachabilityAndAuth(t *testing.T) {
	f := newFakePVE(t)
	got, err := f.client().Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.HasPrefix(got, "8.3.0") {
		t.Errorf("version = %q", got)
	}
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if apiErr, ok := err.(*APIError); ok {
			*target = apiErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// ── types.Backend ─────────────────────────────────────────────────

func TestBackendSatisfiesTheInterface(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu/100/status/start", "UPID:pve1:0:0:0:qmstart:100:corral@pve:")
	f.on("POST /nodes/pve1/qemu/100/status/shutdown", "UPID:pve1:0:0:0:qmshutdown:100:corral@pve:")

	backend := Backend{Client: f.client()}
	if vms, err := backend.ListVMs(); err != nil || len(vms) == 0 {
		t.Fatalf("ListVMs = %v, %v", vms, err)
	}
	if !backend.VMExists("web-prod") {
		t.Error("VMExists should find web-prod")
	}
	if err := backend.StartVM("web-prod"); err != nil {
		t.Errorf("StartVM: %v", err)
	}
	if err := backend.StopVM("web-prod"); err != nil {
		t.Errorf("StopVM: %v", err)
	}
	// Viewer has no local counterpart and must say so rather than failing
	// obscurely or pretending to work.
	err := backend.Viewer("web-prod")
	if err == nil {
		t.Fatal("Viewer should refuse")
	}
	if !strings.Contains(err.Error(), "corral web") {
		t.Errorf("Viewer refusal %q should point at the console that does work", err)
	}
}

func TestVMInfoRendersTheUsefulFields(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/status/current", map[string]any{
		"status": "running", "cpus": 4, "qmpstatus": "running", "agent": 1, "lock": "backup"})
	f.on("GET /nodes/pve1/qemu/100/config", map[string]any{
		"name": "web-prod", "cores": 4, "memory": 8192, "tags": "prod,web", "hotplug": "disk,network,memory"})
	f.on("GET /nodes/pve1/qemu/100/agent/network-get-interfaces", map[string]any{
		"result": []map[string]any{{"name": "eth0", "ip-addresses": []map[string]any{
			{"ip-address-type": "ipv4", "ip-address": "10.0.0.5"}}}}})

	out, err := Backend{Client: f.client()}.VMInfo("web-prod")
	if err != nil {
		t.Fatalf("VMInfo: %v", err)
	}
	for _, want := range []string{"running", "10.0.0.5", "prod,web", "backup", "8192"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("VMInfo output missing %q:\n%s", want, out)
		}
	}
}

func TestGuestConfigReportsHotplug(t *testing.T) {
	f := newFakePVE(t)
	f.on("GET /nodes/pve1/qemu/100/config", map[string]any{
		"cores": 4, "memory": 8192, "hotplug": "disk,network,usb,memory,cpu"})

	cfg, err := f.client().GuestConfig("web-prod")
	if err != nil {
		t.Fatalf("GuestConfig: %v", err)
	}
	// Whether a scale applies live or needs a reboot is something PVE knows and
	// Corral should not guess.
	if !cfg.HotplugsMemory() || !cfg.HotplugsCPU() {
		t.Errorf("hotplug %q read as memory=%v cpu=%v", cfg.Hotplug, cfg.HotplugsMemory(), cfg.HotplugsCPU())
	}
	if cfg.Cores != 4 || cfg.Memory != 8192 {
		t.Errorf("config = %d cores / %d MiB", cfg.Cores, cfg.Memory)
	}
}

// ── consoles ──────────────────────────────────────────────────────

func TestConsoleTicketsCarryEnoughToOpenTheWebsocket(t *testing.T) {
	f := newFakePVE(t)
	f.on("POST /nodes/pve1/qemu/100/vncproxy", map[string]any{
		"ticket": "PVEVNC:abc+def/ghi", "port": "5900", "user": "corral@pve"})
	f.on("POST /nodes/pve1/qemu/100/termproxy", map[string]any{
		"ticket": "PVEVNC:xyz", "port": "5901", "user": "corral@pve"})

	client := f.client()
	vnc, err := client.VNCTicket("web-prod")
	if err != nil {
		t.Fatalf("VNCTicket: %v", err)
	}
	if vnc.Port != "5900" || vnc.Node != "pve1" || vnc.VMID != 100 || vnc.Kind != KindQemu {
		t.Errorf("ticket = %+v", vnc)
	}
	path := vnc.WebsocketPath()
	if !strings.Contains(path, "/nodes/pve1/qemu/100/vncwebsocket") || !strings.Contains(path, "port=5900") {
		t.Errorf("websocket path = %q", path)
	}
	// The ticket contains characters that must not be pasted into a URL raw.
	if strings.Contains(path, "abc+def/ghi") {
		t.Errorf("websocket path carries an unescaped ticket: %q", path)
	}
	term, err := client.TermTicket("web-prod")
	if err != nil {
		t.Fatalf("TermTicket: %v", err)
	}
	if !term.Serial {
		t.Error("a termproxy ticket is the serial console")
	}
}
