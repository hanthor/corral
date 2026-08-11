# Backend parity

**KubeVirt was first-class and everything else was best effort.** That was true,
it was invisible, and this document is where it stops being either.

The rule now: **if a backend can do something Corral ships, Corral should support
it there.** Not every feature every backend has — parity across backends for the
features Corral has. Where a backend genuinely cannot do a thing, that is
recorded with a reason instead of left as silence.

## How this document is kept honest

The table below is **generated from `pkg/backend.Matrix`**, which is the single
source of truth, and `pkg/backend`'s conformance tests fail if:

- a matrix cell has no note (a gap nobody can act on),
- `types.CapabilitiesForBackend` advertises a capability the matrix does not mark
  as shipped (a button that fails on click), or omits one it does (a feature the
  operator cannot reach),
- `pkg/snapshot`'s adapter registry and the matrix disagree,
- **this document's table drifts from the matrix.**

So the numbers here cannot rot silently. Regenerate after changing the matrix.

Legend: ✅ shipped · 🔨 the backend can do this and Corral does not yet · — the
backend cannot, or it is meaningless there.

| Operation | kubevirt | qemu | incus | libvirt | proxmox |
|---|---|---|---|---|---|
| List / inventory | ✅ | ✅ | ✅ | ✅ | ✅ |
| Create | ✅ | ✅ | ✅ | ✅ | ✅ |
| Start | ✅ | ✅ | ✅ | ✅ | ✅ |
| Stop | ✅ | ✅ | ✅ | ✅ | ✅ |
| Restart | ✅ | ✅ | ✅ | ✅ | ✅ |
| Pause / resume | ✅ | ✅ | ✅ | ✅ | ✅ |
| Delete | ✅ | ✅ | ✅ | ✅ | ✅ |
| SSH | ✅ | ✅ | ✅ | 🔨 | ✅ |
| Serial / shell console | ✅ | 🔨 | ✅ | 🔨 | 🔨 |
| Graphical console (VNC) | ✅ | ✅ | 🔨 | ✅ | 🔨 |
| RDP | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Live CPU / memory | ✅ | ✅ | ✅ | ✅ | ✅ |
| Snapshot / restore | ✅ | ✅ | ✅ | ✅ | ✅ |
| Migrate | ✅ | — | 🔨 | 🔨 | ✅ |
| Clone | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| Template mark | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| CPU / memory edit | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| Add / remove disks | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| Expand disk | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| GPU passthrough | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| Export / backup disk | ✅ | ✅ | ✅ | ✅ | ✅ |
| Events | ✅ | ✅ | — | — | ✅ |
| Tags | ✅ | 🔨 | 🔨 | 🔨 | ✅ |
| Published ports | ✅ | ✅ | 🔨 | — | — |
| Containers (CT) | ✅ | — | ✅ | — | 🔨 |

## What the audit found

Four things that were worse than a missing feature, because each was a claim
Corral made that wasn't true. **The first three are fixed** (same change as this
document); the fourth is the structural one and is step 2 of the work below.

1. ~~**Every Incus instance is listed twice.**~~ *Fixed.* `pkg/incus.List` returns *all*
   instances as VMs — it reads `Type` from the JSON and then ignores it — while
   `pkg/ct.listIncusCTs` returns the same instances again as CTs. An Incus
   container therefore appears as both a VM and a CT in the fleet, and an Incus
   *virtual machine* appears as a CT. This is the single clearest symptom of
   LXC support never having been finished.

2. ~~**`pkg/ct`'s Incus path bypasses the runner seam.**~~ *Fixed —* it now goes
   through `pkg/incus`, which targets the configured remote, and demo mode shows
   Incus CTs for the first time. `listIncusCTs`,
   `incusExists`, `incusStart`, `incusStop`, and `incusDelete` call
   `exec.Command` directly instead of going through `shell.Runner`. Consequences:
   they are untestable, they are invisible to demo mode, and they always talk to
   the *local* daemon — the configured remote is ignored, so a CT on a remote
   Incus host cannot be started even though the VM path on the same host can.

3. ~~**Incus instances have no address.**~~ *Fixed —* `state.network` is read,
   skipping loopback and link-local. `List` never reads `state.network`, so
   the IP column is empty for every Incus instance and the RDP/SSH probes have
   nothing to aim at.

4. **The rich operations are reached by `switch backend`, not by an interface.**
   `types.Backend` has nine methods; snapshots, migrate, scale, volumes,
   metrics, clone, template, export, and events are all reached through
   `if backend == "kubevirt"` branches in `cmd/` and `pkg/web` — 33 such sites.
   That is the mechanism by which "best effort" happened: there was no contract
   to fail to satisfy.

`pkg/snapshot` is the counter-example and the template for the fix. It defines an
adapter per backend, reports honestly what each capture achieved, and refuses
with a typed error carrying a remedy. Every backend implements it, including
local QEMU. Nobody had to remember to add libvirt — the contract made the gap
visible.

## The work, in the order it should happen

**1. Stop the lies.** *Done:* Incus containers are CTs and Incus virtual
machines are VMs (`incus.Instance.IsContainer`), the CT path targets the
configured remote through `pkg/incus`, and the instance address is read. The
demo fixture now holds both an Incus container and an Incus VM, so the split
stays covered.

