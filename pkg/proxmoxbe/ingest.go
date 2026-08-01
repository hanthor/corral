package proxmoxbe

// Uploading a disk image into PVE so a VM can be created from it (ADR-0010).
//
// This is the one place ADR-0009's "API only, never SSH" bends, and it bends
// rather than breaks. PVE's storage upload endpoint historically accepted only
// `iso`, `vztmpl` and `backup` content — not disk images — which is why moving
// a VM *into* Proxmox needed a shell on a node and `qm importdisk`. PVE 8.4
// added an `import` content type, and a storage that advertises it can take a
// qcow2 over the same authenticated HTTPS API as everything else.
//
// So the rule here is: find a storage that says it holds `import`, and if none
// does, refuse and name the three ways forward. An operator learning "your PVE
// needs an import-content storage" from a preflight is in a much better place
// than one learning it from a half-created VM.

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ImportContent is the PVE content type for disk images that can seed a VM.
const ImportContent = "import"

// ImportStorage finds a storage that can accept a disk image upload.
//
// Shared storages are preferred: on a cluster, an image uploaded to a
// node-local storage can only seed a VM on that one node, which turns a move
// into a placement decision the operator did not make.
func (c *Client) ImportStorage() (StorageInfo, error) {
	storages, err := c.Storages()
	if err != nil {
		return StorageInfo{}, err
	}
	var fallback *StorageInfo
	for i := range storages {
		if !storages[i].Holds(ImportContent) {
			continue
		}
		if storages[i].Shared == 1 {
			return storages[i], nil
		}
		if fallback == nil {
			fallback = &storages[i]
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return StorageInfo{}, fmt.Errorf(
		"no PVE storage advertises the %q content type, so a disk image cannot be uploaded over the API. "+
			"Either enable import content on a storage (PVE 8.4+: Datacenter → Storage → Content), "+
			"place the image on a shared storage path yourself, or import it on a node with `qm importdisk`",
		ImportContent)
}

// UploadImport streams a local disk image into an import-content storage and
// returns the PVE volume id to create from.
//
// The upload is streamed rather than buffered: these are multi-gigabyte files,
// and reading one into memory to post it would take the server down before the
// disk arrived.
func (c *Client) UploadImport(node, storage, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("proxmox: reading the disk to upload: %w", err)
	}
	defer file.Close()

	filename := filepath.Base(path)
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)

	go func() {
		// Any error here closes the pipe with that error, so the request fails
		// with the real cause rather than an opaque short write.
		defer writer.Close()
		if err := form.WriteField("content", ImportContent); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		part, err := form.CreateFormFile("filename", filename)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if err := form.Close(); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
	}()

	path = fmt.Sprintf("/nodes/%s/storage/%s/upload", node, storage)
	req, err := http.NewRequest(http.MethodPost, c.base+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.Token)
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("proxmox: uploading %s: %w", filename, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &APIError{Status: resp.StatusCode, Method: http.MethodPost, Path: path,
			Message: errorMessage(payload)}
	}

	// The upload runs as a task; the volume is not usable until it finishes.
	if upid := strings.Trim(strings.TrimSpace(extractUPID(payload)), `"`); upid != "" {
		if err := c.WaitTask(Task{UPID: upid, Node: node}, DefaultTimeout); err != nil {
			return "", fmt.Errorf("proxmox: the upload task failed: %w", err)
		}
	}
	return fmt.Sprintf("%s:import/%s", storage, filename), nil
}

// extractUPID pulls the task id out of an upload response. PVE returns it in
// the usual data envelope; an empty result means the upload was synchronous,
// which older versions do.
func extractUPID(payload []byte) string {
	text := string(payload)
	start := strings.Index(text, "UPID:")
	if start < 0 {
		return ""
	}
	end := start
	for end < len(text) && text[end] != '"' && text[end] != '\n' {
		end++
	}
	return text[start:end]
}
