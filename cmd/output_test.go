package cmd

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/types"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

func TestPrintJSON_EncodesMachineReadableOutput(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printJSON([]types.VM{{Name: "web", Backend: "qemu"}}); err != nil {
			t.Fatal(err)
		}
	})
	var vms []types.VM
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(vms) != 1 || vms[0].Name != "web" {
		t.Fatalf("unexpected JSON output: %+v", vms)
	}
}

func TestPrintCatalogJSON_UsesCatalogCollections(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printCatalogJSON("os", []catalog.Image{{Name: "fedora"}}, nil); err != nil {
			t.Fatal(err)
		}
	})
	var images []catalog.Image
	if err := json.Unmarshal([]byte(out), &images); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(images) != 1 || images[0].Name != "fedora" {
		t.Fatalf("unexpected catalog JSON: %+v", images)
	}
}

func TestPrintCatalog_ListsEachImage(t *testing.T) {
	images := []catalog.Image{
		{Name: "fedora-42", DefaultUser: "fedora", Source: "quay.io/containerdisks", Description: "Fedora 42", ContainerDisk: "quay.io/containerdisks/fedora:42"},
		{Name: "debian-12", DefaultUser: "debian", Source: "debian.org", Description: "Debian 12", URL: "https://cloud.debian.org/x.qcow2"},
	}
	out := captureStdout(t, func() { printCatalog(images) })

	if !strings.Contains(out, "fedora-42") || !strings.Contains(out, "Fedora 42") {
		t.Errorf("missing fedora-42 row in output:\n%s", out)
	}
	if !strings.Contains(out, "debian-12") || !strings.Contains(out, "Debian 12") {
		t.Errorf("missing debian-12 row in output:\n%s", out)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "DESCRIPTION") {
		t.Errorf("missing header row in output:\n%s", out)
	}
}

func TestPrintCatalog_Empty(t *testing.T) {
	out := captureStdout(t, func() { printCatalog(nil) })
	if !strings.Contains(out, "NAME") {
		t.Errorf("expected header row even with no images, got:\n%s", out)
	}
}

func TestPrintVMList_ShowsAllVMs(t *testing.T) {
	vms := []types.VM{
		{Name: "web", Backend: "kubevirt", Namespace: "corral-vms", Ready: true, Running: true, CPU: 2, Mem: "4Gi"},
		{Name: "db", Backend: "qemu", Ready: false, Running: false, CPU: 1, Mem: "2G"},
	}
	out := captureStdout(t, func() { printVMList(vms) })

	if !strings.Contains(out, "web") || !strings.Contains(out, "kubevirt") {
		t.Errorf("missing web/kubevirt row:\n%s", out)
	}
	if !strings.Contains(out, "db") || !strings.Contains(out, "qemu") {
		t.Errorf("missing db/qemu row:\n%s", out)
	}
}

func TestPrintVMList_SortsByName(t *testing.T) {
	vms := []types.VM{
		{Name: "zebra", Backend: "qemu"},
		{Name: "apple", Backend: "qemu"},
	}
	out := captureStdout(t, func() { printVMList(vms) })

	appleIdx := strings.Index(out, "apple")
	zebraIdx := strings.Index(out, "zebra")
	if appleIdx == -1 || zebraIdx == -1 || appleIdx > zebraIdx {
		t.Errorf("expected apple before zebra (sorted), got:\n%s", out)
	}
}

func TestPrintVMList_StatusIcons(t *testing.T) {
	vms := []types.VM{
		{Name: "running-vm", Backend: "kubevirt", Ready: true, Running: true},
		{Name: "starting-vm", Backend: "kubevirt", Ready: false, Running: true},
		{Name: "stopped-vm", Backend: "kubevirt", Ready: false, Running: false},
	}
	out := captureStdout(t, func() { printVMList(vms) })

	if !strings.Contains(out, "Running") {
		t.Error("expected a Running status for the ready+running VM")
	}
	if !strings.Contains(out, "Starting") {
		t.Error("expected a Starting status for the running-but-not-ready VM")
	}
	if !strings.Contains(out, "Stopped") {
		t.Error("expected a Stopped status for the not-running VM")
	}
}

func TestPrintVMList_MissingNamespaceAndNode(t *testing.T) {
	vms := []types.VM{{Name: "orphan", Backend: "kubevirt"}}
	out := captureStdout(t, func() { printVMList(vms) })
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash placeholders for empty namespace/node, got:\n%s", out)
	}
}
