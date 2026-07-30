package schedule

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/lifecycle"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func ref(backend, context, namespace, name string) types.InstanceRef {
	return types.InstanceRef{Backend: backend, Context: context, Namespace: namespace, Name: name}
}

// ── cron ──────────────────────────────────────────────────────────

func TestParseCron_Grammar(t *testing.T) {
	for _, expr := range []string{
		"0 9 * * 1-5",
		"*/30 * * * *",
		"0 0,12 * * *",
		"15 2 1 1 0",
		"0 */6 * * *",
		"0 9-17/4 * * *",
	} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q): %v", expr, err)
		}
	}
}

// Non-portable extensions are refused rather than half-supported: a schedule
// that means one thing to Kubernetes and another to Corral's own tick is worse
// than one that won't be created.
func TestParseCron_RejectsWhatItCannotHonour(t *testing.T) {
	for _, expr := range []string{
		"@daily",
		"0 9 * *",           // four fields
		"0 9 * * 1-5 extra", // six
		"60 * * * *",        // minute out of range
		"0 24 * * *",        // hour out of range
		"0 9 * * 8",         // weekday out of range
		"0 17-9 * * *",      // backwards range
		"*/0 * * * *",       // zero step
		"5/2 * * * *",       // step without a range
		"0 0 L * *",         // last-day extension
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) accepted an expression it cannot honour", expr)
		}
	}
}

func TestCron_Matches(t *testing.T) {
	// 2026-07-30 is a Thursday.
	weekdayMorning := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	saturday := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	businessStart, err := ParseCron("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	if !businessStart.Matches(weekdayMorning) {
		t.Error("weekday 09:00 did not match a weekday-morning schedule")
	}
	if businessStart.Matches(saturday) {
		t.Error("Saturday matched a Monday-to-Friday schedule")
	}
	if businessStart.Matches(weekdayMorning.Add(time.Minute)) {
		t.Error("09:01 matched an on-the-hour schedule")
	}
}

// cron(8) ORs day-of-month with day-of-week when both are restricted. Getting
// this backwards silently skips most firings, so it is pinned.
func TestCron_DayFieldsAreOredWhenBothRestricted(t *testing.T) {
	c, err := ParseCron("0 0 1 * 1") // the 1st, or any Monday
	if err != nil {
		t.Fatal(err)
	}
	firstOfMonth := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // a Wednesday
	someMonday := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	neither := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

	if !c.Matches(firstOfMonth) {
		t.Error("the 1st did not match")
	}
	if !c.Matches(someMonday) {
		t.Error("a Monday did not match")
	}
	if c.Matches(neither) {
		t.Error("a day that is neither the 1st nor a Monday matched")
	}
}

// ── validation ────────────────────────────────────────────────────

func TestEntry_Validate(t *testing.T) {
	good := Entry{Ref: ref("qemu", "", "", "dev"), Start: "0 9 * * 1-5"}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid schedule was rejected: %v", err)
	}

	for name, e := range map[string]Entry{
		"no boundaries":   {Ref: ref("qemu", "", "", "dev")},
		"no name":         {Ref: ref("qemu", "", "", ""), Start: "0 9 * * *"},
		"bad cron":        {Ref: ref("qemu", "", "", "dev"), Start: "nope"},
		"unknown backend": {Ref: ref("vmware", "", "", "dev"), Start: "0 9 * * *"},
		"bad stop cron":   {Ref: ref("qemu", "", "", "dev"), Stop: "0 99 * * *"},
	} {
		if err := e.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The capability check is what stops someone scheduling something the backend
// can't do — the issue asks for it at creation time, not at fire time.
func TestEntry_ValidateNamesTheBackend(t *testing.T) {
	e := Entry{Ref: ref("vmware", "", "", "dev"), Start: "0 9 * * *"}
	err := e.Validate()
	if err == nil {
		t.Fatal("want a rejection")
	}
	if !strings.Contains(err.Error(), "vmware") {
		t.Errorf("rejection does not name the backend: %v", err)
	}
}

// ── store ─────────────────────────────────────────────────────────

// A bare name is not a key. Two contexts running a `dev` must hold two
// schedules, or one silently overwrites the other and an instance somewhere
// stops being managed.
func TestStore_SameNameInDifferentContextsAreSeparateSchedules(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "schedules.json"))

	lab := Entry{Ref: ref("incus", "lab", "", "dev"), Start: "0 9 * * 1-5", Stop: "0 18 * * 1-5"}
	dr := Entry{Ref: ref("incus", "dr", "", "dev"), Start: "0 7 * * *"}
	local := Entry{Ref: ref("qemu", "", "", "dev"), Stop: "0 23 * * *"}

	for _, e := range []Entry{lab, dr, local} {
		if err := store.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 distinct schedules, got %d: %+v", len(entries), entries)
	}

	// Removing one context's schedule must leave the others alone.
	if err := store.Remove(lab.Ref); err != nil {
		t.Fatal(err)
	}
	entries, _ = store.List()
	if len(entries) != 2 {
		t.Fatalf("removing one schedule took out %d", 3-len(entries))
	}
	for _, e := range entries {
		if e.ID() == lab.ID() {
			t.Error("the wrong schedule survived")
		}
	}
}

