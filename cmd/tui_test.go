package cmd

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/tuna-os/corral/pkg/demo"
	"github.com/tuna-os/corral/pkg/types"
)

func TestTUI_ModelInitializationAndNavigation(t *testing.T) {
	demo.Enable()

	m := newTUIModel()
	if m.state != "list" {
		t.Fatalf("initial state = %q, want 'list'", m.state)
	}

	// Verify items loaded in demo mode (VMs, CTs, and Incus instances)
	items := m.list.Items()
	if len(items) == 0 {
		t.Fatal("expected items in demo mode TUI, got 0")
	}

	hasIncus := false
	for _, item := range items {
		if vi, ok := item.(vmItem); ok && vi.vm.Backend == "incus" {
			hasIncus = true
			break
		}
	}
	if !hasIncus {
		t.Error("expected Incus demo instance to be present in TUI items")
	}

	// Test pressing Enter on selected item opens actions menu
	newModel, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	updated := newModel.(tuiModel)
	if updated.state != "actions" {
		t.Errorf("state after Enter = %q, want 'actions'", updated.state)
	}

	// Test Esc key returns to list state
	backModel, _ := updated.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	backUpdated := backModel.(tuiModel)
	if backUpdated.state != "list" {
		t.Errorf("state after Esc = %q, want 'list'", backUpdated.state)
	}

	// Test Doctor view navigation ('d' key)
	docModel, _ := backUpdated.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	docUpdated := docModel.(tuiModel)
	if docUpdated.state != "doctor" {
		t.Errorf("state after 'd' = %q, want 'doctor'", docUpdated.state)
	}
}

func TestTUI_ActionApplicability(t *testing.T) {
	runningVM := types.VM{Name: "run-vm", Backend: "kubevirt", Status: "Running", Running: true}
	stoppedVM := types.VM{Name: "stop-vm", Backend: "kubevirt", Status: "Stopped", Running: false}
	incusVM := types.VM{Name: "inc-vm", Backend: "incus", Status: "Running", Running: true}

	if !actionApplies("stop", runningVM) {
		t.Error("expected 'stop' action to apply to running VM")
	}
	if actionApplies("start", runningVM) {
		t.Error("expected 'start' action NOT to apply to running VM")
	}
	if !actionApplies("start", stoppedVM) {
		t.Error("expected 'start' action to apply to stopped VM")
	}
	if actionApplies("migrate", incusVM) {
		t.Error("expected 'migrate' action NOT to apply to Incus VM")
	}
}

func TestTUI_RenderView(t *testing.T) {
	demo.Enable()
	m := newTUIModel()
	m.width = 120
	m.height = 40

	rendered := m.render()
	if !strings.Contains(rendered, "web-prod") {
		t.Errorf("TUI View() output missing expected VM item: %s", rendered)
	}
}

func TestTUI_NarrowTerminalWidth(t *testing.T) {
	demo.Enable()
	m := newTUIModel()

	// Simulate narrow terminal window (e.g. 40 columns)
	newModel, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	updated := newModel.(tuiModel)

	if updated.width != 40 || updated.height != 24 {
		t.Errorf("expected recorded window size 40x24, got %dx%d", updated.width, updated.height)
	}

	rendered := updated.render()
	if rendered == "" {
		t.Error("expected non-empty render output for narrow terminal")
	}
}
