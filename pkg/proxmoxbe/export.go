package proxmoxbe

// Exporting a VM disk for cross-backend moves (ADR-0010).
//
// The PVE REST API has no direct "download this disk image" endpoint for
// block-based storages, so the export path uses vzdump: create a backup on a
// file-based storage, download it, and let the caller extract the disk.
//
// This is deliberately separate from the Backup method: Backup is for the
// operator who wants a PVE-native backup archive, and ExportDisk is for the
// move pipeline, which needs the archive on local disk so the disk can be
// extracted and converted to qcow2.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ExportDisk saves a VM's boot disk as a backup archive at destPath.
//
// It uses vzdump in snapshot mode to avoid stopping the VM, places the archive
// on the first backup-capable file-based storage, waits for the task to
// complete, and downloads the archive to the local path.
//
// The resulting file is a PVE backup archive (a .vma file, possibly compressed
// with zstd). The caller is responsible for extracting the disk image from it
// and converting to the desired format.
func (c *Client) ExportDisk(name, destPath string) error {
	// 1. Find a backup-capable storage that is file-based.
	storage, err := c.findBackupStorage()
	if err != nil {
		return err
	}

	// 2. Trigger vzdump.
	guest, err := c.Resolve(name)
	if err != nil {
		return fmt.Errorf("proxmox: resolving %s for export: %w", name, err)
	}

	// Run vzdump with no compression — the file is downloaded immediately, and
	// compressing several gigabytes only to decompress them a second later is
	// pure cost.
	task, err := c.Backup(name, storage.Storage, "snapshot")
	if err != nil {
		return fmt.Errorf("proxmox: vzdump on %s: %w", name, err)
	}
	if err := c.WaitTask(task, DefaultTimeout); err != nil {
		return fmt.Errorf("proxmox: vzdump task failed: %w", err)
	}

	// 3. Find the backup file in the storage content list.
	volume, err := c.findLatestBackup(storage.Storage, guest.VMID)
	if err != nil {
		return fmt.Errorf("proxmox: locating the backup archive: %w", err)
	}

	// 4. Download the archive.
	if err := c.downloadVolume(guest.Node, storage.Storage, "backup", volume, destPath); err != nil {
		return fmt.Errorf("proxmox: downloading the backup: %w", err)
	}

	return nil
}

// findBackupStorage finds a backup-capable storage that supports file download.
// File-based storages (dir, nfs, cifs, glusterfs, pbs) can serve files over
// HTTP; block-based storages (lvm, zfs, rbd, cephfs) cannot.
func (c *Client) findBackupStorage() (StorageInfo, error) {
	storages, err := c.Storages()
	if err != nil {
		return StorageInfo{}, err
	}
	// Prefer a non-local directory-type storage so the download works from any
	// Corral node, not just the PVE node itself.
	var localFallback *StorageInfo
	for i := range storages {
		if !storages[i].Holds("backup") {
			continue
		}
		// Only file-based storages can serve downloads over the API.
		if !isFileStorage(storages[i].Type) {
			continue
		}
		if storages[i].Shared == 1 || storages[i].Type != "dir" {
			return storages[i], nil
		}
		if localFallback == nil {
			localFallback = &storages[i]
		}
	}
	if localFallback != nil {
		return *localFallback, nil
	}
	return StorageInfo{}, fmt.Errorf(
		"no backup-capable file-based storage found on this PVE cluster; " +
			"add a storage of type 'dir' or 'nfs' that holds 'backup' content, " +
			"or run a manual vzdump and provide the archive path")
}

// isFileStorage reports whether a storage type supports the /download API.
func isFileStorage(typ string) bool {
	switch typ {
	case "dir", "nfs", "cifs", "glusterfs", "pbs":
		return true
	}
	return false
}

// findLatestBackup lists a storage's content and returns the volume id of the
// most recent backup for the given VMID.
func (c *Client) findLatestBackup(storage string, vmid int) (string, error) {
	contents, err := c.storageContent("", storage)
	if err != nil {
		return "", err
	}
	var best string
	for _, entry := range contents {
		if entry.Content != "backup" {
			continue
		}
		// Backup volume names have the form "vzdump-qemu-{vmid}-..."
		if !strings.HasPrefix(entry.VolID, fmt.Sprintf("vzdump-qemu-%d-", vmid)) &&
			!strings.HasPrefix(entry.VolID, fmt.Sprintf("vzdump-lxc-%d-", vmid)) {
			continue
		}
		// The most recent backup sorts last by name (ISO-8601 timestamp).
		if best == "" || entry.VolID > best {
			best = entry.VolID
		}
	}
	if best == "" {
		return "", fmt.Errorf("no backup found for vmid %d on storage %q", vmid, storage)
	}
	return best, nil
}

// StorageEntry is one volume on a storage, as /storage/{storage}/content reports.
type StorageEntry struct {
	VolID      string `json:"volid"`
	Content    string `json:"content"`
	Size       int64  `json:"size"`
	Format     string `json:"format"`
	Path       string `json:"path"`
	VMID       int    `json:"vmid"`
	Parent     string `json:"parent"`
	CreateTime int64  `json:"ctime"`
}

// storageContent lists volumes on a storage. If node is empty, all nodes are
// queried; otherwise the named node is used.
func (c *Client) storageContent(node, storage string) ([]StorageEntry, error) {
	path := "/storage/" + storage + "/content"
	if node != "" {
		path = "/nodes/" + node + path
	}
	var content []StorageEntry
	if err := c.get(path, &content); err != nil {
		// If node-specific fails, try cluster-wide.
		if node != "" {
			return c.storageContent("", storage)
		}
		return nil, err
	}
	return content, nil
}

// downloadVolume fetches a volume from a storage and writes it to destPath.
// The PVE download endpoint returns the file body directly (for local storages)
// or issues a redirect to a temporary URL. Both paths are handled. The file is
// streamed rather than buffered: these are multi-gigabyte disk images.
func (c *Client) downloadVolume(node, storage, content, volume, destPath string) error {
	reqPath := fmt.Sprintf("/nodes/%s/storage/%s/download?content=%s&volume=%s",
		node, storage, content, volume)

	req, err := http.NewRequest(http.MethodGet, c.base+reqPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.Token)

	dl := &http.Client{
		Transport: c.http.Transport,
		// Do not follow redirects: the download endpoint may redirect to a
		// temporary URL that needs different auth, and bundling that logic
		// into CheckRedirect is harder than handling it explicitly.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 0, // no timeout on the download itself; the upload side has one
	}
	resp, err := dl.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: requesting %s: %w", reqPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &APIError{Status: resp.StatusCode, Method: http.MethodGet, Path: reqPath,
			Message: errorMessage(payload)}
	}

	// A redirect (302/307) means the file is served elsewhere.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return c.downloadFromURL(loc, destPath)
		}
		return fmt.Errorf("proxmox: download %s: redirect with no Location header", reqPath)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxmox: download %s: unexpected status %d", reqPath, resp.StatusCode)
	}

	return c.saveResponse(resp, destPath)
}

// downloadFromURL fetches a file from a URL and writes it to destPath.
func (c *Client) downloadFromURL(urlStr, destPath string) error {
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: following redirect %s: %w", urlStr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxmox: download redirect %s returned %d", urlStr, resp.StatusCode)
	}
	return c.saveResponse(resp, destPath)
}

func (c *Client) saveResponse(resp *http.Response, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("proxmox: writing %s: %w", destPath, err)
	}
	return f.Close()
}
