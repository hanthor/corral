package web

// GET /metrics — the fleet as one Prometheus series set (ADR-0011).
//
// The value here is not "metrics about VMs"; every backend already has those.
// It is the fleet labelled by the axes Corral knows and the backends do not —
// which backend and context an instance is on, which *pool* an operator put it
// in, and whether Corral's own view of the world is healthy. `pool` is the
// load-bearing label: it is the only one that spans backends, and it is what
// makes `sum by (pool) (corral_instance_running)` mean "is my application stack
// up" across a KubeVirt cluster and a Proxmox host at once.
//
// A scrape never fans out to the backends. Inventory is assembled by running
// kubectl, virsh, incus, systemctl and HTTPS calls concurrently — fine at a
// human's click rate, actively bad at a scraper's, and worse with two
// Prometheis. So a background collector refreshes a snapshot on a timer and the
// handler renders whatever the last one holds. The cost of that is staleness,
// so the staleness is a metric: without corral_collection_age_seconds a frozen
// collector is indistinguishable from a stable fleet.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tuna-os/corral/pkg/config"

	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/fleet"
	"github.com/tuna-os/corral/pkg/promtext"
	"github.com/tuna-os/corral/pkg/types"
)

// Collection intervals. Doctor is slower on purpose: its checks shell out far
// more heavily than a listing and change far less often.
const (
	metricsInterval = 30 * time.Second
	doctorInterval  = 5 * time.Minute
)

// snapshot is everything /metrics renders. It is replaced wholesale rather than
// mutated, so a scrape either sees the previous collection or the next one and
// never a half-written mixture of the two.
type metricsSnapshot struct {
	taken    time.Time
	duration time.Duration
	ok       bool

	vms    []types.VM
	errors map[string]string
	// pools maps an instance ref to its folder path. Instances in no pool are
	// absent here and are labelled pool="" — dropping them would make the pool
	// sums silently understate the fleet.
	pools  map[string]string
	checks []doctor.Check
	tasks  []TaskEntry
}

var metrics = struct {
	mu   sync.RWMutex
	snap *metricsSnapshot
}{}

// Seams, so a test can drive a collection without a cluster.
var (
	metricsFleet  = fleet.List
	metricsDoctor = doctor.Run
)

// StartMetrics begins background collection. Off unless `corral web --metrics`
// is passed: a laptop's web UI should not poll five backends forever for a
// scraper that does not exist.
func StartMetrics(ctx context.Context) {
	collectMetrics(ctx) // seed, so the first scrape is not an empty body
	go func() {
		inventory := time.NewTicker(metricsInterval)
		checks := time.NewTicker(doctorInterval)
		defer inventory.Stop()
		defer checks.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-inventory.C:
				collectMetrics(ctx)
			case <-checks.C:
				collectChecks()
			}
		}
	}()
}

// collectMetrics takes one snapshot. It keeps the previous doctor checks: they
// are on their own timer, and dropping them between doctor runs would make
// corral_check flap in and out of existence.
func collectMetrics(ctx context.Context) {
	started := time.Now()
	result := metricsFleet(ctx)
	result.VMs = append(result.VMs, peerVMs()...)

	pools := map[string]string{}
	if tree, err := folderStore.Tree(); err == nil {
		pools = tree.PathsByRef()
	}

	metrics.mu.Lock()
	previous := metrics.snap
	next := &metricsSnapshot{
		taken:    started,
		duration: time.Since(started),
		// A collection that produced no instances *and* no per-context errors
		// means the fan-out itself did not work. An empty fleet with a healthy
		// listing is a legitimate zero and stays successful.
		ok:     len(result.VMs) > 0 || len(result.Errors) > 0 || len(contextNames()) == 0,
		vms:    result.VMs,
		errors: result.Errors,
		pools:  pools,
		tasks:  recentTasks(),
	}
	if previous != nil {
		next.checks = previous.checks
	}
	metrics.snap = next
	metrics.mu.Unlock()

	if previous == nil {
		collectChecks() // first run: populate the checks rather than wait 5m
	}
}

func collectChecks() {
	checks := metricsDoctor()
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.snap != nil {
		metrics.snap.checks = checks
	}
}

// handleMetricsExposition renders the current snapshot.
func handleMetricsExposition(w http.ResponseWriter, _ *http.Request) {
	metrics.mu.RLock()
	snap := metrics.snap
	metrics.mu.RUnlock()

	w.Header().Set("Content-Type", promtext.ContentType)
	if snap == nil {
		// Collection is off or has not run. Say so as a metric rather than
		// returning an error: a scraper that gets a 503 records nothing, and
		// "corral is up but not collecting" is the thing worth alerting on.
		out := promtext.New()
		out.Metric("corral_collection_success", "gauge",
			"1 when the last fleet collection succeeded; 0 when it failed or has not run")
		out.Sample(nil, 0)
		_, _ = w.Write([]byte(out.String()))
		return
	}
	_, _ = w.Write([]byte(renderMetrics(snap, time.Now())))
}

