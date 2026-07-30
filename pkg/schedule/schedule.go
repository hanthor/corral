// Package schedule stores lifecycle schedules against canonical instance
// references and evaluates which are due (#133).
//
// The schedule plugin previously wrote a Kubernetes CronJob per boundary,
// which ties it to KubeVirt twice over: the CronJob controller has to exist to
// fire it, and the job's script speaks kubectl. Neither is available for a VM
// on a laptop, an Incus remote, or a libvirt host.
//
// So the schedule itself is stored here, keyed by the full reference rather
// than a bare name — "stop dev at 18:00" is meaningless if three contexts have
// a `dev` — and a tick evaluates what is due and dispatches through
// pkg/lifecycle. On KubeVirt the CronJob path stays available and is still the
// better choice, because it survives the workstation being closed; see
// docs/adr and the plugin's --in-cluster flag.
package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/lifecycle"
	"github.com/tuna-os/corral/pkg/types"
)

// Entry is one instance's start/stop window. Either boundary may be empty:
// "start at 09:00 and never stop automatically" is a legitimate policy.
type Entry struct {
	Ref   types.InstanceRef `json:"ref"`
	Start string            `json:"start,omitempty"` // cron; empty = no autostart
	Stop  string            `json:"stop,omitempty"`  // cron; empty = no autostop
}

// ID is the entry's stable key: the canonical reference string. Two contexts
// running a same-named instance get distinct IDs, which is the whole point.
func (e Entry) ID() string { return e.Ref.String() }

// Validate rejects a schedule that could never work, at creation time rather
// than at 3am on the first tick.
func (e Entry) Validate() error {
	if err := e.Ref.Validate(); err != nil {
		return err
	}
	if e.Start == "" && e.Stop == "" {
		return fmt.Errorf("a schedule needs at least one of start or stop")
	}
	if !lifecycle.Supported(e.Ref.Backend) {
		return fmt.Errorf("the %s backend cannot be started and stopped by Corral, so it cannot be scheduled", e.Ref.Backend)
	}
	for label, expr := range map[string]string{"start": e.Start, "stop": e.Stop} {
		if expr == "" {
			continue
		}
		if _, err := ParseCron(expr); err != nil {
			return fmt.Errorf("%s schedule: %w", label, err)
		}
	}
	return nil
}

// Due reports the actions this entry calls for at t. Both can fire at once if
// someone schedules a start and a stop on the same minute; the caller decides
// what that means rather than this silently dropping one.
func (e Entry) Due(t time.Time) []lifecycle.Action {
	var actions []lifecycle.Action
	if e.Start != "" {
		if c, err := ParseCron(e.Start); err == nil && c.Matches(t) {
			actions = append(actions, lifecycle.Start)
		}
	}
	if e.Stop != "" {
		if c, err := ParseCron(e.Stop); err == nil && c.Matches(t) {
			actions = append(actions, lifecycle.Stop)
		}
	}
	return actions
}

// ── store ─────────────────────────────────────────────────────────

// Store persists entries as JSON. It is deliberately a plain file rather than
// anything cleverer: schedules are a handful of lines, and a format an
// operator can read and hand-edit is worth more here than one that scales.
type Store struct{ path string }

// DefaultPath is where schedules live, alongside the instance registry.
func DefaultPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "schedules.json"
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "corral", "schedules.json")
}

func NewStore() *Store           { return &Store{path: DefaultPath()} }
func NewStoreAt(p string) *Store { return &Store{path: p} }

func (s *Store) Path() string { return s.path }

// List returns every schedule, ordered by ID so output and tests are stable.
func (s *Store) List() ([]Entry, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s is not readable as a schedule list: %w", s.path, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID() < entries[j].ID() })
	return entries, nil
}

// Put adds or replaces the schedule for an instance.
func (s *Store) Put(e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	entries, err := s.List()
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].ID() == e.ID() {
			entries[i], replaced = e, true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	return s.write(entries)
}

// Remove drops one instance's schedule. Removing one that isn't there is not
// an error — `rm` should be safe to run twice.
func (s *Store) Remove(ref types.InstanceRef) error {
	entries, err := s.List()
	if err != nil {
		return err
	}
	id := ref.String()
	kept := entries[:0]
	for _, e := range entries {
		if e.ID() != id {
			kept = append(kept, e)
		}
	}
	return s.write(kept)
}

func (s *Store) write(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if entries == nil {
		entries = []Entry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: a tick reading the file while this runs sees either
	// the old list or the new one, never a truncated one.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ── tick ──────────────────────────────────────────────────────────

// Result is what one entry's tick did. Err is set when the instance's context
// could not be reached, which is expected rather than exceptional: a laptop
// wakes with the VPN down, a remote is rebooting.
type Result struct {
	Ref    types.InstanceRef
	Action lifecycle.Action
	Err    error
}

func (r Result) String() string {
	status := "ok"
	if r.Err != nil {
		status = r.Err.Error()
	}
	return fmt.Sprintf("%s %s: %s", r.Action, r.Ref.String(), status)
}

// Tick runs every schedule due at t and returns one Result per attempted
// action.
//
// A context that cannot be reached does not stop the others and does not
// disable the schedule: the entry stays, the failure is reported, and the next
// tick tries again. The alternative — dropping a schedule because a remote was
// briefly down — loses an operator's policy for a transient reason.
func Tick(entries []Entry, t time.Time) []Result {
	var results []Result
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			// A hand-edited file can hold something unrunnable. Report it
			// against the entry rather than aborting the whole tick.
			results = append(results, Result{Ref: e.Ref, Err: fmt.Errorf("invalid schedule: %w", err)})
			continue
		}
		for _, action := range e.Due(t) {
			results = append(results, Result{Ref: e.Ref, Action: action, Err: lifecycle.Do(e.Ref, action)})
		}
	}
	return results
}

// Failed returns the results that errored, for a caller deciding an exit code.
func Failed(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.Err != nil {
			out = append(out, r)
		}
	}
	return out
}

// Summary renders results for a log line, newest concerns first.
func Summary(results []Result) string {
	if len(results) == 0 {
		return "nothing due"
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, "; ")
}
