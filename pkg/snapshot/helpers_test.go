package snapshot

import (
	"os"
	"testing"

	"github.com/tuna-os/corral/pkg/qemu"
)

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setQemuHome points pkg/qemu's state directory at a throwaway tree, so the
// local adapter never reads the developer's real VMs.
func setQemuHome(t *testing.T, home string) {
	t.Helper()
	qemu.SetStateDirs(home, t.TempDir())
	t.Cleanup(func() { qemu.SetStateDirs("", "") })
}

// runningVM makes pkg/qemu report one VM as active, which is what the local
// adapter's safety check consults.
func runningVM(t *testing.T, name string) {
	t.Helper()
	qemu.SetSystemctl(func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "is-active" && args[len(args)-1] == "corral-"+name {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	})
	t.Cleanup(func() { qemu.SetSystemctl(nil) })
}

func stoppedVM(t *testing.T) {
	t.Helper()
	qemu.SetSystemctl(func(...string) ([]byte, error) { return []byte("inactive\n"), nil })
	t.Cleanup(func() { qemu.SetSystemctl(nil) })
}
