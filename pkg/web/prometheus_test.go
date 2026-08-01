package web

// /metrics tests (ADR-0011).
//
// The assertions that matter are the ones about honesty: that an unreachable
// context is a series rather than an absence, that a stale snapshot says how
// stale it is, and that an instance in no pool is counted rather than dropped.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/folder"
	"github.com/tuna-os/corral/pkg/types"
)

func sampleSnapshot() *metricsSnapshot {
	taken := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return &metricsSnapshot{
		taken:    taken,
		duration: 1500 * time.Millisecond,
		ok:       true,
		vms: []types.VM{
			{Name: "web-1", Backend: "kubevirt", Context: "prod", Namespace: "default",
				Node: "n1", CPU: 4, Mem: "8Gi", Running: true, Ready: true},
			{Name: "web-2", Backend: "proxmox", Context: "pve", CPU: 2, Mem: "4Gi", Running: true},
			{Name: "scratch", Backend: "qemu", CPU: 1, Mem: "1Gi"},
		},
		errors: map[string]string{"lab": "dial tcp: connection refused"},
		pools: map[string]string{
			"kubevirt/prod/default/web-1": "prod/web",
			"proxmox/pve//web-2":          "prod/web",
		},
		checks: []doctor.Check{
			{Name: "KubeVirt installed", OK: true, Severity: "required", Backend: "kubevirt"},
			{Name: "CDI installed", OK: false, Severity: "warning", Backend: "kubevirt"},
		},
		tasks: []TaskEntry{
			{Action: "start", Status: "ok"},
			{Action: "start", Status: "ok"},
			{Action: "move", Status: "error"},
		},
	}
}

// render exercises the exposition with the context resolver stubbed to the
// identity, so a fixture's labels are the fixture's own and not whatever
// contexts happen to be configured on the machine running the test.
// TestContextLabelJoinsInstancesToTheirContext covers the real resolver.
func render(t *testing.T, snap *metricsSnapshot, at time.Time) string {
	t.Helper()
	previous := contextLabel
	contextLabel = func(_, contextName string) string { return contextName }
	t.Cleanup(func() { contextLabel = previous })
	return renderMetrics(snap, at)
}

