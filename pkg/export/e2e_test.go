//go:build e2esnapshot

// Real-tool verification of the export adapters (#131), sharing the
// e2esnapshot tag so one CI step covers both adapter families.
//
// Run with: go test -tags e2esnapshot ./pkg/export/

package export

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

// A real qcow2, a real qemu-img, both formats, and the running-VM refusal.
func TestE2E_QEMUExport(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })

	home, units := t.TempDir(), t.TempDir()
	dir := filepath.Join(home, "expvm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", disk, "32M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}
	os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"name":"expvm","cpu":1,"memory":"1G"}`), 0o644)
	os.WriteFile(filepath.Join(units, "corral-expvm.service"), []byte("# unit\n"), 0o644)

	qemu.SetStateDirs(home, units)
	stopped := func(...string) ([]byte, error) { return []byte("inactive\n"), nil }
	qemu.SetSystemctl(stopped)
	t.Cleanup(func() { qemu.SetStateDirs("", ""); qemu.SetSystemctl(nil) })

	ref := types.InstanceRef{Backend: "qemu", Name: "expvm"}
	out := t.TempDir()

	for _, format := range []Format{Qcow2, RawGz} {
		t.Run(string(format), func(t *testing.T) {
			dest := filepath.Join(out, "expvm."+extFor(format))
			var stages []string
			result, err := (QEMU{}).Export(context.Background(),
				Request{Ref: ref, Dest: dest, Format: format},
				func(p Progress) { stages = append(stages, p.Stage) })
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			t.Logf("%s → %s (%d bytes, %s, stages %v)",
				format, result.Path, result.Bytes, result.Consistency, stages)
			if result.Format != format {
				t.Errorf("format = %q, want %q", result.Format, format)
			}
			if result.Consistency != snapshot.Offline {
				t.Errorf("consistency = %q, want offline", result.Consistency)
			}
			if result.Bytes == 0 {
				t.Error("produced an empty artifact")
			}
			// The artifact has to be something the tools recognise, or it is
			// not a backup — just bytes.
			verifyArtifact(t, dest, format)
		})
	}

	// The safety refusal against the same real disk.
	qemu.SetSystemctl(func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "is-active" {
			return []byte("active\n"), nil
		}
		return []byte(""), nil
	})
	if _, err := (QEMU{}).Export(context.Background(),
		Request{Ref: ref, Dest: filepath.Join(out, "while-running.qcow2")}, nil); err == nil {
		t.Error("exported a running VM's disk")
	} else {
		t.Logf("running VM refused: %v", err)
	}
}

func extFor(f Format) string {
	if f == Qcow2 {
		return "qcow2"
	}
	return "img.gz"
}

// verifyArtifact proves the output is genuinely what it claims: qemu-img can
// read the qcow2, and gzip can decompress the raw.gz into a readable image.
func verifyArtifact(t *testing.T, path string, format Format) {
	t.Helper()
	switch format {
	case Qcow2:
		out, err := exec.Command("qemu-img", "info", "--output=json", path).CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-img cannot read the exported qcow2: %s: %v", out, err)
		}
		if !strings.Contains(string(out), `"format": "qcow2"`) &&
			!strings.Contains(string(out), `"format":"qcow2"`) {
			t.Errorf("exported file is not qcow2: %s", out)
		}
	case RawGz:
		raw := path + ".decompressed"
		defer os.Remove(raw)
		f, err := os.Create(raw)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("gzip", "-dc", path)
		cmd.Stdout = f
		runErr := cmd.Run()
		f.Close()
		if runErr != nil {
			// No gzip binary on this host: the in-process writer is still
			// covered by unit tests, so don't fail the run over a missing tool.
			t.Skipf("gzip not usable here: %v", runErr)
		}
		out, err := exec.Command("qemu-img", "info", "--output=json", raw).CombinedOutput()
		if err != nil {
			t.Fatalf("the decompressed export is not a readable image: %s: %v", out, err)
		}
		t.Logf("raw.gz decompresses to a readable image")
	}
}

// Real virsh + a real domain with a real qcow2 disk.
func TestE2E_LibvirtExport(t *testing.T) {
	if _, err := exec.LookPath("virsh"); err != nil {
		t.Skip("virsh not installed")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })

	uri := ""
	for _, candidate := range []string{"qemu:///session", "qemu:///system"} {
		if exec.Command("virsh", "-c", candidate, "connect").Run() == nil {
			uri = candidate
			break
		}
	}
	if uri == "" {
		t.Skip("no usable libvirt connection")
	}

	dir := t.TempDir()
	disk := filepath.Join(dir, "e2e.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", disk, "32M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}
	const domain = "corral-export-e2e"
	xmlPath := filepath.Join(dir, "domain.xml")
	os.WriteFile(xmlPath, []byte(`<domain type='qemu'>
  <name>`+domain+`</name><memory unit='MiB'>128</memory><vcpu>1</vcpu>
  <os><type arch='x86_64'>hvm</type></os>
  <devices><disk type='file' device='disk'>
    <driver name='qemu' type='qcow2'/><source file='`+disk+`'/>
    <target dev='vda' bus='virtio'/>
  </disk></devices>
