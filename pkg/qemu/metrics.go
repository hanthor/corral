package qemu

// Live resource usage for a local VM, read from the systemd unit's cgroup.
//
// systemd already accounts for the QEMU process — it is the thing that started
// it — so this needs no agent in the guest and no extra socket. What it
// measures is the *host* cost of running the VM: the QEMU process's resident
// memory and CPU time, not what the guest thinks it is using. Those differ
// (ballooning, page sharing, the guest's own idea of free memory), and the
// difference is worth knowing rather than papering over, so the doc comments
// and the returned keys say host.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cpuSampleInterval is how long Metrics waits between the two CPU readings.
//
// CPUUsageNSec is cumulative, so a single reading says how much CPU the VM has
// used since it booted — which is not what anyone means by "CPU usage". Two
// readings a known time apart give a rate. 200ms is long enough to be stable
// against scheduler jitter and short enough that a fleet view polling every
// few seconds does not feel it.
var cpuSampleInterval = 200 * time.Millisecond

// Metrics reports the VM's host-side CPU and memory usage.
//
// Returns empty strings rather than an error when the unit is not running or
// the cgroup has no accounting: a stopped VM legitimately has no usage, and a
// fleet view should render a blank cell rather than an error row.
func Metrics(name string) (map[string]string, error) {
	res := map[string]string{"cpu": "", "mem": ""}
	unit := "corral-" + name

	memBytes, memOK := unitProperty(unit, "MemoryCurrent")
	if memOK && memBytes > 0 {
		res["mem"] = humanBytes(memBytes)
	}

	first, ok := unitProperty(unit, "CPUUsageNSec")
	if !ok {
		return res, nil
	}
	time.Sleep(cpuSampleInterval)
	second, ok := unitProperty(unit, "CPUUsageNSec")
	if !ok || second < first {
		// A restart between samples resets the counter; reporting the negative
		// delta as a huge number would be worse than reporting nothing.
		return res, nil
	}
	// Percent of one core. A 4-vCPU VM saturating every core reads 400%, which
	// is what `top` does and what an operator expects.
	percent := float64(second-first) / float64(cpuSampleInterval.Nanoseconds()) * 100
	res["cpu"] = fmt.Sprintf("%.0f%%", percent)
	return res, nil
}

// unitProperty reads one numeric systemd unit property. systemd prints
// "[NOT_SET]" for an unaccounted property, which parses as not-ok rather than
// zero — zero would claim the VM is using no memory when the truth is that
// nobody is counting.
func unitProperty(unit, property string) (int64, bool) {
	out, err := systemctlRun("show", unit, "-p", property, "--value")
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%dKi", b/1024)
}

// Events returns the unit's recent journal lines.
//
// The local backend is the one place where event history genuinely exists
// without Corral keeping its own: journald already has it, keyed by the unit
// systemd started. libvirt and Incus have no equivalent — both only offer a
// subscription to what happens next — which is why they implement Metricser
// and not Eventer.
func Events(name string) ([]JournalEntry, error) {
	out, err := journalRun("--user", "-u", "corral-"+name,
		"-n", "50", "--no-pager", "-o", "short-iso")
	if err != nil {
		return nil, fmt.Errorf("reading the journal for %s: %w", name, err)
	}
	var entries []JournalEntry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-- ") {
			continue // journald's "-- No entries --" and boot separators
		}
		entries = append(entries, parseJournalLine(line))
	}
	return entries, nil
}

// JournalEntry is one journal line, split into the parts the Event contract
// wants. Kind is a guess from the text — journald's priority is not in
// short-iso output, and re-running with -o json to get it would double the
// cost of a view that refreshes on a timer.
type JournalEntry struct {
	Time    string
	Kind    string // "Normal" | "Warning"
	Message string
}

func parseJournalLine(line string) JournalEntry {
	entry := JournalEntry{Time: "", Kind: "Normal", Message: line}
	// "2026-08-05T10:11:12+0000 host corral-vm[123]: message"
	if fields := strings.SplitN(line, " ", 4); len(fields) == 4 {
		entry.Time = fields[0]
		entry.Message = fields[3]
	}
	lower := strings.ToLower(entry.Message)
	for _, bad := range []string{"error", "fail", "warn", "cannot", "could not", "refused", "denied", "timed out"} {
		if strings.Contains(lower, bad) {
			entry.Kind = "Warning"
			break
		}
	}
	return entry
}
