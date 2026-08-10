# ADR-0010: Moving an instance between backends

**Status:** accepted, implemented
**Date:** 2026-07-31

## Context

Corral aggregates five backends, and an operator who runs more than one
eventually wants an instance to change which one it lives on: a VM prototyped on
a laptop's QEMU belongs on the cluster; a Proxmox guest is being retired onto
KubeVirt; a KubeVirt VM needs to come back down to libvirt for a hardware
passthrough the cluster cannot give it.

`migrate` already exists and means something else — moving a guest between
*nodes of one backend*, live where the backend supports it (ADR-0009's
precondition check, KubeVirt's `VirtualMachineInstanceMigration`). This is the
other axis, and it is necessarily **cold**: there is no shared memory state
between a KubeVirt VMI and a systemd-managed QEMU process, so the guest stops,
its disk moves, and it starts again somewhere else. Calling both "migrate" would
be the kind of overloading that gets someone's production VM stopped when they
expected a live move.

The pieces are already contracts, which is why this is worth doing now rather
than as a special case per pair:

- **Disk out** — `pkg/export` (#131) is backend-neutral: adapters for KubeVirt,
  QEMU, libvirt, and Incus, formats `qcow2` / `raw.gz` / `incus-tar`, progress
  reporting, and `snapshot.Consistency` on the result so an export says what it
  actually captured. It already refuses a running instance where that would
  produce a torn image.
- **Disk in** — `pkg/bootc.Target` is defined as "puts a built disk onto a
  backend as a runnable instance", implemented for QEMU and libvirt. KubeVirt
  ingests through CDI (`ImportDataVolume` from a URL, `UploadDataVolume` from a
  local file). Proxmox creates with `import-from` on `scsi0`.
- **Everything else** — `pkg/backend`'s operation contract (ADR: parity step 2)
  gives uniform power control, and `types.InstanceRef` gives an identity that
  already spans backends.

What is missing is the composition, an honest preflight, and a decision about
which pairs are supported at all.

## Decision

### It is called `move`, never `migrate`

`corral move <instance> --to <context>` and `POST /api/vms/{ns}/{name}/move`.
The distinction is load-bearing and is stated in every surface: **a move stops
the guest.** Where a backend can migrate within itself, that stays `migrate` and
stays live.

### The pipeline, with the source destroyed last

    preflight → export → convert → ingest → verify → (retire source)

The source instance is **stopped, not deleted**, and is only deleted when the
operator asks (`--delete-source`, default off). A move that has produced a
working instance on the destination and left a stopped one behind is a good
outcome; one that deleted the source and failed to ingest is unrecoverable. The
default therefore leaves both, and the surfaces say so.

### Supported pairs

| From ↓ To → | qemu | libvirt | kubevirt | proxmox | incus |
|---|---|---|---|---|---|
| **qemu** | — | yes | yes | yes¹ | **yes** |
| **libvirt** | yes | — | yes | yes¹ | **yes** |
| **kubevirt** | yes | yes | — | yes¹ | **yes** |
| **proxmox** | yes | yes | yes | — | **yes** |
| **incus (VM)** | yes | yes | yes | yes¹ | — |

¹ Proxmox as a *destination* needs a disk-ingest path; see below.

This table is the intended end state. **What is wired today is narrower**, and
the code refuses everything else rather than approximating it — see *First
slice* below for exactly which cells are live.

**Incus is a move destination via Image Publishing.** `Incus` ingests disks by creating an Incus VM image (with `metadata.yaml` and `rootfs.img`), importing it via `incus image import`, launching the VM instance, and deleting the temporary image. If `incus-agent` is not pre-installed in the source guest image, it will not be present in the target instance, and this omission is explicitly recorded in the move's `Dropped` list.

**Containers are not in this graph at all.** An Incus LXC container and a
pet-pod CT have no disk image — they are a rootfs and a PVC. Turning one into a
VM means installing a kernel and a bootloader into a filesystem that never had
them: a *rebuild*, not a move, and one whose failure mode is a guest that
imports cleanly and then does not boot. It is out of scope, and `move` refuses a
container by name rather than attempting something shaped like success.

### Preflight refuses before anything is touched

The step that makes this safe rather than exciting. It runs first, changes
nothing, and reports every reason the move would not work — all of them, not
just the first:

- **Firmware.** A UEFI guest landing on a BIOS-default target boots to a blank
  screen. Detected from the source (KubeVirt's `firmware.bootloader.efi`,
  libvirt's `<loader>`, PVE's `bios: ovmf`) and set on the destination, or
  refused when the destination cannot express it.
- **Disk bus and drivers.** A Windows guest imported from PVE's SATA default
  onto virtio-scsi will not boot without virtio drivers already installed. The
  guest OS is known from the source config where the backend records it; where
  it is not, the preflight *warns* rather than asserting.
- **Space.** Source virtual size versus destination free space, and versus local
  scratch, since the artifact lands on local disk first.
- **Address change.** The guest gets a new MAC and (almost always) a new IP.
  Anything pinned to either breaks. This is a warning, never a refusal — it is
  the operator's call — but it is always said.
- **Capability.** The destination must implement the ingest path at all, which
  is a question `pkg/backend` can already answer.

`--dry-run` prints the plan and the warnings and exits. The web UI shows the
same list before the button commits.

### Proxmox ingest is the one place ADR-0009's "API only" bends

PVE's API cannot accept a raw disk image on older versions: the upload endpoint
takes `iso`, `vztmpl`, and `backup` content, not `images`. So a move *into*
Proxmox resolves one of three paths at preflight, in order:

1. A storage that advertises the `import` content type (PVE 8.4+) —
   `StorageInfo.Holds("import")` already answers this.
2. A shared storage path Corral can write to directly.
3. SSH to a node plus `qm importdisk`.

If none is available, the move is refused with those three options named. This
is worth stating loudly because ADR-0009 chose the API precisely to avoid
requiring SSH; a *destination* move is the one operation that may still need it,
and an operator should learn that from a refusal rather than from a half-moved
VM.

### Configuration travels, deliberately incompletely

Cores, memory, disk size, firmware, guest OS type, tags, and the folder
membership (ADR-0008) follow the instance. What does not: the MAC, the IP, the
node placement, backend-specific tuning (KubeVirt instancetypes, PVE HA groups),
and anything the destination cannot express. The move reports what it dropped —
a silent loss of a passthrough device or a pinned NUMA layout is exactly the
kind of thing that turns up three weeks later as a performance mystery.

### The contract

`pkg/move`, in the shape the other backend-neutral contracts already use:

```go
type Plan struct {
    Source, Destination types.InstanceRef
    Steps               []Step
    Warnings            []string
    Refusals            []Refusal   // non-empty means it will not run
    EstimatedBytes      int64
}

type Mover interface {
    Plan(src types.InstanceRef, dst Target) (Plan, error)
    Execute(ctx context.Context, plan Plan, progress ProgressFunc) (Result, error)
}
```

`Ingester` joins `pkg/backend`'s families as the destination half, so
"can this backend receive a disk" becomes a type assertion the parity matrix
derives from, exactly like every other operation. `bootc.Target` is the existing
implementation of that idea for two backends; generalising it here means bootc
and move share one ingest path rather than growing a second.

## First slice: what is actually wired

`pkg/move` and `corral move` exist, and every cell the table above promises is
now wired:

| From ↓ To → | qemu | libvirt | kubevirt | proxmox | incus |
|---|---|---|---|---|---|
| **kubevirt** | **yes** | **yes** | — | **yes** | **yes** |
| **qemu** | — | **yes** | **yes** | **yes** | **yes** |
| **libvirt** | **yes** | — | **yes** | **yes** | **yes** |
| **proxmox** | no¹ | no¹ | no¹ | — | **yes** |
| **incus (VM)** | **yes** | **yes** | **yes** | **yes** | — |

¹ Proxmox as a *source* needs an export adapter, which `pkg/export` does not
have yet. It is a destination, not yet a source — the mirror image of Incus.

Two things narrow it beyond what the ADR anticipated, each with a refusal that
names the reason:

- **The five ingest paths, and what each costs.** qemu and libvirt delegate to
  the `bootc.Target` that already puts a disk onto them, so a bootc disk and a
  moved disk land the same way. KubeVirt uploads through CDI (`virtctl
  image-upload` creates the DataVolume, and the VM then adopts the resulting
  PVC as its boot disk — `PVC`, never `ImportURL`, since the disk is already in
  the cluster). Proxmox uploads to a storage advertising the `import` content
  type and creates with `import-from`; where no such storage exists it refuses
  with the three ways forward. Incus publishes an Incus VM image (`metadata.yaml`
  + `rootfs.img`), imports it via `incus image import`, launches the VM, and
  removes the temporary image.
- **Firmware travels now.** A UEFI guest was previously refused everywhere but
  libvirt. KubeVirt sets `firmware.bootloader.efi`, PVE sets `bios: ovmf`
  plus an `efidisk0`, and Incus is OVMF-only, so only qemu — whose generated systemd unit has no OVMF
  path — still refuses one. Secure Boot stays off: it needs an EFI vars
  volume and a signed bootloader, and enabling it silently would break exactly
  the imported guests this serves.
- **Incus is both a source and a destination.** `pkg/export`'s Incus adapter grew a
  `qcow2` format for exporting boot disks. For ingest, Incus publishes an image from
  the imported disk and launches the VM.
- **Only qcow2 is ingested.** `raw.gz` is a disk, but a compressed one, and the
  ingest path hands the file to `qemu-img convert`, which does not read gzip.
  Every backend that can export offers qcow2, so nothing is lost by naming the
  constraint instead of producing an artifact the destination rejects.

### The web surface: drag to propose, never to commit

`POST /api/move/preflight` and `POST /api/move` are two endpoints rather than
one, and the split is what makes drag-and-drop safe. The Pool View tree holds
two kinds of node and they behave differently by design:

- Dropping a VM onto a **pool** reassigns folder membership. Nothing is touched,
  so it commits immediately and is undone by dragging back.
- Dropping a VM onto a **backend** proposes a move. The drop calls the
  preflight — which changes nothing, so it is safe on a stray gesture — and what
  comes back *is* the dialog: the steps, the warnings, the dropped
  configuration, and any refusals. A refused plan has no confirm button.

Backends that cannot receive a move are rendered inert with the reason on hover
rather than accepting a drop and refusing afterwards. `POST /api/move` re-runs
the preflight server-side before committing, so a client cannot skip the check
and a plan that went stale between the drop and the click is caught. A refused
preflight is a 200 (the refusals *are* the answer); a refused commit is a 409
carrying the same list.

The instance's folder membership follows it to the destination, since a move
that silently drops a VM out of the grouping an operator organised it into is a
worse surprise than the IP change.

- **Firmware and guest OS are asked of the source, not assumed.** `move.Inspect`
  goes through a `backend.Inspector` family — KubeVirt's
  `firmware.bootloader.efi`, libvirt's `firmware='efi'` or an OVMF `<loader>`,
  PVE's `bios: ovmf` and `ostype`. An Incus VM always answers UEFI, because
  Incus boots its VMs under OVMF with no BIOS option: that is the one backend
  where the fact belongs to the backend rather than the instance, and it means
  an Incus VM cannot move to qemu until qemu's generated unit grows a firmware
  path. qemu is also the one backend that cannot be *asked* — its unit records
  no firmware — so a qemu source inspects to unknown, which downgrades the
  refusal to the unknown-OS warning rather than asserting BIOS.

Also deviating from the sketch above: `Preflight` takes the `types.VM` the
caller's inventory already holds rather than an `InstanceRef`, which keeps
`pkg/move` out of the listing business — the same shape `pkg/web`'s folder
actions use. Firmware and guest OS ride alongside it in `move.Source`, filled
by `Inspect` rather than by the listing: a config read per instance is the right
price at preflight and the wrong one on the dashboard's five-second poll.

## Consequences

- Long-running and resumable-ish: a 40 GiB export, convert, and upload is
  minutes to hours. It runs as a task with progress (the web task log already
  exists, and Proxmox's UPIDs give real progress on that side), and a failure
  leaves the source stopped but intact.
- Local scratch space becomes a real requirement, and the preflight checks it.
- `pkg/export` gains no new API; `pkg/bootc.Target` becomes the seed of
  `backend.Ingester`, which is a refactor with two existing implementations and
  their e2e tests already in place.
- CI can cover qemu ⇄ libvirt on the existing `e2e-incus` runner (both are
  installed there today, and it already exercises the export adapters against
  the real tools). KubeVirt and Proxmox destinations stay unit-tested plus
  manual, the same honesty `docs/backend-parity.md` applies elsewhere.

## Alternatives considered

**Call it `migrate --to-backend`.** Rejected: the flag would hide a stop inside
a verb that means "no downtime" everywhere else in the tool.

**`virt-v2v`.** The right tool for VMware and Hyper-V conversions, and a
plausible future dependency for those. Rejected for this ADR because every pair
here is already qemu-family — the disks are qcow2 or raw and need no guest
conversion — so it would add a heavy dependency to solve a problem this
particular graph does not have.

**Stream disk-to-disk without local scratch.** Attractive for large disks, and
possible for some pairs (CDI can import from a URL Corral serves). Rejected for
the first slice: it multiplies the failure modes and needs a reachable listener,
and the artifact-on-disk path is the one that is debuggable when it goes wrong.
Worth revisiting once the plain path is trusted.

## Not in scope

Live cross-backend migration (there is no shared state to move), container
conversion, and VMware/Hyper-V import. The last is a natural follow-on once
`Ingester` exists, and is where `virt-v2v` would earn its place.
