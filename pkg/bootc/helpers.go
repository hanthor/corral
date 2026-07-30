package bootc

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/qemu"
)

// Seams for the local-QEMU target, so a test can exercise Import without a
// real VM home or a real qemu.
var (
	vmHome        = qemu.VMHome
	createLocalVM = qemu.Create
)

// qemuImg resolves qemu-img the way pkg/qemu does, so a brew-only host behaves
// the same here.
func qemuImg() string {
	if bin, err := runner.LookPath("qemu-img"); err == nil {
		return bin
	}
	for _, base := range []string{"/home/linuxbrew/.linuxbrew/bin", "/usr/bin", "/usr/local/bin"} {
		candidate := filepath.Join(base, "qemu-img")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "qemu-img"
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// memoryMiB normalises 4Gi/4G/4096 into the MiB libvirt wants.
//
// A bare number is MiB, not bytes: that is the unit the rest of Corral and
// libvirt both use, and running it through parseSize would read "2048" as two
// kilobytes and hand libvirt a zero.
func memoryMiB(mem string) string {
	const fallback = "4096"
	value := strings.TrimSpace(mem)
	if value == "" {
		return fallback
	}
	if !strings.ContainsAny(value, "GgMmTtKkIiBb") {
		var n int
		if _, err := fmt.Sscanf(value, "%d", &n); err == nil && n > 0 {
			return fmt.Sprint(n)
		}
		return fallback
	}
	bytes, err := parseSize(value)
	if err != nil || bytes < 1<<20 {
		return fallback
	}
	return fmt.Sprint(bytes / (1 << 20))
}

func writeTemp(content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "corral-bootc-*.xml")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// writeKey puts the SSH public key somewhere the build container can read it.
// Empty is allowed and means "no key", but it is worth being deliberate about:
// a bootc disk built with no key has no way in at all.
func writeKey(key string) (path string, cleanup func(), err error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "corral-buildkey-*.pub")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	// The container reads it, so it has to be world-readable.
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
