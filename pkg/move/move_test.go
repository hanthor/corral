package move

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/backend"
	"github.com/tuna-os/corral/pkg/export"
	"github.com/tuna-os/corral/pkg/types"
)

// ── harness ───────────────────────────────────────────────────────

// stub replaces every seam with a recorded fake and restores the real ones when
// the test ends. Preflight and Execute are otherwise untestable without a
// hypervisor, and the point of the seams is that the composition is what has
// bugs, not the adapters.
type stub struct {
	ingestable map[string]bool
	uefiOK     map[string]bool
	formats    map[string][]export.Format
	free       int64
	freeErr    error

	exportErr  error
	exportedTo string
	ingestErr  error
	stopErr    error
	deleteErr  error

	stopped  []string
	deleted  []string
	ingested []types.InstanceRef
	shapes   []backend.Shape
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{
		ingestable: map[string]bool{"qemu": true, "libvirt": true, "kubevirt": true, "proxmox": true},
		uefiOK:     map[string]bool{"libvirt": true, "kubevirt": true, "proxmox": true},
		formats: map[string][]export.Format{
			"kubevirt": {export.RawGz, export.Qcow2},
			"qemu":     {export.Qcow2, export.RawGz},
			"libvirt":  {export.Qcow2, export.RawGz},
			"incus":    {export.IncusTar, export.Qcow2},
		},
		free: 1 << 40,
	}

	saved := struct {
		exportDisk   func(context.Context, export.Request, export.ProgressFunc) (export.Result, error)
		ingestDisk   func(types.InstanceRef, string, backend.Shape) error
		powerOff     func(types.InstanceRef) error
		removeSource func(types.InstanceRef) error
		canIngest    func(string) bool
		ingestReason func(string) string
		acceptsUEFI  func(types.InstanceRef) bool
		formatsFor   func(string) []export.Format
		freeSpace    func(string) (int64, error)
	}{exportDisk, ingestDisk, powerOff, removeSource, canIngest, ingestReason, acceptsUEFI, formatsFor, freeSpace}
	t.Cleanup(func() {
		exportDisk, ingestDisk, powerOff, removeSource = saved.exportDisk, saved.ingestDisk, saved.powerOff, saved.removeSource
		canIngest, ingestReason, acceptsUEFI = saved.canIngest, saved.ingestReason, saved.acceptsUEFI
		formatsFor, freeSpace = saved.formatsFor, saved.freeSpace
	})

	canIngest = func(b string) bool { return s.ingestable[b] }
	ingestReason = func(b string) string { return "stub: " + b + " has no ingest path" }
	acceptsUEFI = func(ref types.InstanceRef) bool { return s.uefiOK[ref.Backend] }
	formatsFor = func(b string) []export.Format { return s.formats[b] }
	freeSpace = func(string) (int64, error) { return s.free, s.freeErr }

	exportDisk = func(_ context.Context, req export.Request, progress export.ProgressFunc) (export.Result, error) {
		if s.exportErr != nil {
			return export.Result{}, s.exportErr
		}
		// Write something real: Execute removes the artifact afterwards, and a
		// test that never creates it cannot notice if it stops doing so.
		if err := os.WriteFile(req.Dest, []byte("disk"), 0o644); err != nil {
			return export.Result{}, err
		}
		s.exportedTo = req.Dest
		progress(export.Progress{Stage: "exporting", Done: 4, Total: 4})
		return export.Result{Path: req.Dest, Format: req.Format, Bytes: 4}, nil
	}
	ingestDisk = func(ref types.InstanceRef, disk string, shape backend.Shape) error {
		if s.ingestErr != nil {
			return s.ingestErr
		}
		if _, err := os.Stat(disk); err != nil {
			return fmt.Errorf("ingest was handed a disk that is not there: %w", err)
		}
		s.ingested = append(s.ingested, ref)
		s.shapes = append(s.shapes, shape)
		return nil
	}
	powerOff = func(ref types.InstanceRef) error {
		if s.stopErr != nil {
			return s.stopErr
		}
		s.stopped = append(s.stopped, ref.Name)
		return nil
	}
	removeSource = func(ref types.InstanceRef) error {
		if s.deleteErr != nil {
			return s.deleteErr
		}
		s.deleted = append(s.deleted, ref.Name)
		return nil
	}
	return s
}

