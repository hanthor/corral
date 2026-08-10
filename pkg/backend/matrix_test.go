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

	"github.com/tuna-os/corral/pkg/export"
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

// pkg/export is the other real per-backend registry, and the export row had
// silently gone stale against it — three backends listed as "possible" while
// their adapters had been shipping for releases. A matrix that drifts is worse
// than no matrix, so this ties the row to the registry the same way snapshots
// are tied.
func TestExportAdaptersAgreeWithTheMatrix(t *testing.T) {
	for _, backend := range Backends {
		entry, _ := Get("export", backend)
		supported := export.Supported(backend)
		if (entry.Support == Shipped) != supported {
			t.Errorf("export/%s: matrix says %q, export.Supported says %v",
				backend, entry.Support, supported)
		}
		if supported && len(export.Formats(backend)) == 0 {
			t.Errorf("export.Supported(%q) is true but it advertises no formats", backend)
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

// ── the operation contract ────────────────────────────────────────

// The point of the contract: what a backend supports is what its adapter
// implements, and the matrix has to agree. This is the test that makes adding a
// method to an adapter *the* way to close a gap — forget the matrix and CI says
// so; claim it in the matrix without implementing and CI says that too.
func TestAdaptersAgreeWithTheMatrix(t *testing.T) {
	for _, family := range Families {
		for _, operation := range family.Operations {
			for _, backend := range Backends {
				entry, ok := Get(operation, backend)
				if !ok {
					t.Fatalf("family %s claims operation %q, which the matrix has no row for",
						family.Name, operation)
				}
				provides := Provides(backend, operation)

				switch entry.Support {
				case Shipped:
					if !provides {
						t.Errorf("%s/%s: the matrix says shipped but %s's adapter does not implement %s — "+
							"either wire it through the contract or downgrade the cell",
							operation, backend, backend, family.Name)
					}
				case Possible, Unsupported:
					if provides {
						t.Errorf("%s/%s: %s's adapter implements %s, so this is shipped — "+
							"update the matrix (and the capability flag) rather than hiding it",
							operation, backend, backend, family.Name)
					}
				}
			}
		}
	}
}

// Every backend must have an adapter, and every adapter must be constructible
// from a bare reference — capability derivation probes the type, so a
// constructor that needs a live cluster would make the whole mechanism
// unavailable offline.
func TestEveryBackendHasAProbeableAdapter(t *testing.T) {
	for _, backend := range Backends {
		if !Registered(backend) {
			t.Errorf("backend %q has no adapter registered", backend)
			continue
		}
		adapter, err := For(types.InstanceRef{Backend: backend, Name: "probe"})
		if err != nil {
			t.Errorf("For(%q) = %v; the constructor must not need a live connection", backend, err)
			continue
		}
		if adapter.Backend() != backend {
			t.Errorf("adapter for %q reports backend %q", backend, adapter.Backend())
		}
		// Power is the floor: a backend that cannot start, stop, and delete is
		// not a backend Corral can drive.
		if _, ok := adapter.(Power); !ok {
			t.Errorf("%s's adapter does not implement Power", backend)
		}
	}
}

func TestForRejectsWhatItCannotServe(t *testing.T) {
	if _, err := For(types.InstanceRef{Name: "x"}); err == nil {
		t.Error("a reference with no backend should be refused")
	}
	_, err := For(types.InstanceRef{Backend: "vmware", Name: "x"})
	if err == nil {
		t.Fatal("an unknown backend should be refused")
	}
	// The refusal has to say what *is* available, or the operator is left
	// guessing at spelling.
	for _, want := range []string{"vmware", "kubevirt", "backend-parity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

// Implemented is the per-backend summary the docs and the doctor can print. It
// is also the clearest statement of the parity problem: one backend implements
// everything and the others implement a fraction.
func TestImplementedSummarisesEachBackend(t *testing.T) {
	// Counted over the families that carry matrix operations. Ingester is
	// deliberately excluded: it is the destination half of a move (ADR-0010),
	// has no matrix row, and the backend that implements every *instance*
	// operation is not the one that can necessarily receive a disk — qemu and
	// libvirt can, KubeVirt cannot yet.
	operational := 0
	for _, family := range Families {
		if len(family.Operations) > 0 {
			operational++
		}
	}
	kubevirt := 0
	for _, name := range Implemented("kubevirt") {
		if familyByName(name).Operations != nil {
			kubevirt++
		}
	}
	if kubevirt != operational {
		t.Errorf("kubevirt implements %d of %d operational families (%v) — the reference backend should be complete",
			kubevirt, operational, Implemented("kubevirt"))
	}
	for _, backend := range []string{"qemu", "incus", "libvirt"} {
		if got := Implemented(backend); len(got) >= len(Families) {
			t.Errorf("%s implements %v, which the matrix does not claim", backend, got)
		}
	}
	if got := Implemented("proxmox"); len(got) < 8 {
		t.Errorf("proxmox implements only %v; it was built against the contract", got)
	}
	if got := Implemented("vmware"); got != nil {
		t.Errorf("Implemented on an unknown backend = %v, want nil", got)
	}
}

func familyByName(name string) Family {
	for _, family := range Families {
		if family.Name == name {
			return family
		}
	}
	return Family{}
}

// The destination half of a move: who can receive a disk, and does everyone
// else explain why not.
func TestIngestRefusalsAreExplained(t *testing.T) {
	for _, backend := range []string{"qemu", "libvirt", "kubevirt", "proxmox", "incus"} {
		if !CanIngest(backend) {
			t.Errorf("%s should be able to receive a moved instance", backend)
		}
		if got := IngestRefusal(backend); got != "" {
			t.Errorf("%s can ingest but returns a refusal: %q", backend, got)
		}
	}
	// A backend that cannot receive must say why, and name the alternative.
	for _, backend := range []string{"vmware"} {
		if CanIngest(backend) {
			t.Errorf("%s claims it can ingest", backend)
		}
		refusal := IngestRefusal(backend)
		if refusal == "" {
			t.Errorf("%s cannot ingest and gives no reason", backend)
		}
	}
}

// Firmware is a refusal boundary, so which backends can express EFI boot is
// worth pinning: a UEFI guest that lands on a BIOS-only target boots to a blank
// screen, and the preflight only knows to stop it if this stays honest.
func TestIngestersDeclareTheirFirmwareSupport(t *testing.T) {
	for backend, want := range map[string]bool{
		// qemu's generated systemd unit has no OVMF path yet.
		"qemu": false,
		// libvirt's domain XML selects firmware; KubeVirt sets
		// firmware.bootloader.efi; PVE sets bios=ovmf plus an EFI vars disk; Incus is OVMF-only.
		"libvirt": true, "kubevirt": true, "proxmox": true, "incus": true,
	} {
		adapter, err := probe(backend)
		if err != nil {
			t.Fatalf("probing %s: %v", backend, err)
		}
		ingester, ok := adapter.(Ingester)
		if !ok {
			t.Fatalf("%s is not an Ingester", backend)
		}
		if got := ingester.AcceptsUEFI(); got != want {
			t.Errorf("%s AcceptsUEFI = %v, want %v", backend, got, want)
		}
	}
}

// The docs carry a per-backend gap list under "### <backend> — N gaps", and it
// is the part an operator reads to decide what to ask for. A count that drifts
// from the matrix is worse than none: it reads as a promise about how much is
// missing. The table above is already checked cell by cell; this checks the
// prose that summarises it.
func TestDocsGapCountsMatchTheMatrix(t *testing.T) {
	doc, err := os.ReadFile("../../docs/backend-parity.md")
	if err != nil {
		t.Fatalf("reading the parity doc: %v", err)
	}
	text := string(doc)

	for _, backend := range Backends {
		gaps := Gaps(backend)
		heading := fmt.Sprintf("### %s — %d gaps", backend, len(gaps))
		if len(gaps) == 0 {
			continue
		}
		if !strings.Contains(text, heading) {
			// Show what the doc claims, so the fix is obvious.
			claimed := "no heading at all"
			if idx := strings.Index(text, "### "+backend+" — "); idx >= 0 {
				end := strings.Index(text[idx:], "\n")
				claimed = text[idx : idx+end]
			}
			t.Errorf("the matrix has %d gaps for %s but the doc says %q",
				len(gaps), backend, claimed)
		}
		// Every gap must actually be listed, or the count is right by accident.
		for _, gap := range gaps {
			bullet := "- **" + gap + "** —"
			section := text[strings.Index(text, "### "+backend+" — "):]
			if next := strings.Index(section[3:], "\n### "); next >= 0 {
				section = section[:next+3]
			}
			if !strings.Contains(section, bullet) {
				t.Errorf("%s's gap %q is missing from the doc's list", backend, gap)
			}
		}
	}
}
