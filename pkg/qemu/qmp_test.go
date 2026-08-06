package qemu

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ── PPM decoding ──────────────────────────────────────────────────

func makePPM(width, height int, pixel color.RGBA) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "P6\n%d %d\n255\n", width, height)
	for range height {
		for range width {
			buf.WriteByte(pixel.R)
			buf.WriteByte(pixel.G)
			buf.WriteByte(pixel.B)
		}
	}
	return buf.Bytes()
}

func TestDecodePPM(t *testing.T) {
	data := makePPM(2, 2, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	img, err := decodePPMReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decodePPMReader: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 2 || bounds.Dy() != 2 {
		t.Fatalf("unexpected dimensions: %v", bounds)
	}
	r, g, b, a := img.At(0, 0).RGBA()
	if r>>8 != 10 || g>>8 != 20 || b>>8 != 30 || a>>8 != 255 {
		t.Errorf("unexpected pixel: r=%d g=%d b=%d a=%d", r>>8, g>>8, b>>8, a>>8)
	}
}

func TestDecodePPM_WithComment(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("P6\n# a comment\n1 1\n255\n")
	buf.Write([]byte{100, 150, 200})

	img, err := decodePPMReader(&buf)
	if err != nil {
		t.Fatalf("decodePPMReader with comment: %v", err)
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if r>>8 != 100 || g>>8 != 150 || b>>8 != 200 {
		t.Errorf("unexpected pixel after comment header: r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

func TestDecodePPM_WrongMagic(t *testing.T) {
	_, err := decodePPMReader(strings.NewReader("P5\n1 1\n255\n\x00"))
	if err == nil {
		t.Error("expected error for non-P6 magic")
	}
}

func TestDecodePPM_UnsupportedMaxVal(t *testing.T) {
	_, err := decodePPMReader(strings.NewReader("P6\n1 1\n65535\n\x00\x00"))
	if err == nil {
		t.Error("expected error for maxval != 255")
	}
}

func TestDecodePPM_TruncatedData(t *testing.T) {
	_, err := decodePPMReader(strings.NewReader("P6\n2 2\n255\n\x01\x02\x03"))
	if err == nil {
		t.Error("expected error for truncated pixel data")
	}
}

func TestDecodePPM_MissingFile(t *testing.T) {
	_, err := decodePPM(filepath.Join(t.TempDir(), "does-not-exist.ppm"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// ── QMP protocol + Screenshot ────────────────────────────────────

// fakeQMPServer listens on a unix socket and speaks just enough QMP to
// exercise Screenshot(): a greeting, qmp_capabilities, and screendump (which
// writes a real PPM to the requested path, mirroring what real QEMU does).
func fakeQMPServer(t *testing.T, sockPath string) {
	t.Helper()
	// sun_path caps unix socket paths at ~108 bytes; a long TMPDIR/GOTMPDIR
	// pushes t.TempDir past it and bind fails with EINVAL. Environmental,
	// not a code defect — skip rather than fail.
	if len(sockPath) > 100 {
		t.Skipf("socket path too long for sun_path (%d bytes): %s", len(sockPath), sockPath)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listening on fake QMP socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		greeting := map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []string{}}}
		gb, _ := json.Marshal(greeting)
		conn.Write(append(gb, '\n'))

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			switch req["execute"] {
			case "qmp_capabilities":
				conn.Write([]byte(`{"return": {}}` + "\n"))
			case "screendump":
				args, _ := req["arguments"].(map[string]any)
				filename, _ := args["filename"].(string)
				ppm := makePPM(1, 1, color.RGBA{R: 42, G: 84, B: 126, A: 255})
				if err := os.WriteFile(filename, ppm, 0644); err != nil {
					conn.Write([]byte(`{"error": {"class": "GenericError", "desc": "` + err.Error() + `"}}` + "\n"))
					continue
				}
				conn.Write([]byte(`{"return": {}}` + "\n"))
			default:
				conn.Write([]byte(`{"error": {"class": "CommandNotFound", "desc": "unknown command"}}` + "\n"))
			}
		}
	}()
}

func TestScreenshot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	vmDir := filepath.Join(VMHome(), "screenshotvm")
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		t.Fatalf("creating vm dir: %v", err)
	}
	fakeQMPServer(t, filepath.Join(vmDir, "qmp.sock"))

	outPath := filepath.Join(tmp, "out.png")
	if err := Screenshot("screenshotvm", outPath); err != nil {
		t.Fatalf("Screenshot: %v", err)
	}

	if info, err := os.Stat(outPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty PNG at %s: %v", outPath, err)
	}

	// The screendump temp file should be cleaned up.
	if _, err := os.Stat(filepath.Join(vmDir, "screenshot.ppm")); !os.IsNotExist(err) {
		t.Errorf("expected screenshot.ppm to be removed, stat err = %v", err)
	}
}

func TestScreenshot_DefaultOutputName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	origWD, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(origWD) })
	os.Chdir(tmp)

	vmDir := filepath.Join(VMHome(), "defaultname")
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		t.Fatalf("creating vm dir: %v", err)
	}
	fakeQMPServer(t, filepath.Join(vmDir, "qmp.sock"))

	if err := Screenshot("defaultname", ""); err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if _, err := os.Stat("defaultname-screenshot.png"); err != nil {
		t.Errorf("expected default-named screenshot file: %v", err)
	}
}

func TestScreenshot_NoSocket(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	err := Screenshot("no-such-vm", filepath.Join(tmp, "out.png"))
	if err == nil {
		t.Error("expected error when QMP socket is missing")
	}
}

// ── pause / resume ────────────────────────────────────────────────

// recordingQMPServer speaks the handshake and records every command it is
// asked to execute, so a test can assert which QMP verb was sent rather than
// only that the call returned nil.
func recordingQMPServer(t *testing.T, sockPath string, seen *[]string) {
	t.Helper()
	if len(sockPath) > 100 {
		t.Skipf("socket path too long for sun_path (%d bytes): %s", len(sockPath), sockPath)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listening on fake QMP socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Write([]byte(`{"QMP":{"version":{},"capabilities":[]}}` + "\n"))
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if json.Unmarshal(line, &req) != nil {
						continue
					}
					command, _ := req["execute"].(string)
					if command != "qmp_capabilities" {
						mu.Lock()
						*seen = append(*seen, command)
						mu.Unlock()
					}
					conn.Write([]byte(`{"return": {}}` + "\n"))
				}
			}()
		}
	}()
}

// Pause must be QMP `stop`, not a unit stop. The distinction is the whole
// point: `stop` freezes the vCPUs with the guest's RAM intact, where stopping
// the unit kills the process and loses everything not on disk.
func TestPauseAndResumeUseQMPStopAndCont(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	vmDir := filepath.Join(VMHome(), "pausevm")
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		t.Fatal(err)
	}
	var seen []string
	recordingQMPServer(t, filepath.Join(vmDir, "qmp.sock"), &seen)

	if err := Pause("pausevm"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := Resume("pausevm"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := strings.Join(seen, ","); got != "stop,cont" {
		t.Fatalf("QMP commands = %q, want \"stop,cont\"", got)
	}
}

// A VM created before corral added -qmp has no socket. The error has to say
// how to get one, because "no such file" sends someone looking for a bug that
// is not there.
func TestPauseWithoutAQMPSocketExplainsItself(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if err := os.MkdirAll(filepath.Join(VMHome(), "oldvm"), 0755); err != nil {
		t.Fatal(err)
	}

	err := Pause("oldvm")
	if err == nil {
		t.Fatal("pausing a VM with no QMP socket reported success")
	}
	for _, want := range []string{"oldvm", "corral create --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
