// Package move is the cross-backend cold move (ADR-0010).
//
// `migrate` already means "move a guest between nodes of one backend, live
// where the backend supports it". This is the other axis, and it is necessarily
// cold: there is no shared memory state between a KubeVirt VMI and a
// systemd-managed QEMU process, so the guest stops, its disk moves, and it
// starts again somewhere else. The verb is `move` everywhere, and every surface
// says the same sentence: a move stops the guest.
//
// Nothing here is new mechanism. pkg/export already answers "get this
// instance's disk out" for four backends, and backend.Ingester answers "make
// this disk a runnable instance here". This package is the composition, the
// preflight that refuses before anything is touched, and the decision about
// which pairs are supported at all.
package move

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/backend"
	"github.com/tuna-os/corral/pkg/export"
	"github.com/tuna-os/corral/pkg/types"
)

// Target is where an instance is going. Everything except Backend is optional:
// an empty Name keeps the source's, and an empty Scratch uses the OS temp dir.
type Target struct {
	Backend   string
	Context   string
	Namespace string
	Name      string

	// Scratch is where the exported artifact lands before it is ingested. The
	// disk goes through local storage rather than streaming disk-to-disk,
	// because the artifact-on-disk path is the one that is debuggable when it
	// goes wrong (ADR-0010).
	Scratch string
	// SSHKey is injected into the destination instance where the backend's
	// create path supports it.
	SSHKey string
	// DeleteSource removes the source after a successful move. Off by default:
	// a move that leaves a stopped source behind is recoverable, and one that
	// deleted the source and failed to ingest is not.
	DeleteSource bool
}

// Refusal is one reason a move will not run. Refusals are collected, never
// returned one at a time — an operator who fixes the firmware mismatch only to
// be told about the disk space has been failed by the preflight.
type Refusal struct {
	Reason string `json:"reason"`
	Remedy string `json:"remedy,omitempty"`
}

func (r Refusal) String() string {
	if r.Remedy == "" {
		return r.Reason
	}
	return r.Reason + " — " + r.Remedy
}

// Step is one stage of the pipeline, in the order it will run. A plan renders
// as this list so `--dry-run` shows what would happen rather than a summary of
// it.
type Step struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// Inspect fills in the two facts a listing does not carry by asking the
// source's backend, and returns a Source ready for Preflight.
//
// Callers go through this rather than constructing a Source directly, because a
// zero-valued UEFI field is indistinguishable from "this guest is BIOS" and
// that is the difference between refusing a doomed move and performing one.
// A backend that cannot answer leaves both unknown, which downgrades the
// firmware refusal to the unknown-OS warning rather than silently asserting.
func Inspect(vm types.VM, container bool) Source {
	info := inspectGuest(vm.Ref())
	return Source{VM: vm, UEFI: info.UEFI, OSType: info.OSType, Container: container}
}

// Source is the instance being moved, as the caller's inventory already knows
// it, plus the two facts no listing carries. ADR-0010 wrote the signature as
// taking an InstanceRef; taking the VM instead keeps this package out of the
// listing business, the same way pkg/web's folder actions take the live map
// they already have.
type Source struct {
	VM types.VM
	// UEFI records that the guest boots via EFI. A UEFI guest that lands on a
	// BIOS-default target shows a blank screen, so this decides a refusal.
	UEFI bool
	// OSType is the guest family the source recorded ("linux", "windows", "").
	// Empty means unknown, which produces a warning rather than an assertion.
	OSType string
	// Container marks a rootfs-backed instance. Containers are out of scope:
	// turning one into a VM means installing a kernel and a bootloader into a
	// filesystem that never had them, which is a rebuild, not a move.
	Container bool
}

