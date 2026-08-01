package cmd

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/export"
	"github.com/tuna-os/corral/pkg/move"
	"github.com/tuna-os/corral/pkg/types"
)

func samplePlan() move.Plan {
	return move.Plan{
		Source:      types.InstanceRef{Backend: "kubevirt", Name: "web-1"},
		Destination: types.InstanceRef{Backend: "qemu", Name: "web-1"},
		Format:      export.Qcow2,
		Steps: []move.Step{
			{Name: "preflight", Detail: "check everything"},
			{Name: "export", Detail: "write the disk"},
		},
		Warnings: []string{"the guest gets a new IP"},
		Dropped:  []string{"node placement"},
	}
}

func TestPrintPlanShowsStepsWarningsAndDrops(t *testing.T) {
	out := captureStdout(t, func() { printPlan(samplePlan()) })
	for _, want := range []string{
		"kubevirt/web-1 → qemu/web-1",
		"1. preflight",
		"2. export",
		"the guest gets a new IP",
		"Not carried over:",
		"node placement",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Refused:") {
		t.Errorf("a plan with no refusals should not print a refusal section:\n%s", out)
	}
}

func TestPrintPlanShowsRefusalsWithTheirRemedy(t *testing.T) {
	plan := samplePlan()
	plan.Refusals = []move.Refusal{{
		Reason: "the guest boots via UEFI and the qemu backend has no firmware path",
		Remedy: "move it to a backend that can express EFI boot",
	}}
	out := captureStdout(t, func() { printPlan(plan) })
	if !strings.Contains(out, "Refused:") || !strings.Contains(out, "boots via UEFI") {
		t.Errorf("the refusal must be printed:\n%s", out)
	}
	if !strings.Contains(out, "express EFI boot") {
		t.Errorf("and so must the remedy — a refusal without one is a dead end:\n%s", out)
	}
}

func TestMoveRequiresADestinationBackend(t *testing.T) {
	saved := moveTo
	t.Cleanup(func() { moveTo = saved })
	moveTo = ""

	err := moveCmd.RunE(moveCmd, []string{"web-1"})
	if err == nil || !strings.Contains(err.Error(), "--to is required") {
		t.Fatalf("expected a --to error before any lookup, got %v", err)
	}
}

// TestMoveCommandIsRegisteredAndDistinctFromMigrate guards the ADR-0010 naming
// decision: `move` is cold and cross-backend, `migrate` is live and within one
// backend. Collapsing them would stop someone's production VM.
func TestMoveCommandIsRegisteredAndDistinctFromMigrate(t *testing.T) {
	var found, foundMigrate bool
	for _, c := range rootCmd.Commands() {
		switch c.Name() {
		case "move":
			found = true
			if !strings.Contains(c.Short, "stops") {
				t.Errorf("`move`'s one-line help must say the guest stops, got %q", c.Short)
			}
			if !strings.Contains(c.Long, "corral migrate") {
				t.Error("`move`'s help should point at `migrate` for the live, same-backend case")
			}
		case "migrate":
			foundMigrate = true
		}
	}
	if !found {
		t.Fatal("`corral move` is not registered")
	}
	if !foundMigrate {
		t.Fatal("`corral migrate` disappeared; the two verbs are supposed to coexist")
	}
}

func TestMoveFlagDefaultsKeepTheSource(t *testing.T) {
	flag := moveCmd.Flags().Lookup("delete-source")
	if flag == nil {
		t.Fatal("--delete-source is missing")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--delete-source defaults to %q; deleting a source before the ingest is proven is unrecoverable", flag.DefValue)
	}
	if moveCmd.Flags().Lookup("dry-run") == nil {
		t.Error("--dry-run is missing, so there is no way to read the preflight without committing")
	}
}