func sourceVM() Source {
	return Source{
		VM: types.VM{
			Name:      "web-1",
			Backend:   "kubevirt",
			Context:   "prod",
			Namespace: "default",
			CPU:       4,
			Mem:       "8Gi",
			Disk:      "20Gi",
			Tags:      []string{"team-a"},
		},
		OSType: "linux",
	}
}

func to(backend string) Target { return Target{Backend: backend, Scratch: os.TempDir()} }

func refusalText(p Plan) string {
	var b strings.Builder
	for _, r := range p.Refusals {
		b.WriteString(r.String())
		b.WriteString("\n")
	}
	return b.String()
}

func mustRefuse(t *testing.T, p Plan, substr string) {
	t.Helper()
	if p.OK() {
		t.Fatalf("expected a refusal mentioning %q, but the plan was accepted", substr)
	}
	if !strings.Contains(refusalText(p), substr) {
		t.Fatalf("expected a refusal mentioning %q, got:\n%s", substr, refusalText(p))
	}
}

func warningText(p Plan) string { return strings.Join(p.Warnings, "\n") }

// ── planning ──────────────────────────────────────────────────────

func TestPreflightAcceptsASupportedPair(t *testing.T) {
	newStub(t)
	plan := Preflight(sourceVM(), to("qemu"))
	if !plan.OK() {
		t.Fatalf("kubevirt → qemu should be supported, got:\n%s", refusalText(plan))
	}
	if plan.Format != export.Qcow2 {
		t.Errorf("format = %q, want qcow2 (the only artifact the ingest path reads)", plan.Format)
	}
	if plan.Destination.Name != "web-1" {
		t.Errorf("destination name = %q, want the source's name when --name is not given", plan.Destination.Name)
	}
	if plan.Shape.CPU != 4 || plan.Shape.Mem != "8Gi" || plan.Shape.Disk != "20Gi" {
		t.Errorf("shape did not carry the source's sizing: %+v", plan.Shape)
	}
	if plan.EstimatedBytes != 20<<30 {
		t.Errorf("EstimatedBytes = %d, want 20GiB", plan.EstimatedBytes)
	}
}

func TestPreflightRenamesTheDestinationWhenAsked(t *testing.T) {
	newStub(t)
	target := to("qemu")
	target.Name = "web-1-local"
	plan := Preflight(sourceVM(), target)
	if plan.Destination.Name != "web-1-local" {
		t.Fatalf("destination name = %q, want web-1-local", plan.Destination.Name)
	}
	if plan.Source.Name != "web-1" {
		t.Fatalf("renaming the destination must not rename the source, got %q", plan.Source.Name)
	}
}

func TestPreflightRefusesTheSameBackendAndPointsAtMigrate(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Backend = "qemu"
	src.VM.Context = ""
	mustRefuse(t, Preflight(src, to("qemu")), "corral migrate")
}

func TestPreflightAllowsTheSameBackendInADifferentContext(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Backend = "libvirt"
	src.VM.Context = "lab"
	target := to("libvirt")
	target.Context = "dc1"
	if plan := Preflight(src, target); !plan.OK() {
		t.Fatalf("libvirt/lab → libvirt/dc1 is a real move, got:\n%s", refusalText(plan))
	}
}

func TestPreflightRefusesContainers(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.Container = true
	mustRefuse(t, Preflight(src, to("qemu")), "containers cannot be moved")
}

func TestPreflightRefusesIncusAsADestination(t *testing.T) {
	newStub(t)
	plan := Preflight(sourceVM(), to("incus"))
	mustRefuse(t, plan, "cannot receive a moved instance")
	if !strings.Contains(refusalText(plan), "stub: incus") {
		t.Errorf("the refusal should carry the backend's own explanation, got:\n%s", refusalText(plan))
	}
}

// Incus is a source now that its export adapter can produce a qcow2 of the
// boot disk. It is still not a destination — those are different problems.
func TestPreflightAllowsIncusAsASource(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Backend = "incus"
	plan := Preflight(src, to("qemu"))
	if !plan.OK() {
		t.Fatalf("incus → qemu should be allowed:\n%s", refusalText(plan))
	}
	if plan.Format != export.Qcow2 {
		t.Errorf("format = %q, want qcow2 — the archive is not bootable elsewhere", plan.Format)
	}
}

// A backend whose only artifact is its own archive still cannot be a source,
// and the refusal has to say why rather than failing at qemu-img.
func TestPreflightRefusesASourceWhoseExportIsNotADisk(t *testing.T) {
	s := newStub(t)
	s.formats["incus"] = []export.Format{export.IncusTar}
	src := sourceVM()
	src.VM.Backend = "incus"
	mustRefuse(t, Preflight(src, to("qemu")), "not a disk image")
}