func TestStore_PutReplacesRatherThanDuplicates(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "schedules.json"))
	r := ref("qemu", "", "", "dev")

	if err := store.Put(Entry{Ref: r, Start: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Entry{Ref: r, Start: "0 10 * * *"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.List()
	if len(entries) != 1 {
		t.Fatalf("re-scheduling the same instance made %d entries", len(entries))
	}
	if entries[0].Start != "0 10 * * *" {
		t.Errorf("start = %q, want the replacement", entries[0].Start)
	}
}

func TestStore_RemoveIsIdempotent(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "schedules.json"))
	if err := store.Remove(ref("qemu", "", "", "never-scheduled")); err != nil {
		t.Errorf("removing a schedule that was never there failed: %v", err)
	}
}

func TestStore_RejectsAnUnrunnableSchedule(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "schedules.json"))
	if err := store.Put(Entry{Ref: ref("qemu", "", "", "dev"), Start: "@hourly"}); err == nil {
		t.Fatal("stored a schedule the tick could never evaluate")
	}
	if entries, _ := store.List(); len(entries) != 0 {
		t.Errorf("the rejected schedule was written anyway: %+v", entries)
	}
}

// ── tick across three simultaneous backend contexts ───────────────

// The issue asks for coverage of at least three backend contexts at once. This
// drives a real tick through the actual backend clients — with their command
// runners faked — so it proves the reference-to-command mapping, not just that
// a dispatcher was called.
func TestTick_ThreeBackendContextsAtOnce(t *testing.T) {
	fake := wireAllBackends(t)

	kube := ref("kubevirt", "", "corral-vms", "web")
	lab := ref("incus", "lab", "", "dev")
	local := ref("qemu", "", "", "laptop")

	// 09:00 on a Thursday: all three start.
	entries := []Entry{
		{Ref: kube, Start: "0 9 * * 1-5", Stop: "0 18 * * 1-5"},
		{Ref: lab, Start: "0 9 * * 1-5"},
		{Ref: local, Start: "0 9 * * 1-5"},
	}
	at := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	results := Tick(entries, at)
	if len(results) != 3 {
		t.Fatalf("want one result per context, got %d: %s", len(results), Summary(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Ref.String(), r.Err)
		}
		if r.Action != lifecycle.Start {
			t.Errorf("%s: action = %q, want start", r.Ref.String(), r.Action)
		}
	}

	// Each backend got its own command, with its own context.
	calls := strings.Join(commandStrings(fake), "\n")
	for _, want := range []string{
		"virtctl start web -n corral-vms",
		"incus start lab:dev",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in:\n%s", want, calls)
		}
	}
}