func hasLine(body, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func mustLine(t *testing.T, body, want string) {
	t.Helper()
	if !hasLine(body, want) {
		t.Errorf("missing line:\n  %s\nin body:\n%s", want, body)
	}
}

func TestMetricsReportsCollectionAge(t *testing.T) {
	snap := sampleSnapshot()
	body := render(t, snap, snap.taken.Add(45*time.Second))
	mustLine(t, body, "corral_collection_age_seconds 45")
	mustLine(t, body, "corral_collection_duration_seconds 1.5")
	mustLine(t, body, "corral_collection_success 1")
}

// Without the age metric a frozen collector is indistinguishable from a stable
// fleet — every series stays green while the world moves on.
func TestMetricsAgeGrowsWithAStaleSnapshot(t *testing.T) {
	snap := sampleSnapshot()
	body := render(t, snap, snap.taken.Add(2*time.Hour))
	mustLine(t, body, "corral_collection_age_seconds 7200")
}

func TestMetricsReportsAnUnreachableContextAsDownNotAsEmpty(t *testing.T) {
	snap := sampleSnapshot()
	body := render(t, snap, snap.taken)

	if !strings.Contains(body, `corral_backend_up{backend="",context="lab"} 0`) {
		t.Errorf("a context that errored must be reported down, not omitted:\n%s", body)
	}
	if !strings.Contains(body, `corral_backend_error{backend="",context="lab"} 1`) {
		t.Errorf("and its error must be its own series:\n%s", body)
	}
	if !strings.Contains(body, `corral_backend_up{backend="kubevirt",context="prod"} 1`) {
		t.Errorf("a healthy context should be up:\n%s", body)
	}
}

func TestMetricsLabelsInstancesWithTheirPool(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())

	// The point of the whole exercise: two instances on different backends
	// carrying the same pool label, so one query spans both.
	for _, want := range []string{
		`corral_instance_running{backend="kubevirt",context="prod",name="web-1",namespace="default",pool="prod/web"} 1`,
		`corral_instance_running{backend="proxmox",context="pve",name="web-2",namespace="",pool="prod/web"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\nin:\n%s", want, body)
		}
	}
}

// An instance in no pool gets pool="" rather than being dropped. Dropping it
// would make the pool sums silently understate the fleet, which is the worst
// kind of wrong number: one that looks plausible.
func TestMetricsCountsUnpooledInstances(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())
	mustLine(t, body, `corral_pool_instances{pool=""} 1`)
	mustLine(t, body, `corral_pool_instances{pool="prod/web"} 2`)
	mustLine(t, body, `corral_pool_running{pool="prod/web"} 2`)
	mustLine(t, body, `corral_pool_running{pool=""} 0`)
}

func TestMetricsAggregatesByBackendContextAndState(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())
	mustLine(t, body, `corral_instances{backend="kubevirt",context="prod",state="ready"} 1`)
	mustLine(t, body, `corral_instances{backend="proxmox",context="pve",state="running"} 1`)
	mustLine(t, body, `corral_instances{backend="qemu",context="",state="stopped"} 1`)
}

func TestMetricsEmitsInstanceMetadataAndSizing(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())
	if !strings.Contains(body, `corral_instance_info{backend="kubevirt",bootc="false",context="prod",name="web-1",namespace="default",node="n1",pool="prod/web",template="false"} 1`) {
		t.Errorf("instance info line is wrong:\n%s", body)
	}
	mustLine(t, body, `corral_instance_cpu_cores{backend="qemu",context="",name="scratch",namespace="",pool=""} 1`)
	// 8Gi
	if !strings.Contains(body, `corral_instance_memory_bytes{backend="kubevirt",context="prod",name="web-1",namespace="default",pool="prod/web"} 8589934592`) {
		t.Errorf("memory should be converted to bytes:\n%s", body)
	}
}

// A VM whose memory string cannot be parsed is skipped rather than reported as
// zero: a VM with 0 bytes of RAM is a lie, an absent series is honest.
func TestMetricsSkipsUnparseableMemoryRatherThanReportingZero(t *testing.T) {
	snap := sampleSnapshot()
	snap.vms = []types.VM{{Name: "odd", Backend: "qemu", Mem: "some"}}
	body := render(t, snap, time.Now())
	if strings.Contains(body, "corral_instance_memory_bytes{") {
		t.Errorf("an unparseable memory string should produce no series:\n%s", body)
	}
	// The instance still exists in every other metric.
	if !strings.Contains(body, `corral_instance_running{backend="qemu",context="",name="odd",namespace="",pool=""} 0`) {
		t.Errorf("the instance itself should still be reported:\n%s", body)
	}
}

func TestMetricsExposesDoctorChecksWithSeverity(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())
	mustLine(t, body, `corral_check{backend="kubevirt",context="",name="KubeVirt installed",severity="required"} 1`)
	mustLine(t, body, `corral_check{backend="kubevirt",context="",name="CDI installed",severity="warning"} 0`)
}

func TestMetricsCountsTasksByActionAndStatus(t *testing.T) {
	body := render(t, sampleSnapshot(), time.Now())
	mustLine(t, body, `corral_tasks{action="start",status="ok"} 2`)
	mustLine(t, body, `corral_tasks{action="move",status="error"} 1`)
	// The ring drops old entries, so this must not claim to be a counter.
	if !strings.Contains(body, "# TYPE corral_tasks gauge") {
		t.Error("corral_tasks must be a gauge — the task ring is a bounded window")
	}
}

func TestMetricsBodyIsStableAcrossRenders(t *testing.T) {
	snap := sampleSnapshot()
	at := snap.taken.Add(time.Second)
	if render(t, snap, at) != render(t, snap, at) {
		t.Fatal("two renders of one snapshot must be byte-identical")
	}
}

// Label values from a real fleet contain quotes; one unescaped quote makes
// Prometheus reject the entire body, losing every series rather than one.
func TestMetricsEscapesHostileLabelValues(t *testing.T) {
	snap := sampleSnapshot()
	snap.vms = []types.VM{{Name: `weird"vm\name`, Backend: "qemu"}}
	snap.checks = []doctor.Check{{Name: `check "one"`, OK: true}}
	body := render(t, snap, time.Now())

	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "{") || strings.HasPrefix(line, "#") {
			continue
		}
		labels := line[strings.Index(line, "{")+1 : strings.LastIndex(line, "}")]
		// Every quote inside the label block must be either a delimiter or
		// escaped. Counting unescaped ones catches a truncated line.
		if strings.Count(strings.ReplaceAll(labels, `\"`, ""), `"`)%2 != 0 {
			t.Fatalf("unbalanced quotes in label block: %s", line)
		}
	}
	if !strings.Contains(body, `name="weird\"vm\\name"`) {
		t.Errorf("the instance name should be escaped in place:\n%s", body)
	}
}

// ── the endpoint ──────────────────────────────────────────────────

func TestMetricsEndpointReportsFailureWhenCollectionIsOff(t *testing.T) {
	srv := newDemoServer(t)
	previous := metrics.snap
	metrics.snap = nil
	t.Cleanup(func() { metrics.snap = previous })

	body := getText(t, srv, "/metrics")
	if !strings.Contains(body, "corral_collection_success 0") {
		t.Fatalf("a server that is not collecting should say so as a metric, got:\n%s", body)
	}
}

func TestMetricsEndpointServesTheSnapshot(t *testing.T) {
	srv := newDemoServer(t)
	previous := metrics.snap
	metrics.snap = sampleSnapshot()
	t.Cleanup(func() { metrics.snap = previous })

	body := getText(t, srv, "/metrics")
	if !strings.Contains(body, `corral_instance_running{backend="kubevirt"`) {
		t.Fatalf("the endpoint should render the snapshot, got:\n%s", body)
	}
}