func TestPreflightRefusesABackendThatCannotExport(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Backend = "proxmox" // not in export.Supported today
	mustRefuse(t, Preflight(src, to("qemu")), "cannot export a disk")
}

func TestPreflightRefusesUEFIOntoABIOSOnlyDestination(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.UEFI = true

	plan := Preflight(src, to("qemu"))
	mustRefuse(t, plan, "boots via UEFI")

	if plan := Preflight(src, to("libvirt")); !plan.OK() {
		t.Fatalf("libvirt can express EFI boot, so this should be allowed:\n%s", refusalText(plan))
	}
}

func TestPreflightRefusesWhenScratchIsTooSmall(t *testing.T) {
	s := newStub(t)
	s.free = 1 << 20 // 1MiB against a 20GiB disk
	mustRefuse(t, Preflight(sourceVM(), to("qemu")), "free and the disk needs up to")
}

func TestPreflightSkipsTheSpaceCheckWhenFreeSpaceIsUnknown(t *testing.T) {
	s := newStub(t)
	s.free, s.freeErr = 0, errors.New("statfs: no such file or directory")
	if plan := Preflight(sourceVM(), to("qemu")); !plan.OK() {
		t.Fatalf("an unreadable scratch filesystem must not refuse the move:\n%s", refusalText(plan))
	}
}

func TestPreflightReportsEveryRefusalAtOnce(t *testing.T) {
	s := newStub(t)
	s.free = 1 << 20
	src := sourceVM()
	src.UEFI = true
	src.Container = true

	plan := Preflight(src, to("qemu"))
	if len(plan.Refusals) < 3 {
		t.Fatalf("an operator should learn about all of them at once, got %d:\n%s",
			len(plan.Refusals), refusalText(plan))
	}
	for _, want := range []string{"containers cannot be moved", "boots via UEFI", "free and the disk needs up to"} {
		if !strings.Contains(refusalText(plan), want) {
			t.Errorf("missing refusal %q in:\n%s", want, refusalText(plan))
		}
	}
}

func TestPreflightRefusesAnEmptyDestinationBackend(t *testing.T) {
	newStub(t)
	mustRefuse(t, Preflight(sourceVM(), Target{}), "no destination backend")
}

func TestPreflightAlwaysWarnsAboutTheAddressChange(t *testing.T) {
	newStub(t)
	plan := Preflight(sourceVM(), to("qemu"))
	if !strings.Contains(warningText(plan), "new MAC address") {
		t.Fatalf("the address change must always be said, got:\n%s", warningText(plan))
	}
	if !plan.OK() {
		t.Fatalf("and it must never be a refusal:\n%s", refusalText(plan))
	}
}

func TestPreflightWarnsAboutWindowsDiskBusses(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.OSType = "windows"
	if !strings.Contains(warningText(Preflight(src, to("qemu"))), "virtio drivers") {
		t.Fatal("a Windows guest should be warned about virtio drivers")
	}
}

func TestPreflightWarnsWhenTheGuestOSIsUnknown(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.OSType = ""
	if !strings.Contains(warningText(Preflight(src, to("qemu"))), "did not record a guest OS type") {
		t.Fatal("an unknown OS should warn rather than assert compatibility")
	}
}

func TestPreflightWarnsWhenTheSourceIsRunning(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Running = true
	plan := Preflight(src, to("qemu"))
	if !plan.StopFirst {
		t.Fatal("a running source must be stopped first")
	}
	if !strings.Contains(warningText(plan), "will be stopped") {
		t.Fatalf("and the plan must say so, got:\n%s", warningText(plan))
	}
}

func TestPreflightNamesWhatItDrops(t *testing.T) {
	newStub(t)
	plan := Preflight(sourceVM(), to("qemu"))
	joined := strings.Join(plan.Dropped, "\n")
	for _, want := range []string{"MAC address and IP", "node placement", "instancetype"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dropped config should mention %q, got:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "tags") {
		t.Errorf("qemu cannot record tags and the source has one, so that should be named:\n%s", joined)
	}
}

func TestPreflightStepsFollowThePipelineOrder(t *testing.T) {
	newStub(t)
	src := sourceVM()
	src.VM.Running = true
	target := to("qemu")
	target.DeleteSource = true

	var names []string
	for _, s := range Preflight(src, target).Steps {
		names = append(names, s.Name)
	}
	want := []string{"preflight", "stop source", "export", "ingest", "verify", "delete source"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", names, want)
	}
}