// One unreachable context must not stop the others, and must not cost the
// operator their schedule — the entry stays and the next tick retries.
func TestTick_AnUnreachableContextIsIsolated(t *testing.T) {
	fake := wireAllBackends(t)
	// The Incus remote is down; everything else is fine.
	fake.AddResponse("incus start dr:dev", "Error: Get \"https://dr:8443\": dial tcp: connection refused", errSimulated{})

	entries := []Entry{
		{Ref: ref("incus", "dr", "", "dev"), Start: "0 9 * * *"},
		{Ref: ref("qemu", "", "", "laptop"), Start: "0 9 * * *"},
	}
	at := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	results := Tick(entries, at)
	if len(results) != 2 {
		t.Fatalf("want both contexts attempted, got %s", Summary(results))
	}
	failed := Failed(results)
	if len(failed) != 1 {
		t.Fatalf("want exactly the down context to fail, got %s", Summary(results))
	}
	if failed[0].Ref.Context != "dr" {
		t.Errorf("the wrong context failed: %s", failed[0].Ref.String())
	}
	for _, r := range results {
		if r.Ref.Backend == "qemu" && r.Err != nil {
			t.Errorf("the healthy local context was dragged down: %v", r.Err)
		}
	}
}

// A schedule file edited by hand can hold nonsense. One bad entry must not
// silently cancel the tick for every good one.
func TestTick_AnInvalidEntryDoesNotAbortTheTick(t *testing.T) {
	wireAllBackends(t)

	entries := []Entry{
		{Ref: ref("vmware", "", "", "ghost"), Start: "0 9 * * *"},
		{Ref: ref("qemu", "", "", "laptop"), Start: "0 9 * * *"},
	}
	results := Tick(entries, time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC))

	var ranLocal bool
	for _, r := range results {
		if r.Ref.Backend == "qemu" && r.Err == nil {
			ranLocal = true
		}
	}
	if !ranLocal {
		t.Fatalf("a bad entry stopped a good one: %s", Summary(results))
	}
	if len(Failed(results)) != 1 {
		t.Errorf("want the bad entry reported once: %s", Summary(results))
	}
}

func TestTick_NothingDueDoesNothing(t *testing.T) {
	fake := wireAllBackends(t)
	entries := []Entry{{Ref: ref("qemu", "", "", "laptop"), Start: "0 9 * * 1-5"}}

	// Saturday: the weekday schedule must not fire.
	results := Tick(entries, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))
	if len(results) != 0 {
		t.Fatalf("fired off-schedule: %s", Summary(results))
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("touched a backend with nothing due: %v", commandStrings(fake))
	}
}

// ── helpers ───────────────────────────────────────────────────────

type errSimulated struct{}

func (errSimulated) Error() string { return "simulated failure" }

// wireAllBackends points every backend client at one fake runner, so a tick
// spanning contexts can be observed in a single call log.
func wireAllBackends(t *testing.T) *shell.Fake {
	t.Helper()
	fake := shell.NewFake()

	// KubeVirt starts through virtctl; the fake resolves any LookPath.
	fake.AddPrefixResponse("/fake/bin/virtctl", "", nil)
	fake.AddPrefixResponse("virtctl", "", nil)
	fake.AddPrefixResponse("kubectl", "", nil)
	fake.AddPrefixResponse("incus", "", nil)
	fake.AddPrefixResponse("virsh", "", nil)

	kubevirt.SetDefaultRunner(fake)
	kubevirt.SetPackageRunner(fake)
	incus.SetRunner(fake)
	libvirt.SetRunner(fake)

	// The local backend is systemd, not a command runner: give it a throwaway
	// state dir, a real unit file (pkg/qemu treats a missing one as "no such
	// VM"), and an in-memory systemctl.
	home, units := t.TempDir(), t.TempDir()
	mkLocalVM(t, home, units, "laptop")
	qemu.SetStateDirs(home, units)
	qemu.SetSystemctl(func(...string) ([]byte, error) { return []byte("inactive\n"), nil })

	t.Cleanup(func() {
		kubevirt.SetDefaultRunner(shell.Real{})
		kubevirt.SetPackageRunner(shell.Real{})
		incus.SetRunner(shell.Real{})
		libvirt.SetRunner(shell.Real{})
		qemu.SetStateDirs("", "")
		qemu.SetSystemctl(nil)
	})
	return fake
}

func commandStrings(fake *shell.Fake) []string {
	var out []string
	for _, c := range fake.Calls() {
		out = append(out, strings.TrimSpace(c.Name+" "+strings.Join(c.Args, " ")))
	}
	return out
}
