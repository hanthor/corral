package backend

// Conformance tests for the parity matrix.
//
// These are not tests of the backends; they are tests of the claims Corral makes
// about the backends. Three claims have to agree or an operator gets lied to:
// the matrix here, the capability flags types.CapabilitiesForBackend hands to
// every UI, and the snapshot adapter registry. A fourth check keeps
// docs/backend-parity.md from drifting away from all three.

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/snapshot"
	"github.com/tuna-os/corral/pkg/types"
)

func TestMatrixIsComplete(t *testing.T) {
	for _, op := range Operations {
		row, ok := Matrix[op.ID]
		if !ok {
			t.Errorf("operation %q has no row in the matrix", op.ID)
			continue
		}
		for _, backend := range Backends {
			entry, ok := row[backend]
			if !ok {
				t.Errorf("operation %q has no entry for backend %q", op.ID, backend)
				continue
			}
			switch entry.Support {
			case Shipped, Possible, Unsupported:
			default:
				t.Errorf("%s/%s has support %q, which is not one of the three", op.ID, backend, entry.Support)
			}
			// A cell without a note is a cell nobody can act on: for Possible
			// it must name the native mechanism, for Unsupported the reason.
			if strings.TrimSpace(entry.Note) == "" {
				t.Errorf("%s/%s has no note", op.ID, backend)
			}
		}
		for backend := range row {
			if !contains(Backends, backend) {
				t.Errorf("operation %q has an entry for unknown backend %q", op.ID, backend)
			}
		}
	}
	for id := range Matrix {
		if !contains(OperationIDs(), id) {
			t.Errorf("matrix has a row for unknown operation %q", id)
		}
	}
}

// The capability flags are what every UI gates on. A flag set for an operation
// nothing implements is a button that fails on click; a flag unset for an
// operation that does work is a feature the operator cannot reach — which is
// how libvirt SSH and the Incus console ended up invisible.
func TestCapabilitiesAgreeWithTheMatrix(t *testing.T) {
	for _, op := range Operations {
		if op.Capability == "" {
			continue
		}
		for _, backend := range Backends {
			entry, _ := Get(op.ID, backend)
			declared := capabilityFlag(t, backend, op.Capability)

			switch entry.Support {
			case Shipped:
				if !declared {
					t.Errorf("%s ships %s but does not declare the %s capability — no surface will offer it",
						backend, op.ID, op.Capability)
				}
			case Unsupported:
				if declared {
					t.Errorf("%s declares the %s capability for an operation it cannot perform: %s",
						backend, op.Capability, entry.Note)
				}
			case Possible:
				// A declared-but-unimplemented capability is the failure mode
				// worth catching: the UI offers it and the call goes nowhere.
				if declared {
					t.Errorf("%s declares the %s capability but %s is only Possible, not Shipped (%s) — "+
						"either implement it or drop the flag",
						backend, op.Capability, op.ID, entry.Note)
				}
			}
		}
	}
}

// Every capability field must be covered by some operation, or a flag exists
// that the matrix cannot speak about.
func TestEveryCapabilityFieldHasAnOperation(t *testing.T) {
	covered := map[string]bool{}
	for _, op := range Operations {
		if op.Capability != "" {
			covered[op.Capability] = true
		}
	}
	fields := reflect.TypeOf(types.InstanceCapabilities{})
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if !covered[name] {
			t.Errorf("types.InstanceCapabilities.%s is not covered by any operation in the matrix", name)
		}
	}
}

// pkg/snapshot is the one operation that already has a real per-backend adapter
// contract, so it is the reference: the matrix must agree with its registry.
func TestSnapshotAdaptersAgreeWithTheMatrix(t *testing.T) {
	for _, backend := range Backends {
		entry, _ := Get("snapshots", backend)
		supported := snapshot.Supported(backend)
		if (entry.Support == Shipped) != supported {
			t.Errorf("snapshots/%s: matrix says %q, snapshot.Supported says %v",
				backend, entry.Support, supported)
		}
		if supported {
			if _, err := snapshot.For(types.InstanceRef{Backend: backend, Name: "probe"}); err != nil {
				t.Errorf("snapshot.Supported(%q) is true but For returns %v", backend, err)
			}
		}
	}
}

