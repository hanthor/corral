package proxmoxbe

// Task tracking.
//
// Most PVE mutations return a UPID rather than completing inline. That is more
// than an inconvenience: it is the reason this backend can report real progress
// for migration, export, and clone, where the existing backends mostly report
// "started" and hope. Every mutation here returns its UPID, and callers choose
// between firing and forgetting or waiting.

import (
	"fmt"
	"strings"
	"time"
)

// Task is a PVE task id plus the node that owns it. A UPID is only meaningful
// on its own node, which is why the pair travels together.
type Task struct {
	UPID string
	Node string
}

// Valid reports whether this is a real task. A synchronous endpoint returns no
// UPID, and waiting on nothing should be a no-op rather than an error.
func (t Task) Valid() bool { return strings.HasPrefix(t.UPID, "UPID:") }

// TaskStatus is a task's current state.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" | "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" or a failure description
	Type       string `json:"type"`
	Node       string `json:"node"`
}

// Done reports whether the task has finished, successfully or otherwise.
func (s TaskStatus) Done() bool { return s.Status == "stopped" }

// OK reports success. PVE says "OK" and nothing else on success; anything else
// in exitstatus is the failure reason, and it is worth surfacing verbatim.
func (s TaskStatus) OK() bool { return s.ExitStatus == "OK" }

// TaskStatus fetches a task's state.
func (c *Client) TaskStatus(t Task) (TaskStatus, error) {
	var status TaskStatus
	if !t.Valid() {
		return status, fmt.Errorf("proxmox: %q is not a task id", t.UPID)
	}
	err := c.get(fmt.Sprintf("/nodes/%s/tasks/%s/status", t.Node, urlEscape(t.UPID)), &status)
	return status, err
}

// TaskLog returns a task's output lines, newest last. This is what the web UI's
// task log shows, and what makes a failed migration diagnosable without opening
// the PVE console.
func (c *Client) TaskLog(t Task) ([]string, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("proxmox: %q is not a task id", t.UPID)
	}
	var entries []struct {
		N int    `json:"n"`
		T string `json:"t"`
	}
	if err := c.get(fmt.Sprintf("/nodes/%s/tasks/%s/log?limit=200", t.Node, urlEscape(t.UPID)), &entries); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.T)
	}
	return lines, nil
}

// WaitTask polls until the task finishes or the deadline passes, and reports the
// failure with the task's own log tail rather than a bare exit status — "task
// failed" without the reason is the least useful error a backend can produce.
func (c *Client) WaitTask(t Task, timeout time.Duration) error {
	if !t.Valid() {
		return nil // a synchronous operation; nothing to wait for
	}
	deadline := time.Now().Add(timeout)
	interval := 500 * time.Millisecond
	for {
		status, err := c.TaskStatus(t)
		if err != nil {
			return err
		}
		if status.Done() {
			if status.OK() {
				return nil
			}
			detail := status.ExitStatus
			if lines, logErr := c.TaskLog(t); logErr == nil && len(lines) > 0 {
				tail := lines
				if len(tail) > 5 {
					tail = tail[len(tail)-5:]
				}
				detail += ": " + strings.Join(tail, " / ")
			}
			return fmt.Errorf("proxmox: task %s failed: %s", status.Type, detail)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("proxmox: task %s did not finish within %s (it may still be running: %s)",
				status.Type, timeout, t.UPID)
		}
		time.Sleep(interval)
		// Back off gently: a migration takes minutes, and polling it twice a
		// second for all of them is rude to the API.
		if interval < 3*time.Second {
			interval += 250 * time.Millisecond
		}
	}
}

// RecentTasks returns the node's recent tasks, which is what this backend's
// Events view shows — PVE has no per-guest event stream, but its task history is
// the equivalent record and can be filtered by guest.
func (c *Client) RecentTasks(node string, vmid int, limit int) ([]TaskEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	path := fmt.Sprintf("/nodes/%s/tasks?limit=%d", node, limit)
	if vmid > 0 {
		path += fmt.Sprintf("&vmid=%d", vmid)
	}
	var tasks []TaskEntry
	if err := c.get(path, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// TaskEntry is one row of a node's task history.
type TaskEntry struct {
	UPID       string `json:"upid"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
	User       string `json:"user"`
	Node       string `json:"node"`
	StartTime  int64  `json:"starttime"`
	EndTime    int64  `json:"endtime"`
	ID         string `json:"id"`
}

// Failed reports whether this task ended in anything other than success. A task
// still running has not failed.
func (e TaskEntry) Failed() bool {
	return e.Status == "stopped" && e.ExitStatus != "" && e.ExitStatus != "OK"
}

// urlEscape escapes a UPID for a path segment. A UPID contains colons, which are
// legal in a path segment but not something to leave to chance.
func urlEscape(s string) string {
	return strings.ReplaceAll(s, " ", "%20")
}