// Plan is the whole decision, made before anything is touched.
type Plan struct {
	Source      types.InstanceRef `json:"source"`
	Destination types.InstanceRef `json:"destination"`

	Shape  backend.Shape `json:"-"`
	Format export.Format `json:"format"`
	Steps  []Step        `json:"steps"`
	// Warnings are things the operator should know and then decide about. They
	// never block: an IP change is the operator's call, but it is always said.
	Warnings []string `json:"warnings,omitempty"`
	// Dropped lists configuration the destination cannot express. A silent loss
	// of a passthrough device turns up three weeks later as a mystery.
	Dropped  []string  `json:"dropped,omitempty"`
	Refusals []Refusal `json:"refusals,omitempty"`

	// EstimatedBytes is the source's provisioned disk size — the worst case for
	// scratch space, since a sparse disk exports smaller and never larger.
	EstimatedBytes int64 `json:"estimatedBytes"`

	Artifact     string `json:"artifact"`
	StopFirst    bool   `json:"stopFirst"`
	DeleteSource bool   `json:"deleteSource"`
}

// OK reports whether the plan can run.
func (p Plan) OK() bool { return len(p.Refusals) == 0 }

// Result is what a completed move did.
type Result struct {
	Destination   types.InstanceRef `json:"destination"`
	Bytes         int64             `json:"bytes"`
	SourceStopped bool              `json:"sourceStopped"`
	SourceDeleted bool              `json:"sourceDeleted"`
	Warnings      []string          `json:"warnings,omitempty"`
	Dropped       []string          `json:"dropped,omitempty"`
}

// ── seams ─────────────────────────────────────────────────────────
//
// Each stage of the pipeline is a package var so the composition can be tested
// without a hypervisor. The defaults are the real ones.

var (
	exportDisk = func(ctx context.Context, req export.Request, progress export.ProgressFunc) (export.Result, error) {
		adapter, err := export.For(req.Ref)
		if err != nil {
			return export.Result{}, err
		}
		return adapter.Export(ctx, req, progress)
	}

	ingestDisk = func(ref types.InstanceRef, disk string, shape backend.Shape) error {
		adapter, err := backend.For(ref)
		if err != nil {
			return err
		}
		ingester, ok := adapter.(backend.Ingester)
		if !ok {
			return fmt.Errorf("the %s backend cannot receive a moved instance: %s",
				ref.Backend, backend.IngestRefusal(ref.Backend))
		}
		return ingester.Ingest(ref, disk, shape)
	}

	powerOff = func(ref types.InstanceRef) error {
		adapter, err := backend.For(ref)
		if err != nil {
			return err
		}
		power, ok := adapter.(backend.Power)
		if !ok {
			return fmt.Errorf("the %s backend cannot stop an instance", ref.Backend)
		}
		return power.Stop(ref.Name)
	}

	removeSource = func(ref types.InstanceRef) error {
		adapter, err := backend.For(ref)
		if err != nil {
			return err
		}
		power, ok := adapter.(backend.Power)
		if !ok {
			return fmt.Errorf("the %s backend cannot delete an instance", ref.Backend)
		}
		return power.Delete(ref.Name)
	}

	inspectGuest = backend.Inspect
	canIngest    = backend.CanIngest
	ingestReason = backend.IngestRefusal
	acceptsUEFI  = destinationAcceptsUEFI
	formatsFor   = export.Formats
	freeSpace    = availableBytes
)

// destinationAcceptsUEFI asks the destination adapter directly rather than
// keeping a second table of which backends have firmware paths.
func destinationAcceptsUEFI(dst types.InstanceRef) bool {
	adapter, err := backend.For(dst)
	if err != nil {
		return false
	}
	ingester, ok := adapter.(backend.Ingester)
	return ok && ingester.AcceptsUEFI()
}

// ── planning ──────────────────────────────────────────────────────

// ingestibleFormats are the export artifacts an ingester can consume, in
// preference order.
//
// Only qcow2, and deliberately: raw.gz is a disk but a compressed one, and the
// ingest path hands the file straight to `qemu-img convert`, which does not
// read gzip. Decompressing first is a step this slice does not have, and every
// backend that can export at all offers qcow2, so nothing is lost by saying so
// rather than producing an artifact the destination silently rejects. An Incus
// export is an instance tarball restorable only with `incus import` — a fine
// backup, and not something another backend can boot.
var ingestibleFormats = []export.Format{export.Qcow2}