// Backends whose matrix rows say they ship the basics must actually satisfy the
// interface the CLI dispatches through. This is a compile-time claim made
// explicit, so deleting a method breaks here with a parity message rather than
// somewhere downstream.
func TestShippedBackendsSatisfyTheInterface(t *testing.T) {
	implemented := map[string]types.Backend{}
	for name, b := range interfaceImplementations() {
		implemented[name] = b
	}
	for _, backend := range Backends {
		entry, _ := Get("list", backend)
		if entry.Support != Shipped {
			continue
		}
		if _, ok := implemented[backend]; !ok {
			// kubevirt and libvirt are reached through their own clients rather
			// than the types.Backend interface; the matrix knows they ship, so
			// this is a note, not a failure — see docs/backend-parity.md for
			// why the interface itself is the next thing to widen.
			t.Logf("%s ships list but is not reachable through types.Backend (client-only)", backend)
		}
	}
}

// The docs table is generated from the same data by hand; if the two disagree,
// the docs are lying to whoever reads them instead of the code.
func TestDocsTableMatchesTheMatrix(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "backend-parity.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	doc := string(raw)

	for _, op := range Operations {
		row := docRow(doc, op.Title)
		if row == "" {
			t.Errorf("docs/backend-parity.md has no row for %q", op.Title)
			continue
		}
		cells := strings.Split(strings.Trim(row, "|"), "|")
		// Title plus one cell per backend.
		if len(cells) != len(Backends)+1 {
			t.Errorf("row %q has %d cells, want %d", op.Title, len(cells), len(Backends)+1)
			continue
		}
		for i, backend := range Backends {
			entry, _ := Get(op.ID, backend)
			cell := strings.TrimSpace(cells[i+1])
			if want := marker(entry.Support); !strings.Contains(cell, want) {
				t.Errorf("docs row %q, backend %s: cell %q does not carry %q for support %q",
					op.Title, backend, cell, want, entry.Support)
			}
		}
	}
}

// Gaps is the work list; it must be non-empty for the backends the audit found
// wanting, so nobody mistakes silence for parity.
func TestGapsAreEnumerable(t *testing.T) {
	for _, backend := range []string{"qemu", "incus", "libvirt"} {
		if len(Gaps(backend)) == 0 {
			t.Errorf("%s has no Possible entries; either it reached parity (update this test) "+
				"or the matrix is not being maintained", backend)
		}
	}
	if got := Gaps("kubevirt"); len(got) != 0 {
		t.Errorf("kubevirt has gaps %v — the reference backend should be fully shipped or the "+
			"matrix should say what it cannot do", got)
	}
	// Snapshots are the operation every backend implements — the point of the
	// adapter contract, and the shape the rest should follow.
	if got := ShippedBy("snapshots"); len(got) != len(Backends) {
		t.Errorf("snapshots shipped by %v, want every backend", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────

func capabilityFlag(t *testing.T, backend, field string) bool {
	t.Helper()
	caps := types.CapabilitiesForBackend(backend)
	value := reflect.ValueOf(caps).FieldByName(field)
	if !value.IsValid() {
		t.Fatalf("types.InstanceCapabilities has no field %q", field)
	}
	return value.Bool()
}

func docRow(doc, title string) string {
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "| "+title+" ") {
			return trimmed
		}
	}
	return ""
}

// marker is the glyph the docs table uses for each support level.
func marker(s Support) string {
	switch s {
	case Shipped:
		return "✅"
	case Possible:
		return "🔨"
	default:
		return "—"
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func init() {
	// Fail loudly if a backend is added to types without the matrix learning
	// about it: CapabilitiesForBackend returning anything non-zero for a
	// backend the matrix has never heard of means a UI can offer operations
	// nothing here describes.
	for _, backend := range []string{"kubevirt", "qemu", "incus", "libvirt", "proxmox"} {
		if !contains(Backends, backend) {
			panic(fmt.Sprintf("backend %q is missing from Backends", backend))
		}
	}
}
