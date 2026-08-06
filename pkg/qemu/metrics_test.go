package qemu

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// scriptedSystemctl answers `show <unit> -p <property> --value` from a table,
// advancing through the values on repeat calls so the two CPU samples differ.
func scriptedSystemctl(t *testing.T, values map[string][]string) {
	t.Helper()
	previous := systemctlRun
	calls := map[string]int{}
	systemctlRun = func(args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "show" {
			return nil, fmt.Errorf("unexpected systemctl call: %v", args)
		}
		property := args[3]
		series, ok := values[property]
		if !ok {
			return nil, fmt.Errorf("no value scripted for %s", property)
		}
		i := calls[property]
		if i >= len(series) {
			i = len(series) - 1
		}
		calls[property]++
		return []byte(series[i] + "\n"), nil
	}
	t.Cleanup(func() { systemctlRun = previous })
}

func fastSampling(t *testing.T) {
	t.Helper()
	previous := cpuSampleInterval
	cpuSampleInterval = time.Millisecond
	t.Cleanup(func() { cpuSampleInterval = previous })
}

// CPUUsageNSec is cumulative, so a single reading is lifetime CPU time. Two
// readings a known interval apart give the rate an operator means by "CPU".
func TestMetricsComputesACPURateFromTwoSamples(t *testing.T) {
	fastSampling(t)
	// 1ms of interval, 500µs of CPU consumed between samples → 50% of a core.
	scriptedSystemctl(t, map[string][]string{
		"MemoryCurrent": {"1073741824"},
		"CPUUsageNSec":  {"1000000", "1500000"},
	})

	got, err := Metrics("dev")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if got["cpu"] != "50%" {
		t.Errorf("cpu = %q, want 50%% (500µs used over a 1ms interval)", got["cpu"])
	}
	if got["mem"] != "1.0Gi" {
		t.Errorf("mem = %q, want 1.0Gi", got["mem"])
	}
}

// A restart between the two samples resets the counter. Reporting the negative
// delta as an enormous percentage would be worse than reporting nothing.
func TestMetricsIgnoresACounterReset(t *testing.T) {
	fastSampling(t)
	scriptedSystemctl(t, map[string][]string{
		"MemoryCurrent": {"1048576"},
		"CPUUsageNSec":  {"9000000", "10000"},
	})

	got, _ := Metrics("dev")
	if got["cpu"] != "" {
		t.Errorf("cpu = %q, want empty after a counter reset", got["cpu"])
	}
	if got["mem"] == "" {
		t.Error("memory should still be reported when only the CPU sample is unusable")
	}
}

// systemd prints [NOT_SET] when a property is unaccounted. Parsing that as
// zero would claim the VM uses no memory, when the truth is nobody is counting.
func TestMetricsTreatsUnsetAsUnknownNotZero(t *testing.T) {
	fastSampling(t)
	scriptedSystemctl(t, map[string][]string{
		"MemoryCurrent": {"[NOT_SET]"},
		"CPUUsageNSec":  {"[NOT_SET]"},
	})

	got, err := Metrics("dev")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if got["mem"] != "" || got["cpu"] != "" {
		t.Fatalf("unaccounted properties reported as values: %+v", got)
	}
}

// A stopped VM has no usage, and a fleet view should render a blank cell rather
// than an error row.
func TestMetricsOnAStoppedUnitIsEmptyNotAnError(t *testing.T) {
	fastSampling(t)
	previous := systemctlRun
	systemctlRun = func(...string) ([]byte, error) { return nil, fmt.Errorf("Unit not loaded.") }
	t.Cleanup(func() { systemctlRun = previous })

	got, err := Metrics("gone")
	if err != nil {
		t.Fatalf("a stopped unit should not error: %v", err)
	}
	if got["cpu"] != "" || got["mem"] != "" {
		t.Fatalf("got usage for a unit that is not running: %+v", got)
	}
}

// ── events ────────────────────────────────────────────────────────

func TestEventsParsesJournalLines(t *testing.T) {
	previous := journalRun
	journalRun = func(...string) ([]byte, error) {
		return []byte(strings.Join([]string{
			"-- Boot 1234 --",
			"2026-08-05T10:11:12+0000 host corral-vm[1]: Started corral-dev.",
			"2026-08-05T10:11:20+0000 host corral-vm[1]: qemu: could not open disk image",
			"",
		}, "\n")), nil
	}
	t.Cleanup(func() { journalRun = previous })

	events, err := Events("dev")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (the boot separator is not an event): %+v", len(events), events)
	}
	if events[0].Time != "2026-08-05T10:11:12+0000" {
		t.Errorf("time = %q", events[0].Time)
	}
	if events[0].Message != "Started corral-dev." {
		t.Errorf("message = %q, want the text without the host and unit prefix", events[0].Message)
	}
	if events[0].Kind != "Normal" {
		t.Errorf("a normal line was classed %q", events[0].Kind)
	}
	// The severity guess: journald's priority is absent from short-iso output,
	// and re-running with -o json to get it would double the cost of a view
	// that refreshes on a timer.
	if events[1].Kind != "Warning" {
		t.Errorf("a line containing an error was classed %q", events[1].Kind)
	}
}

func TestEventsOnAnEmptyJournalReturnsNothing(t *testing.T) {
	previous := journalRun
	journalRun = func(...string) ([]byte, error) { return []byte("-- No entries --\n"), nil }
	t.Cleanup(func() { journalRun = previous })

	events, err := Events("dev")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("journald's placeholder was read as an event: %+v", events)
	}
}