// renderMetrics is the whole exposition, pure over a snapshot so it is testable
// without a server, a cluster, or a clock.
func renderMetrics(snap *metricsSnapshot, now time.Time) string {
	out := promtext.New()

	// ── collection health ────────────────────────────────────────
	out.Metric("corral_collection_age_seconds", "gauge",
		"Seconds since the fleet snapshot these metrics are rendered from was taken")
	out.Sample(nil, now.Sub(snap.taken).Seconds())

	out.Metric("corral_collection_duration_seconds", "gauge",
		"How long the last fleet collection took")
	out.Sample(nil, snap.duration.Seconds())

	out.Metric("corral_collection_success", "gauge",
		"1 when the last fleet collection succeeded; 0 when it failed or has not run")
	out.Bool(nil, snap.ok)

	// ── backend reachability ─────────────────────────────────────
	//
	// An unreachable context must not look like a context with nothing in it.
	// That distinction is invisible in the instance counts and is exactly the
	// alert an operator wants.
	seen := map[string]string{} // context -> backend
	for _, vm := range snap.vms {
		seen[vm.Context] = vm.Backend
	}
	for _, name := range contextNames() {
		if _, ok := seen[name]; !ok {
			seen[name] = backendOfContext(name)
		}
	}
	// A context that errored may be absent from both lists — it returned no
	// instances, and a peer or a since-removed context is not in the local
	// config. Seeding from the error map is what keeps "unreachable" from
	// rendering as "not there".
	for name := range snap.errors {
		if _, ok := seen[name]; !ok {
			seen[name] = backendOfContext(name)
		}
	}
	out.Metric("corral_backend_up", "gauge",
		"1 when the last collection reached this context")
	for _, c := range sortedKeys(seen) {
		_, failed := snap.errors[c]
		out.Bool(map[string]string{"context": c, "backend": seen[c]}, !failed)
	}

	out.Metric("corral_backend_error", "gauge",
		"1 when the last collection of this context returned an error")
	for _, c := range sortedKeys(seen) {
		_, failed := snap.errors[c]
		out.Bool(map[string]string{"context": c, "backend": seen[c]}, failed)
	}

	// ── per-instance ─────────────────────────────────────────────
	poolOf := func(vm types.VM) string { return snap.pools[vm.Ref().String()] }
	base := func(vm types.VM) map[string]string {
		return map[string]string{
			"name": vm.Name, "backend": vm.Backend,
			"context": vm.Context, "namespace": vm.Namespace,
			"pool": poolOf(vm),
		}
	}

	out.Metric("corral_instance_info", "gauge",
		"Static metadata about an instance; the value is always 1")
	for _, vm := range snap.vms {
		labels := base(vm)
		labels["node"] = vm.Node
		labels["template"] = boolLabel(vm.IsTemplate)
		labels["bootc"] = boolLabel(vm.Bootc)
		out.Sample(labels, 1)
	}

	out.Metric("corral_instance_running", "gauge", "1 when the instance is running")
	for _, vm := range snap.vms {
		out.Bool(base(vm), vm.Running)
	}

	out.Metric("corral_instance_ready", "gauge",
		"1 when the instance is running and its readiness signal is good")
	for _, vm := range snap.vms {
		out.Bool(base(vm), vm.Ready)
	}

	out.Metric("corral_instance_cpu_cores", "gauge", "Configured vCPUs")
	for _, vm := range snap.vms {
		out.Sample(base(vm), float64(vm.CPU))
	}

	out.Metric("corral_instance_memory_bytes", "gauge", "Configured memory")
	for _, vm := range snap.vms {
		if bytes, err := parseQuantity(vm.Mem); err == nil {
			out.Sample(base(vm), float64(bytes))
		}
	}

	// ── fleet and pool aggregates ────────────────────────────────
	counts := map[[3]string]int{}
	for _, vm := range snap.vms {
		counts[[3]string{vm.Backend, vm.Context, stateOf(vm)}]++
	}
	out.Metric("corral_instances", "gauge",
		"Instances by backend, context, and state")
	for _, k := range sortedTupleKeys(counts) {
		out.Sample(map[string]string{"backend": k[0], "context": k[1], "state": k[2]}, float64(counts[k]))
	}

	poolTotal := map[string]int{}
	poolRunning := map[string]int{}
	for _, vm := range snap.vms {
		p := poolOf(vm)
		poolTotal[p]++
		if vm.Running {
			poolRunning[p]++
		}
	}
	out.Metric("corral_pool_instances", "gauge",
		"Instances in a pool; pool=\"\" is everything ungrouped")
	for _, name := range sortedKeys(poolTotal) {
		out.Sample(map[string]string{"pool": name}, float64(poolTotal[name]))
	}
	out.Metric("corral_pool_running", "gauge", "Running instances in a pool")
	for _, name := range sortedKeys(poolTotal) {
		out.Sample(map[string]string{"pool": name}, float64(poolRunning[name]))
	}

	// ── doctor ───────────────────────────────────────────────────
	out.Metric("corral_check", "gauge",
		"1 when a doctor check passes; severity distinguishes required from advisory")
	for _, c := range snap.checks {
		out.Bool(map[string]string{
			"name": c.Name, "backend": c.Backend,
			"context": c.Context, "severity": c.Severity,
		}, c.OK)
	}

	// ── tasks ────────────────────────────────────────────────────
	//
	// A gauge, not a counter, and named in the HELP as what it is: the task log
	// is a bounded ring that drops its oldest entries, so this can decrease. A
	// counter that silently decreases breaks rate() in a way that is very hard
	// to debug from the graph.
	taskCounts := map[[3]string]int{}
	for _, t := range snap.tasks {
		taskCounts[[3]string{t.Action, t.Status, ""}]++
	}
	out.Metric("corral_tasks", "gauge",
		"Tasks in the server's recent-activity ring by action and status; "+
			"a bounded window, not a monotonic total")
	for _, k := range sortedTupleKeys(taskCounts) {
		out.Sample(map[string]string{"action": k[0], "status": k[1]}, float64(taskCounts[k]))
	}

	return out.String()
}

