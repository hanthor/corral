package schedule

import (
	"os"
	"path/filepath"
	"testing"
)

// mkLocalVM lays out the on-disk shape pkg/qemu expects for one VM: a state
// directory with metadata, and a systemd unit — pkg/qemu.Start reads a missing
// unit as "no such VM", so both are needed for the local backend to act.
func mkLocalVM(t *testing.T, home, units, name string) {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"` + name + `","cpu":2,"memory":"4G","disk_size":"20G","vnc_port":5901}`
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "corral-"+name+".service"), []byte("# test unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