func TestPreflightRetainsTheSourceByDefault(t *testing.T) {
	newStub(t)
	plan := Preflight(sourceVM(), to("qemu"))
	if plan.DeleteSource {
		t.Fatal("deleting the source must be opt-in: a failed ingest after a delete is unrecoverable")
	}
	last := plan.Steps[len(plan.Steps)-1]
	if last.Name != "retain source" || !strings.Contains(last.Detail, "--delete-source") {
		t.Fatalf("the plan should say the source stays and how to change that, got %+v", last)
	}
}

func TestPreflightArtifactLandsInScratch(t *testing.T) {
	newStub(t)
	target := to("qemu")
	target.Scratch = "/var/tmp/corral"
	plan := Preflight(sourceVM(), target)
	if filepath.Dir(plan.Artifact) != "/var/tmp/corral" {
		t.Fatalf("artifact = %q, want it under the scratch directory", plan.Artifact)
	}
	if !strings.Contains(plan.Artifact, "web-1") || !strings.HasSuffix(plan.Artifact, ".qcow2") {
		t.Fatalf("artifact = %q, want a name that identifies the instance and format", plan.Artifact)
	}
}

// ── execution ─────────────────────────────────────────────────────

func TestExecuteRunsThePipeline(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	src := sourceVM()
	src.VM.Running = true
	target := to("qemu")
	target.Scratch = dir

	plan := Preflight(src, target)
	var stages []string
	result, err := Execute(context.Background(), plan, func(p Progress) { stages = append(stages, p.Stage) })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(s.stopped) != 1 || s.stopped[0] != "web-1" {
		t.Errorf("source should have been stopped once, got %v", s.stopped)
	}
	if len(s.ingested) != 1 || s.ingested[0].Backend != "qemu" {
		t.Errorf("destination should have been ingested once, got %v", s.ingested)
	}
	if !result.SourceStopped || result.SourceDeleted {
		t.Errorf("result = %+v, want stopped and not deleted", result)
	}
	if len(s.deleted) != 0 {
		t.Errorf("nothing should have been deleted, got %v", s.deleted)
	}
	if !strings.Contains(strings.Join(stages, ","), "exporting") {
		t.Errorf("progress stages = %v, want the export reported", stages)
	}
}