// stateOf flattens the fleet's status strings into the three states worth
// aggregating on. The raw Status carries things like "↓ 42.5%" for an ISO
// download, which would be terrible cardinality as a label value.
func stateOf(vm types.VM) string {
	switch {
	case vm.Ready:
		return "ready"
	case vm.Running:
		return "running"
	default:
		return "stopped"
	}
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ── deterministic iteration ───────────────────────────────────────
//
// Map order is random in Go, and a metrics body whose lines reorder every
// scrape is unreadable in a diff and impossible to golden-test. Sorting here
// costs nothing at fleet scale and makes two scrapes comparable by eye.

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedTupleKeys[V any](m map[[3]string]V) [][3]string {
	keys := make([][3]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		for n := 0; n < 3; n++ {
			if keys[i][n] != keys[j][n] {
				return keys[i][n] < keys[j][n]
			}
		}
		return false
	})
	return keys
}

// recentTasks copies the task ring under its own lock.
func recentTasks() []TaskEntry {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	out := make([]TaskEntry, 0, len(activity.entries))
	for _, e := range activity.entries {
		out = append(out, *e)
	}
	return out
}

// contextNames lists every configured context, so a context that returned
// nothing at all still gets a corral_backend_up series. Without this, a context
// that is down and empty simply vanishes from the metrics.
func contextNames() []string {
	out := []string{}
	for _, c := range config.Contexts() {
		out = append(out, c.Name)
	}
	return out
}

func backendOfContext(name string) string {
	if c, ok := config.FindContext(name); ok {
		return c.Backend
	}
	return ""
}

// parseQuantity reads the memory strings the backends report ("4Gi", "2048Mi",
// "8G"). An unparseable one is skipped rather than reported as zero: a VM with
// 0 bytes of RAM would be a lie, and an absent series is honest.
func parseQuantity(s string) (int64, error) {
	value := strings.TrimSpace(s)
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	upper := strings.ToUpper(value)
	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(upper, "TIB"), strings.HasSuffix(upper, "TI"), strings.HasSuffix(upper, "T"):
		multiplier = 1 << 40
	case strings.HasSuffix(upper, "GIB"), strings.HasSuffix(upper, "GI"), strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
	case strings.HasSuffix(upper, "MIB"), strings.HasSuffix(upper, "MI"), strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
	case strings.HasSuffix(upper, "KIB"), strings.HasSuffix(upper, "KI"), strings.HasSuffix(upper, "K"):
		multiplier = 1 << 10
	}
	digits := strings.TrimRight(upper, "KMGTIB")
	var n int64
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a memory size", s)
	}
	return n * multiplier, nil
}

// MetricsEnabled turns the background collector on. It is off by default
// because collection is real work — a full fan-out to every configured context
// every 30 seconds — and a laptop's web UI should not do it for a scraper that
// does not exist.
var MetricsEnabled bool
