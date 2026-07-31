package plugin

import "testing"

// A v0.0.0 dev/rolling build (Go pseudo-version) must not be gated out of
// plugins that require a real minimum — it carries no meaningful ordering (#).
func TestCompatible_DevVersionExempt(t *testing.T) {
	old := CurrentVersion
	defer func() { CurrentVersion = old }()

	e := &Entry{Name: "bootc", Version: "0.2.0", Corral: ">=0.1.0"}

	for _, v := range []string{
		"v0.0.0-20260730194842-8f48135a42ef", // Go pseudo-version (rolling build)
		"v0.0.0",                             // plain `go build`, no -X cmd.version
		"unknown",
		"git-abcdef",
	} {
		CurrentVersion = v
		if err := e.Compatible(); err != nil {
			t.Errorf("CurrentVersion=%q: expected compatible, got %v", v, err)
		}
	}

	// A real release below the floor (patch!=0, so not the dev exemption) is
	// still correctly rejected.
	CurrentVersion = "v0.0.9"
	if err := e.Compatible(); err == nil {
		t.Errorf("v0.0.9 is below 0.1.0 and should be rejected, got nil")
	}
	CurrentVersion = "v0.4.0"
	if err := e.Compatible(); err != nil {
		t.Errorf("v0.4.0 should satisfy >=0.1.0, got %v", err)
	}
}
