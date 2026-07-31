package proxmoxbe

// Snapshots.
//
// PVE's snapshot API is close to what pkg/snapshot's contract wants, including
// the part every other backend has to guess at: whether the capture is
// consistent. `vmstate=1` writes the guest's memory into the snapshot, which
// makes a running guest's capture as good as a suspend-and-copy; without it a
// running guest is crash-consistent and says so.

import (
	"fmt"
	"strings"
	"time"
)

// SnapshotInfo is one PVE snapshot.
type SnapshotInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SnapTime    int64  `json:"snaptime"`
	VMState     int    `json:"vmstate"`
	Parent      string `json:"parent"`
	Running     int    `json:"running"`
}

// WithMemory reports whether the guest's RAM was captured too.
func (s SnapshotInfo) WithMemory() bool { return s.VMState == 1 }

// Created renders the snapshot time as RFC3339, which is the shape every Corral
// surface formats.
func (s SnapshotInfo) Created() string {
	if s.SnapTime == 0 {
		return ""
	}
	return time.Unix(s.SnapTime, 0).UTC().Format(time.RFC3339)
}

// ListSnapshots returns a guest's snapshots. PVE includes a synthetic "current"
// entry describing the live state; it is not a snapshot and is filtered out —
// offering it as one would put a restore-to-now button on screen.
func (c *Client) ListSnapshots(name string) ([]SnapshotInfo, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return nil, err
	}
	var snaps []SnapshotInfo
	if err := c.get(guest.path()+"/snapshot", &snaps); err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		if s.Name == "current" {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// Snapshot captures a guest. withMemory asks PVE to include RAM, which is what
// makes a running guest's capture consistent rather than crash-consistent.
func (c *Client) Snapshot(name, snapName string, withMemory bool) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	if snapName == "" {
		snapName = fmt.Sprintf("corral-%d", time.Now().Unix())
	}
	if err := validSnapshotName(snapName); err != nil {
		return Task{}, err
	}
	params := map[string]string{"snapname": snapName, "description": "created by corral"}
	if withMemory && guest.Kind == KindQemu {
		params["vmstate"] = "1"
	}
	raw, err := c.post(guest.path()+"/snapshot", form(params))
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// RestoreSnapshot rolls a guest back.
func (c *Client) RestoreSnapshot(name, snapName string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	raw, err := c.post(guest.path()+"/snapshot/"+snapName+"/rollback", nil)
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// DeleteSnapshot removes one snapshot.
func (c *Client) DeleteSnapshot(name, snapName string) (Task, error) {
	guest, err := c.Resolve(name)
	if err != nil {
		return Task{}, err
	}
	raw, err := c.delete(guest.path()+"/snapshot/"+snapName, nil)
	if err != nil {
		return Task{}, err
	}
	return Task{UPID: unquote(raw), Node: guest.Node}, nil
}

// validSnapshotName enforces PVE's own rule up front. PVE rejects a bad name
// with a parameter error that does not say what the rule is.
func validSnapshotName(name string) error {
	if name == "" || len(name) > 40 {
		return fmt.Errorf("proxmox: snapshot name must be 1–40 characters")
	}
	if name == "current" {
		return fmt.Errorf("proxmox: %q is reserved for the live state", name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9', r == '_', r == '-':
			if i == 0 && (r == '_' || r == '-') {
				return fmt.Errorf("proxmox: snapshot name must start with a letter or digit")
			}
		default:
			return fmt.Errorf("proxmox: snapshot name %q may only contain letters, digits, '-' and '_'",
				name)
		}
	}
	return nil
}

// Storages lists the cluster's storages, which is what a backup or a create has
// to choose between.
func (c *Client) Storages() ([]StorageInfo, error) {
	var storages []StorageInfo
	if err := c.get("/storage", &storages); err != nil {
		return nil, err
	}
	return storages, nil
}

// StorageInfo is one PVE storage.
type StorageInfo struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Shared  int    `json:"shared"`
}

// Holds reports whether this storage accepts a content type ("images",
// "vztmpl", "iso", "backup").
func (s StorageInfo) Holds(content string) bool {
	for _, c := range strings.Split(s.Content, ",") {
		if strings.TrimSpace(c) == content {
			return true
		}
	}
	return false
}