func TestExecuteRemovesTheArtifact(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	target := to("qemu")
	target.Scratch = dir

	if _, err := Execute(context.Background(), Preflight(sourceVM(), target), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(s.exportedTo); !os.IsNotExist(err) {
		t.Fatalf("the scratch artifact should be cleaned up, %s still exists", s.exportedTo)
	}
}

func TestExecuteRemovesTheArtifactAfterAFailedIngest(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	s.ingestErr = errors.New("libvirt said no")
	target := to("qemu")
	target.Scratch = dir

	if _, err := Execute(context.Background(), Preflight(sourceVM(), target), nil); err == nil {
		t.Fatal("expected the ingest failure to surface")
	}
	if _, err := os.Stat(s.exportedTo); !os.IsNotExist(err) {
		t.Fatal("a failed move must not leave gigabytes in scratch")
	}
}

func TestExecuteDeletesTheSourceOnlyWhenAsked(t *testing.T) {
	s := newStub(t)
	target := to("qemu")
	target.Scratch = t.TempDir()
	target.DeleteSource = true

	result, err := Execute(context.Background(), Preflight(sourceVM(), target), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.SourceDeleted || len(s.deleted) != 1 {
		t.Fatalf("--delete-source should have removed the source, got %+v / %v", result, s.deleted)
	}
}

func TestExecuteRefusesToRunARefusedPlan(t *testing.T) {
	s := newStub(t)
	plan := Preflight(sourceVM(), to("incus"))
	if _, err := Execute(context.Background(), plan, nil); err == nil {
		t.Fatal("Execute must not run a plan preflight refused")
	}
	if len(s.stopped)+len(s.ingested) != 0 {
		t.Fatal("and it must not have touched anything first")
	}
}

func TestExecuteDoesNotExportWhenStoppingTheSourceFails(t *testing.T) {
	s := newStub(t)
	s.stopErr = errors.New("virtctl: connection refused")
	src := sourceVM()
	src.VM.Running = true
	target := to("qemu")
	target.Scratch = t.TempDir()

	if _, err := Execute(context.Background(), Preflight(src, target), nil); err == nil {
		t.Fatal("expected the stop failure to surface")
	}
	if s.exportedTo != "" {
		t.Fatal("exporting a running guest's disk would produce a torn image")
	}
}

func TestExecuteSaysTheSourceSurvivedWhenIngestFails(t *testing.T) {
	s := newStub(t)
	s.ingestErr = errors.New("no space on the pool")
	src := sourceVM()
	src.VM.Running = true
	target := to("qemu")
	target.Scratch = t.TempDir()

	_, err := Execute(context.Background(), Preflight(src, target), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the source is intact") {
		t.Fatalf("the operator needs to know the source survived, got: %v", err)
	}
}

func TestExecuteReportsButDoesNotFailOnAnUndeletableSource(t *testing.T) {
	s := newStub(t)
	s.deleteErr = errors.New("kubectl: forbidden")
	target := to("qemu")
	target.Scratch = t.TempDir()
	target.DeleteSource = true

	result, err := Execute(context.Background(), Preflight(sourceVM(), target), nil)
	if err != nil {
		t.Fatalf("the move worked; a leftover source is not a failed move: %v", err)
	}
	if result.SourceDeleted {
		t.Fatal("SourceDeleted must be false when the delete failed")
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "could not be deleted") {
		t.Fatalf("and it must be reported, got %v", result.Warnings)
	}
}

func TestExecuteCarriesTheShapeToTheDestination(t *testing.T) {
	s := newStub(t)
	target := to("libvirt")
	target.Scratch = t.TempDir()
	target.SSHKey = "ssh-ed25519 AAAA"
	src := sourceVM()
	src.UEFI = true

	if _, err := Execute(context.Background(), Preflight(src, target), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := s.shapes[0]
	if got.CPU != 4 || got.Mem != "8Gi" || got.Disk != "20Gi" || !got.UEFI || got.SSHKey != "ssh-ed25519 AAAA" {
		t.Fatalf("shape = %+v, want the source's sizing plus the requested key and firmware", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"20G", 20 << 30}, {"20Gi", 20 << 30}, {"20GiB", 20 << 30},
		{"512M", 512 << 20}, {"2Ti", 2 << 40}, {"1024", 1024},
	} {
		got, err := parseSize(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "lots", "-5G", "0G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should have failed", bad)
		}
	}
}

func TestHumanReadsLikeASize(t *testing.T) {
	for in, want := range map[int64]string{
		20 << 30: "20.0GiB", 512 << 20: "512.0MiB", 3 << 40: "3.0TiB", 900: "900B",
	} {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestRealSeamsAreWiredToTheRealPackages guards the thing the stubs hide: that
// the defaults point at pkg/export and pkg/backend rather than at nothing.
func TestRealSeamsAreWiredToTheRealPackages(t *testing.T) {
	if !canIngest("qemu") {
		t.Error("qemu implements backend.Ingester, so the real canIngest should say so")
	}
	if canIngest("incus") {
		t.Error("incus deliberately does not implement Ingester")
	}
	if ingestReason("incus") == "" {
		t.Error("and the absence must come with an explanation")
	}
	if got := formatsFor("kubevirt"); len(got) == 0 {
		t.Error("the real export.Formats should answer for kubevirt")
	}
	if _, err := freeSpace(os.TempDir()); err != nil {
		t.Errorf("freeSpace on the temp dir should work: %v", err)
	}
}

// TestPairsMatchTheADR pins the supported-pairs table to the code, so the two
// cannot drift silently. It reflects the first slice: qemu and libvirt are the
// only destinations wired, which ADR-0010 records.
func TestPairsMatchTheADR(t *testing.T) {
	s := newStub(t)
	// Real capability answers, stubbed free space: whether /tmp happens to hold
	// 20GiB on this machine is not what the table is about.
	canIngest, ingestReason = backend.CanIngest, backend.IngestRefusal
	acceptsUEFI, formatsFor = destinationAcceptsUEFI, export.Formats
	_ = s

	sources := []string{"kubevirt", "qemu", "libvirt", "incus"}
	destinations := map[string]bool{"qemu": true, "libvirt": true, "kubevirt": true, "proxmox": true, "incus": false}

	for _, from := range sources {
		for dst, want := range destinations {
			if from == dst {
				continue
			}
			src := sourceVM()
			src.VM.Backend = from
			plan := Preflight(src, to(dst))
			if plan.OK() != want {
				t.Errorf("%s → %s: allowed = %v, want %v (%s)",
					from, dst, plan.OK(), want, refusalText(plan))
			}
		}
	}
}
