package qemu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// qmpResponse covers the three message shapes QEMU's QMP monitor sends:
// the initial greeting, a command reply (return/error), and async events.
// Only one of Return/Error/Event is populated per message.
type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
}

// qmpDial connects to a VM's QMP unix socket and completes the
// qmp_capabilities handshake QEMU requires before accepting any other
// command.
func qmpDial(sockPath string) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to QMP socket %s: %w (is the VM running?)", sockPath, err)
	}
	reader := bufio.NewReader(conn)

	// Greeting arrives unprompted on connect.
	if _, err := readQMPMessage(reader); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("reading QMP greeting: %w", err)
	}

	if _, err := qmpExecute(conn, reader, "qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("qmp_capabilities: %w", err)
	}
	return conn, reader, nil
}

// readQMPMessage reads one newline-delimited JSON message, skipping async
// events, and returns the first return/error reply it sees.
func readQMPMessage(reader *bufio.Reader) (*qmpResponse, error) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.Event != "" {
			continue
		}
		return &resp, nil
	}
}

func qmpExecute(conn net.Conn, reader *bufio.Reader, command string, args map[string]any) (json.RawMessage, error) {
	req := map[string]any{"execute": command}
	if args != nil {
		req["arguments"] = args
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	resp, err := readQMPMessage(reader)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", resp.Error.Class, resp.Error.Desc)
	}
	return resp.Return, nil
}

// Screenshot captures the running VM's current framebuffer via its QMP
// monitor socket and writes a PNG to outPath (default:
// "<name>-screenshot.png" in the current directory). The VM must have been
// created with this corral version (older units lack the -qmp socket) and
// must be running.
func Screenshot(name, outPath string) error {
	vmDir := filepath.Join(VMHome(), name)
	sockPath := filepath.Join(vmDir, "qmp.sock")
	if _, err := os.Stat(sockPath); err != nil {
		return fmt.Errorf("no QMP socket for %q — is it running? if it was created with an older corral, recreate it (corral create --force ...) to pick up QMP support", name)
	}

	conn, reader, err := qmpDial(sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	// screendump writes a PPM file directly via the QEMU process — since the
	// QEMU backend and the corral CLI share a host, a path next to the VM's
	// other state is reachable from both sides.
	ppmPath := filepath.Join(vmDir, "screenshot.ppm")
	defer os.Remove(ppmPath)
	if _, err := qmpExecute(conn, reader, "screendump", map[string]any{"filename": ppmPath}); err != nil {
		return fmt.Errorf("screendump: %w", err)
	}

	img, err := decodePPM(ppmPath)
	if err != nil {
		return fmt.Errorf("decoding screendump: %w", err)
	}

	if outPath == "" {
		outPath = name + "-screenshot.png"
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer f.Close()
	return png.Encode(f, img)
}

// decodePPM reads a binary PPM (P6), the format QEMU's screendump command
// produces, without pulling in an external image codec dependency.
func decodePPM(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decodePPMReader(f)
}

func decodePPMReader(f io.Reader) (image.Image, error) {
	r := bufio.NewReader(f)

	magic, err := readPPMToken(r)
	if err != nil {
		return nil, err
	}
	if magic != "P6" {
		return nil, fmt.Errorf("unsupported PPM format %q (expected P6)", magic)
	}
	width, err := readPPMInt(r)
	if err != nil {
		return nil, fmt.Errorf("reading width: %w", err)
	}
	height, err := readPPMInt(r)
	if err != nil {
		return nil, fmt.Errorf("reading height: %w", err)
	}
	maxVal, err := readPPMInt(r)
	if err != nil {
		return nil, fmt.Errorf("reading maxval: %w", err)
	}
	if maxVal != 255 {
		return nil, fmt.Errorf("unsupported PPM maxval %d (expected 255)", maxVal)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	row := make([]byte, width*3)
	for y := range height {
		if _, err := io.ReadFull(r, row); err != nil {
			return nil, fmt.Errorf("reading pixel row %d: %w", y, err)
		}
		for x := range width {
			img.SetRGBA(x, y, color.RGBA{
				R: row[x*3],
				G: row[x*3+1],
				B: row[x*3+2],
				A: 255,
			})
		}
	}
	return img, nil
}

// readPPMToken reads one whitespace-delimited header token, skipping '#'
// comments — both are part of the PPM header grammar (netpbm(5)).
func readPPMToken(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case b == '#':
			for {
				c, err := r.ReadByte()
				if err != nil || c == '\n' {
					break
				}
			}
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			if len(buf) > 0 {
				return string(buf), nil
			}
		default:
			buf = append(buf, b)
		}
	}
}

func readPPMInt(r *bufio.Reader) (int, error) {
	tok, err := readPPMToken(r)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("parsing PPM header integer %q: %w", tok, err)
	}
	return n, nil
}

// Pause and Resume stop and restart the guest's vCPUs through QMP.
//
// This is a real suspend-to-memory, not a stop: the guest's RAM stays
// allocated, its clock stops, and `cont` resumes it exactly where it was. A
// stop-then-start would be a reboot wearing the wrong name, which is the
// distinction pkg/backend's Suspender family exists to keep honest.
//
// The QMP socket only exists on VMs created by a corral new enough to add
// `-qmp` to the unit, so the error says how to get one rather than just
// reporting a missing file.
func Pause(name string) error { return qmpSimple(name, "stop", "pause") }

// Resume restarts a paused guest's vCPUs.
func Resume(name string) error { return qmpSimple(name, "cont", "resume") }

func qmpSimple(name, command, verb string) error {
	sockPath := filepath.Join(VMHome(), name, "qmp.sock")
	if _, err := os.Stat(sockPath); err != nil {
		return fmt.Errorf("no QMP socket for %q, so it cannot be %sd — is it running? "+
			"if it was created with an older corral, recreate it (corral create --force ...) "+
			"to pick up QMP support", name, verb)
	}
	conn, reader, err := qmpDial(sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := qmpExecute(conn, reader, command, nil); err != nil {
		return fmt.Errorf("qmp %s: %w", command, err)
	}
	return nil
}