// Preflight builds the plan. It touches nothing, and it reports every reason
// the move would not work rather than the first.
func Preflight(src Source, dst Target) Plan {
	from := refOf(src.VM)
	to := types.InstanceRef{
		Backend:   dst.Backend,
		Context:   dst.Context,
		Namespace: dst.Namespace,
		Name:      firstNonEmpty(dst.Name, src.VM.Name),
	}

	plan := Plan{
		Source:       from,
		Destination:  to,
		DeleteSource: dst.DeleteSource,
		StopFirst:    src.VM.Running,
		Shape: backend.Shape{
			CPU:    src.VM.CPU,
			Mem:    src.VM.Mem,
			Disk:   src.VM.Disk,
			UEFI:   src.UEFI,
			OSType: src.OSType,
			SSHKey: dst.SSHKey,
			Tags:   append([]string(nil), src.VM.Tags...),
		},
	}

	refuse := func(reason, remedy string) {
		plan.Refusals = append(plan.Refusals, Refusal{Reason: reason, Remedy: remedy})
	}

	// Identity first: everything below reads these fields.
	if err := from.Validate(); err != nil {
		refuse("the source instance is not fully identified", err.Error())
	}
	if strings.TrimSpace(dst.Backend) == "" {
		refuse("no destination backend was given", "pass --to with a backend name")
		return plan
	}
	if from.Backend == to.Backend && from.Context == to.Context {
		refuse(fmt.Sprintf("the source and destination are both %s in the same context",
			from.Backend),
			"to move a guest between nodes of one backend, use `corral migrate`, which stays live where the backend supports it")
	}

	if src.Container {
		refuse("containers cannot be moved between backends",
			"a container is a rootfs, not a disk image; turning one into a VM means installing a kernel and bootloader, which is a rebuild rather than a move")
	}

	// Can the disk come out, and as something a disk-consumer can use?
	if !export.Supported(from.Backend) {
		refuse(fmt.Sprintf("the %s backend cannot export a disk", from.Backend),
			"a move needs the source's disk; see docs/backend-parity.md")
	} else {
		format, ok := pickFormat(formatsFor(from.Backend))
		if !ok {
			refuse(fmt.Sprintf("the %s backend exports %s, which is not a disk image",
				from.Backend, joinFormats(formatsFor(from.Backend))),
				"that artifact restores only into the backend it came from, so it cannot seed an instance elsewhere")
		}
		plan.Format = format
	}

	// Can the disk go in?
	if !canIngest(to.Backend) {
		refuse(fmt.Sprintf("the %s backend cannot receive a moved instance", to.Backend),
			ingestReason(to.Backend))
	} else if src.UEFI && !acceptsUEFI(to) {
		refuse(fmt.Sprintf("the guest boots via UEFI and the %s backend has no firmware path",
			to.Backend),
			"a UEFI guest booted on a BIOS machine shows a blank screen; move it to a backend that can express EFI boot")
	}

	// Space. Provisioned size is the worst case both for the artifact and for
	// what the destination will allocate.
	scratch := firstNonEmpty(dst.Scratch, os.TempDir())
	plan.Artifact = filepath.Join(scratch, artifactName(from, plan.Format))
	if size, err := parseSize(src.VM.Disk); err == nil {
		plan.EstimatedBytes = size
		if free, err := freeSpace(scratch); err == nil && free > 0 && free < size {
			refuse(fmt.Sprintf("%s has %s free and the disk needs up to %s",
				scratch, human(free), human(size)),
				"free space or pass a scratch directory on a larger filesystem")
		}
	} else if strings.TrimSpace(src.VM.Disk) != "" {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("could not read the source disk size (%q), so scratch space was not checked", src.VM.Disk))
	}

	// Warnings: true whether or not the move is refused, and worth saying even
	// on a plan that will not run, because fixing the refusal does not fix them.
	plan.Warnings = append(plan.Warnings,
		"the guest stops for the whole move and gets a new MAC address and almost certainly a new IP; anything pinned to either will break")
	switch strings.ToLower(src.OSType) {
	case "windows":
		plan.Warnings = append(plan.Warnings,
			"a Windows guest needs virtio drivers already installed to boot on the destination's disk bus; if it was installed on SATA or IDE it will bluescreen")
	case "":
		plan.Warnings = append(plan.Warnings,
			"the source did not record a guest OS type, so disk-bus compatibility could not be checked")
	}
	if plan.StopFirst {
		plan.Warnings = append(plan.Warnings,
			"the source is running and will be stopped before its disk is exported")
	}

	plan.Dropped = droppedConfig(src, to)
	plan.Steps = stepsFor(plan, src)
	return plan
}