// StartMetrics is what wires the seams together; this drives one collection and
// checks the snapshot it produced, including the pool join.
func TestStartMetricsCollectsFleetFoldersAndChecks(t *testing.T) {
	previousFleet, previousDoctor, previousSnap := metricsFleet, metricsDoctor, metrics.snap
	t.Cleanup(func() {
		metricsFleet, metricsDoctor, metrics.snap = previousFleet, previousDoctor, previousSnap
	})

	ref := types.InstanceRef{Backend: "qemu", Name: "one"}
	scratchFolders(t, folder.Folder{Path: "lab", Members: []types.InstanceRef{ref}})
	metricsFleet = func(context.Context) fleet.Result {
		return fleet.Result{
			VMs:    []types.VM{{Name: "one", Backend: "qemu", Running: true, CPU: 1, Mem: "1Gi"}},
			Errors: map[string]string{},
		}
	}
	metricsDoctor = func() []doctor.Check {
		return []doctor.Check{{Name: "kubectl present", OK: true, Severity: "required"}}
	}

	collectMetrics(context.Background())

	body := render(t, metrics.snap, time.Now())
	mustLine(t, body, `corral_instance_running{backend="qemu",context="",name="one",namespace="",pool="lab"} 1`)
	mustLine(t, body, `corral_pool_instances{pool="lab"} 1`)
	if !strings.Contains(body, `corral_check{backend="",context="",name="kubectl present",severity="required"} 1`) {
		t.Errorf("doctor checks should be collected on the first run:\n%s", body)
	}
	mustLine(t, body, "corral_collection_success 1")
}

// A context that failed is reported as a failed context, not as an empty one —
// even when it is absent from the local config, which is the case for a peer or
// a context removed since the last collection.
func TestCollectionReportsAFailedContextThatReturnedNothing(t *testing.T) {
	previousFleet, previousSnap := metricsFleet, metrics.snap
	t.Cleanup(func() { metricsFleet, metrics.snap = previousFleet, previousSnap })

	metricsFleet = func(context.Context) fleet.Result {
		return fleet.Result{Errors: map[string]string{"prod": "context deadline exceeded"}}
	}
	collectMetrics(context.Background())
	if !metrics.snap.ok {
		t.Fatal("a collection with a reported error is still a successful collection — the error is its own series")
	}
	if !strings.Contains(render(t, metrics.snap, time.Now()), `corral_backend_error{backend="",context="prod"} 1`) {
		t.Error("and the failing context must be reported")
	}
}

// getText fetches a non-JSON body — /metrics is the only endpoint that serves
// one, so the helper lives here rather than in the shared test utilities.
func getText(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("GET %s: content type %q — a scraper needs the exposition type", path, ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// The label that used to make the metrics unjoinable: fleet.List keys its
// per-context errors by the config name while the instances it returns carry
// the backend's own context string, so corral_backend_up and
// corral_instance_running described the same cluster under two different
// labels and no query could relate them.
func TestContextLabelJoinsInstancesToTheirContext(t *testing.T) {
	// The demo server configures real contexts; resolve against those.
	newDemoServer(t)
	var kubevirtContext string
	for _, c := range config.Contexts() {
		if c.Backend == "kubevirt" {
			kubevirtContext = c.Name
			if got := resolveContextLabel(c.Backend, c.Context); got != c.Name {
				t.Errorf("resolveContextLabel(%q, %q) = %q, want the config name %q",
					c.Backend, c.Context, got, c.Name)
			}
		}
	}
	if kubevirtContext == "" {
		t.Skip("no kubevirt context configured in this environment")
	}

	// A context nobody configured keeps its raw value rather than vanishing —
	// a peer, or one removed since the last collection, is still real.
	if got := resolveContextLabel("kubevirt", "some-other-cluster"); got != "some-other-cluster" {
		t.Errorf("an unconfigured context should keep its own name, got %q", got)
	}
}

// End to end over one collection: the instance series and the reachability
// series must carry the same context label, or a dashboard cannot join them.
func TestMetricsInstanceAndBackendSeriesShareAContextLabel(t *testing.T) {
	newDemoServer(t)
	previousFleet, previousSnap := metricsFleet, metrics.snap
	t.Cleanup(func() { metricsFleet, metrics.snap = previousFleet, previousSnap })
	scratchFolders(t)

	var configured config.ContextConfig
	for _, c := range config.Contexts() {
		if c.Backend == "kubevirt" {
			configured = c
		}
	}
	if configured.Name == "" {
		t.Skip("no kubevirt context configured in this environment")
	}

	metricsFleet = func(context.Context) fleet.Result {
		return fleet.Result{VMs: []types.VM{{
			Name: "one", Backend: configured.Backend, Context: configured.Context, Running: true,
		}}}
	}
	collectMetrics(context.Background())
	body := renderMetrics(metrics.snap, time.Now())

	want := `context="` + configured.Name + `"`
	var onInstance, onBackend bool
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "corral_instance_running{") && strings.Contains(line, want) {
			onInstance = true
		}
		if strings.HasPrefix(line, "corral_backend_up{") && strings.Contains(line, want) {
			onBackend = true
		}
	}
	if !onInstance || !onBackend {
		t.Fatalf("the two series must agree on %s (instance=%v backend=%v):\n%s",
			want, onInstance, onBackend, body)
	}
}