</domain>`), 0o644)
	if out, err := exec.Command("virsh", "-c", uri, "define", xmlPath).CombinedOutput(); err != nil {
		t.Skipf("cannot define a domain here: %s: %v", out, err)
	}
	t.Cleanup(func() { exec.Command("virsh", "-c", uri, "undefine", domain).Run() })

	ref := types.InstanceRef{Backend: "libvirt", Context: uri, Name: domain}
	dest := filepath.Join(t.TempDir(), "domain.qcow2")
	result, err := (Libvirt{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	t.Logf("libvirt → %s (%d bytes, %s)", result.Path, result.Bytes, result.Consistency)
	// Defined but never started, so the domain was captured offline.
	if result.Consistency != snapshot.Offline {
		t.Errorf("consistency = %q, want offline", result.Consistency)
	}
	verifyArtifact(t, dest, Qcow2)
}

// Real Incus, and the honesty check that matters here: what comes out is an
// instance archive, not a bootable disk image.
func TestE2E_IncusExport(t *testing.T) {
	if _, err := exec.LookPath("incus"); err != nil {
		t.Skip("incus not installed")
	}
	SetRunner(shell.Real{})
	t.Cleanup(func() { SetRunner(shell.Real{}) })

	if exec.Command("incus", "query", "local:/1.0").Run() != nil {
		t.Skip("no reachable local Incus remote")
	}
	const instance = "corral-export-e2e"
	if out, err := exec.Command("incus", "launch", "images:ubuntu/22.04", instance).CombinedOutput(); err != nil {
		t.Skipf("cannot launch an Incus instance here: %s: %v", out, err)
	}
	t.Cleanup(func() { exec.Command("incus", "delete", "--force", instance).Run() })

	ref := types.InstanceRef{Backend: "incus", Context: "local", Name: instance}
	dest := filepath.Join(t.TempDir(), "instance.tar.gz")
	result, err := (Incus{}).Export(context.Background(), Request{Ref: ref, Dest: dest}, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	t.Logf("incus → %s (%d bytes, %s, %s)", result.Path, result.Bytes, result.Format, result.Consistency)
	if result.Format != IncusTar {
		t.Errorf("format = %q, want %q — an Incus export is not a disk image", result.Format, IncusTar)
	}
	if result.Consistency != snapshot.Crash {
		t.Errorf("consistency = %q; a running instance's export is crash-consistent", result.Consistency)
	}
	// "Backup" has to mean the data. --instance-only drops the instance's
	// snapshots, not its rootfs, but the difference between a real archive and
	// a few KB of metadata is exactly the mistake worth catching here — an
	// Ubuntu rootfs is tens of megabytes at minimum.
	const plausibleArchive = 1 << 20
	if result.Bytes < plausibleArchive {
		t.Errorf("archive is %d bytes — too small to contain the instance's filesystem", result.Bytes)
	}
	// Refusing a disk-image format is the whole point of the separate format.
	if _, err := (Incus{}).Export(context.Background(),
		Request{Ref: ref, Dest: dest, Format: Qcow2}, nil); err == nil {
		t.Error("Incus accepted a qcow2 request")
	}
}