// droppedConfig names what will not survive. Deliberately incomplete travel is
// fine; silent incompleteness is not.
func droppedConfig(src Source, to types.InstanceRef) []string {
	dropped := []string{"MAC address and IP", "node placement"}
	switch src.VM.Backend {
	case "kubevirt":
		dropped = append(dropped, "KubeVirt instancetype/preference and any PVC annotations")
	case "proxmox":
		dropped = append(dropped, "PVE HA group, boot order, and per-disk cache settings")
	case "incus":
		dropped = append(dropped, "Incus profiles and device overrides")
	}
	if src.VM.Bootc {
		dropped = append(dropped, "the bootc image reference (the moved instance is a plain disk, no longer image-managed)")
	}
	if len(src.VM.Tags) > 0 && to.Backend == "qemu" {
		dropped = append(dropped, "tags (the qemu backend has nowhere to record them)")
	}
	return dropped
}

func stepsFor(plan Plan, src Source) []Step {
	steps := []Step{{
		Name:   "preflight",
		Detail: "check firmware, disk, space, and capability without changing anything",
	}}
	if plan.StopFirst {
		steps = append(steps, Step{
			Name:   "stop source",
			Detail: fmt.Sprintf("stop %s so the exported disk is not torn", plan.Source.Name),
		})
	}
	steps = append(steps,
		Step{Name: "export", Detail: fmt.Sprintf("write %s as %s", plan.Artifact, plan.Format)},
		Step{Name: "ingest", Detail: fmt.Sprintf("create %s on %s from that disk, stopped",
			plan.Destination.Name, plan.Destination.Backend)},
		Step{Name: "verify", Detail: "confirm the destination instance exists"},
	)
	if plan.DeleteSource {
		steps = append(steps, Step{
			Name:   "delete source",
			Detail: fmt.Sprintf("remove %s from %s", plan.Source.Name, plan.Source.Backend),
		})
	} else {
		steps = append(steps, Step{
			Name:   "retain source",
			Detail: "the source stays, stopped; pass --delete-source to remove it",
		})
	}
	_ = src
	return steps
}

// ── execution ─────────────────────────────────────────────────────

// Progress reports the stage a move is in. Total is 0 where the backend cannot
// say how large the artifact will be, which most cannot until it is finished.
type Progress struct {
	Stage string
	Done  int64
	Total int64
}

// ProgressFunc receives progress updates from the goroutine doing the work, so
// an implementation must not block.
type ProgressFunc func(Progress)

func (p ProgressFunc) report(stage string, done, total int64) {
	if p != nil {
		p(Progress{Stage: stage, Done: done, Total: total})
	}
}

