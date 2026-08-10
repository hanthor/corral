package proxmoxbe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExportVzdumpUsesTheNodeStream(t *testing.T) {
	client, err := New(Config{Host: "pve.example", Token: "root@pam!corral=secret"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	SetVzdumpRunner(func(_ context.Context, got Guest, dest string) error {
		called = true
		if got.Node != "pve1" || got.VMID != 101 {
			t.Fatalf("guest = %+v, want pve1/101", got)
		}
		return os.WriteFile(dest, []byte("vma.zst"), 0o600)
	})
	t.Cleanup(func() { SetVzdumpRunner(nil) })

	dest := filepath.Join(t.TempDir(), "backup.vma.zst")
	err = client.ExportVzdump(context.Background(), Guest{Node: "pve1", VMID: 101}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("vzdump stream runner was not called")
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "vma.zst" {
		t.Fatalf("destination = %q, err=%v", data, err)
	}
}

func TestExportVzdumpRejectsIncompleteGuest(t *testing.T) {
	client, err := New(Config{Host: "pve.example", Token: "root@pam!corral=secret"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.ExportVzdump(context.Background(), Guest{Node: "pve1"}, filepath.Join(t.TempDir(), "backup"))
	if err == nil {
		t.Fatal("incomplete guest was accepted")
	}
}