**2. Generalise the adapter contract.** *Done:* `pkg/backend/ops.go` defines a
small interface per operation family — `Power`, `Restarter`, `Suspender`,
`Sizer`, `Storer`, `Mover`, `Cloner`, `Templater`, `Tagger`, `Observer`,
`Exporter`, plus `Addresser` — and `pkg/backend/adapters.go` holds one adapter
per backend, the only place a backend's own signature is translated. A surface
calls `backend.For(ref)` and asserts the family it needs; it never switches on a
backend name again.

What makes it more than documentation: **support is derived from the
assertions.** `Provides(backend, operation)` answers from the adapter's type, and
a conformance test fails if the matrix claims an operation the adapter does not
implement, *or* if an adapter implements one the matrix has not been updated for.
So adding a method is how a gap gets closed, and forgetting the paperwork is a
red build rather than a silent inconsistency.

Two consequences worth knowing. `Power` is `Start`/`Stop`/`Delete` only, with
`Restart` split into its own interface, because two backends can merely fake a
reboot by stopping and starting — and a fake is what the contract exists to
prevent a backend claiming. And an adapter must be constructible from a bare
`InstanceRef`: derivation probes the type, never a live connection, so the
mechanism works offline and in tests.

The first surface converted is the TUI's power/pause/migrate path, which was a
per-backend if/else ladder per action. The behavioural win is the refusals: the
ladder's final `else` sent every unknown backend to local QEMU, and its pause and
migrate branches did nothing at all off KubeVirt. Now an unsupported action names
the backend and points here.

**3. Close the gaps, cheapest-first per backend.** The lists below come from the
matrix, so they stay current. The notes name the native mechanism, so none of
these start from a blank page.

**4. Add the Proxmox backend** per ADR-0009. *Done for the operations above:*
`pkg/proxmoxbe` drives a real PVE cluster over its HTTPS API, and it deliberately
did **not** add a sixth arm to the `switch` sites — it registers a
`pkg/snapshot` adapter (the one contract that exists) and implements
`types.Backend`, leaving the rest behind `Client` methods for step 2 to attach.
Its consoles are the honest exception: tickets are implemented, the websocket
bridge is not, so the capability flags say no and the matrix says why.

## Gaps by backend

### qemu — 9 gaps

- **tty** — the serial socket the generated unit already defines
- **rdp** — the same probe and bridge over the hostfwd port
- **clone** — qemu-img convert plus a new unit
- **template** — the same mark in the local registry
- **scale** — rewrite the unit and restart
- **volumes** — qemu-img create plus a unit edit
- **expand** — qemu-img resize while stopped
- **gpu** — vfio-pci in the generated unit
- **tags** — the local registry, which already persists per-VM state

### incus — 11 gaps

- **vnc** — incus console --type=vga for Incus VMs; the web vncBridge handles local, libvirt, and cluster namespaces only
- **rdp** — same, via the instance address
- **migrate** — incus move, including between remotes
- **clone** — incus copy
- **template** — incus publish, or the registry mark
- **scale** — incus config set limits.cpu / limits.memory, live
- **volumes** — incus storage volume attach
- **expand** — incus config device set … size
- **gpu** — incus config device add … gpu
- **tags** — instance config `user.corral.tag.<name>`
- **ports** — incus config device add … proxy

### libvirt — 11 gaps

- **ssh** — the domain's address via the guest agent or DHCP leases, then plain ssh — pkg/libvirt has SSH but the TUI does not offer it because the capability table omits it
- **tty** — virsh console
- **rdp** — same, via the domain address
- **migrate** — virsh migrate --live to another URI
- **clone** — virt-clone
- **template** — the registry mark
- **scale** — virsh setvcpus / setmem
- **volumes** — virsh attach-disk / detach-disk
- **expand** — virsh blockresize
- **gpu** — hostdev in the domain XML
- **tags** — domain metadata

### proxmox — 4 gaps

- **tty** — termproxy tickets are implemented (pkg/proxmoxbe.TermTicket); the web websocket bridge is not wired yet
- **vnc** — vncproxy tickets are implemented (pkg/proxmoxbe.VNCTicket); the web websocket bridge is not wired yet
- **rdp** — same, via the guest address
- **containers** — pkg/proxmoxbe.Containers lists them and Create makes them; pkg/ct does not yet surface a non-Kubernetes CT

## Testing parity

Three layers, each catching what the others cannot:

- **Conformance** (`pkg/backend`) — the claims agree with each other and with
  this document. Pure data, no cluster.
- **Per-backend unit tests with `shell.Fake`** — the right native command is
  issued with the right arguments for each operation, per backend. This is where
  "does Incus LXC actually work" is answered: the commands are asserted, not the
  daemon's behaviour.
- **Real-backend e2e** — `.github/workflows/e2e.yml` runs kind plus emulated
  KubeVirt, and `.github/workflows/e2e-incus.yml` runs a real Incus daemon, a
  real libvirt, and local QEMU on one runner: a triple-backend aggregate
  inventory, the snapshot/export/device adapters against the real tools, and the
  container-versus-VM split asserted in both directions (a container in
  `ct list` and *not* in `list`, a virtual machine the other way round).
  Proxmox cannot run in CI at all; ADR-0009 records `httptest` against recorded
  payloads plus a documented manual pass as the honest substitute — which is the
  same admission `docs/testing.md` makes about real KVM hardware.