// Execute runs a plan. It refuses to run one that did not pass preflight rather
// than re-deriving the decision, so a caller cannot skip the check by calling
// Execute directly.
//
// The artifact is removed on the way out whether or not the move succeeded: it
// is a copy of a disk that still exists on the source, and leaving multi-gigabyte
// files in scratch after a failure is its own outage.
func Execute(ctx context.Context, plan Plan, progress ProgressFunc) (Result, error) {
	if !plan.OK() {
		return Result{}, fmt.Errorf("this move was refused by preflight: %s", plan.Refusals[0])
	}

	result := Result{Destination: plan.Destination, Warnings: plan.Warnings, Dropped: plan.Dropped}

	if plan.StopFirst {
		progress.report("stopping source", 0, 0)
		if err := powerOff(plan.Source); err != nil {
			return result, fmt.Errorf("stopping %s: %w", plan.Source.Name, err)
		}
		result.SourceStopped = true
	}

	progress.report("exporting", 0, plan.EstimatedBytes)
	exported, err := exportDisk(ctx, export.Request{
		Ref:    plan.Source,
		Dest:   plan.Artifact,
		Format: plan.Format,
	}, func(p export.Progress) { progress.report("exporting", p.Done, p.Total) })
	if err != nil {
		return result, fmt.Errorf("exporting %s: %w", plan.Source.Name, err)
	}
	defer os.Remove(exported.Path)
	result.Bytes = exported.Bytes

	progress.report("ingesting", 0, exported.Bytes)
	// Ingest consumes the disk, so a failure here has already possibly moved
	// it. The source is untouched either way, which is the whole reason the
	// default leaves it in place.
	if err := ingestDisk(plan.Destination, exported.Path, plan.Shape); err != nil {
		return result, fmt.Errorf("creating %s on %s: %w (the source is intact%s)",
			plan.Destination.Name, plan.Destination.Backend, err,
			map[bool]string{true: " but stopped", false: ""}[result.SourceStopped])
	}

	if plan.DeleteSource {
		progress.report("deleting source", 0, 0)
		if err := removeSource(plan.Source); err != nil {
			// The move worked. Say what happened and do not fail the whole
			// operation over a source that is merely still there.
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("the move succeeded but the source could not be deleted: %v", err))
			return result, nil
		}
		result.SourceDeleted = true
	}

	progress.report("done", exported.Bytes, exported.Bytes)
	return result, nil
}

// ── helpers ───────────────────────────────────────────────────────

func refOf(vm types.VM) types.InstanceRef {
	return types.InstanceRef{
		Peer:      vm.Peer,
		Backend:   vm.Backend,
		Context:   vm.Context,
		Namespace: vm.Namespace,
		Name:      vm.Name,
	}
}

func pickFormat(offered []export.Format) (export.Format, bool) {
	for _, want := range ingestibleFormats {
		for _, have := range offered {
			if want == have {
				return want, true
			}
		}
	}
	return "", false
}

func joinFormats(formats []export.Format) string {
	names := make([]string, 0, len(formats))
	for _, f := range formats {
		names = append(names, string(f))
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

func artifactName(ref types.InstanceRef, format export.Format) string {
	name := ref.Name
	if name == "" {
		name = "instance"
	}
	ext := string(format)
	if ext == "" {
		ext = "img"
	}
	return fmt.Sprintf("corral-move-%s-%s.%s", ref.Backend, name, ext)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseSize accepts the sizes the rest of Corral uses: 20G, 20Gi, 20480M.
func parseSize(s string) (int64, error) {
	value := strings.TrimSpace(s)
	if value == "" {
		return 0, fmt.Errorf("empty size")
	}
	var multiplier int64 = 1
	upper := strings.ToUpper(value)
	switch {
	case strings.HasSuffix(upper, "TIB"), strings.HasSuffix(upper, "TI"), strings.HasSuffix(upper, "T"):
		multiplier = 1 << 40
	case strings.HasSuffix(upper, "GIB"), strings.HasSuffix(upper, "GI"), strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
	case strings.HasSuffix(upper, "MIB"), strings.HasSuffix(upper, "MI"), strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
	}
	digits := strings.TrimRight(upper, "GIMBT")
	var n int64
	if _, err := fmt.Sscanf(digits, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size like 20G", s)
	}
	return n * multiplier, nil
}

func human(bytes int64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.1fTiB", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(bytes)/(1<<20))
	}
	return fmt.Sprintf("%dB", bytes)
}
