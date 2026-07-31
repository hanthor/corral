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
| List / inventory | ✅ | ✅ | ✅ | ✅ | 🔨 |
| Create | ✅ | ✅ | ✅ | ✅ | 🔨 |
| Start | ✅ | ✅ | ✅ | ✅ | 🔨 |
| Stop | ✅ | ✅ | ✅ | ✅ | 🔨 |
| Restart | ✅ | ✅ | 🔨 | 🔨 | 🔨 |
| Pause / resume | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Delete | ✅ | ✅ | ✅ | ✅ | 🔨 |
| SSH | ✅ | ✅ | ✅ | 🔨 | 🔨 |
| Serial / shell console | ✅ | 🔨 | ✅ | 🔨 | 🔨 |
| Graphical console (VNC) | ✅ | ✅ | 🔨 | ✅ | 🔨 |
| RDP | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Live CPU / memory | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Snapshot / restore | ✅ | ✅ | ✅ | ✅ | 🔨 |
| Migrate | ✅ | — | 🔨 | 🔨 | 🔨 |
| Clone | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Template mark | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| CPU / memory edit | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Add / remove disks | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Expand disk | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| GPU passthrough | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Export / backup disk | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Events | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
| Tags | ✅ | 🔨 | 🔨 | 🔨 | 🔨 |
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

**2. Generalise the adapter contract.** Grow `types.Backend` — or better, a set
of small optional interfaces alongside it, one per operation family (`Power`,
`Console`, `Sizing`, `Storage`, `Observability`, `Lifecycle`) — so a backend
declares what it implements and the capability table is *derived* from that
rather than hand-maintained. `pkg/snapshot.Adapter` is the shape to copy, and
`pkg/backend` is where the registry belongs.

**3. Close the gaps, cheapest-first per backend.** The lists below come from the
matrix, so they stay current. The notes name the native mechanism, so none of
these start from a blank page.

**4. Add the Proxmox backend** per ADR-0009. It arrives after step 2 so it lands
on the contract rather than adding a sixth `switch` arm.

## Gaps by backend

### qemu — 13 gaps

- **pause** — QMP stop/cont on the unit's QMP socket, which pkg/qemu already creates
- **tty** — the serial socket the generated unit already defines
- **rdp** — the same probe and bridge over the hostfwd port
- **metrics** — QMP query-status / host cgroup accounting for the unit
- **clone** — qemu-img convert plus a new unit
- **template** — the same mark in the local registry
- **scale** — rewrite the unit and restart
- **volumes** — qemu-img create plus a unit edit
- **expand** — qemu-img resize while stopped
- **gpu** — vfio-pci in the generated unit
- **export** — copy or convert the disk while stopped
- **events** — journalctl --user for the unit
- **tags** — the local registry, which already persists per-VM state

### incus — 16 gaps

- **restart** — incus restart — Corral stops then starts instead
- **pause** — incus pause / incus start
- **vnc** — incus console --type=vga for Incus VMs; the web vncBridge handles local, libvirt, and cluster namespaces only
- **rdp** — same, via the instance address
- **metrics** — incus info or GET /1.0/instances/{name}/state
- **migrate** — incus move, including between remotes
- **clone** — incus copy
- **template** — incus publish, or the registry mark
- **scale** — incus config set limits.cpu / limits.memory, live
- **volumes** — incus storage volume attach
- **expand** — incus config device set … size
- **gpu** — incus config device add … gpu
- **export** — incus export
- **events** — incus monitor, or the events websocket
- **tags** — instance config user.corral.tag.<name>
- **ports** — incus config device add … proxy

### libvirt — 16 gaps

- **restart** — virsh reboot — Corral stops then starts instead
- **pause** — virsh suspend / virsh resume
- **ssh** — the domain's address via the guest agent or DHCP leases, then plain ssh — pkg/libvirt has SSH but the TUI does not offer it because the capability table omits it
- **tty** — virsh console
- **rdp** — same, via the domain address
- **metrics** — virsh domstats
- **migrate** — virsh migrate --live to another URI
- **clone** — virt-clone
- **template** — the registry mark
- **scale** — virsh setvcpus / setmem
- **volumes** — virsh attach-disk / detach-disk
- **expand** — virsh blockresize
- **gpu** — hostdev in the domain XML
- **export** — copy or convert the backing volume
- **events** — virsh event / domain lifecycle events
- **tags** — domain metadata

### proxmox — 24 gaps

- **list** — GET /cluster/resources?type=vm — ADR-0009
- **create** — POST /nodes/{node}/qemu — ADR-0009
- **start** — POST /nodes/{node}/qemu/{vmid}/status/start
- **stop** — POST /nodes/{node}/qemu/{vmid}/status/shutdown
- **restart** — POST …/status/reboot
- **pause** — POST …/status/suspend and /resume
- **delete** — DELETE /nodes/{node}/qemu/{vmid}
- **ssh** — guest agent network-get-interfaces, then plain ssh
- **tty** — POST …/termproxy + /vncwebsocket
- **vnc** — POST …/vncproxy + GET /vncwebsocket
- **rdp** — same, via the guest address
- **metrics** — GET …/rrddata and /status/current
- **snapshots** — POST …/snapshot, /rollback — ADR-0009
- **migrate** — POST …/migrate with online=1
- **clone** — POST …/clone, full or linked
- **template** — POST …/template — PVE has the concept natively
- **scale** — POST …/config cores/memory, hotplug where enabled
- **volumes** — POST …/config scsiN / unlink
- **expand** — PUT …/resize
- **gpu** — hostpci0 in the VM config
- **export** — vzdump, then download the archive
- **events** — GET /nodes/{node}/tasks and the task log
- **tags** — the config's own tags field — PVE has tags natively
- **containers** — /nodes/{node}/lxc — PVE containers map onto Corral CTs, ADR-0009


## Testing parity

Three layers, each catching what the others cannot:

- **Conformance** (`pkg/backend`) — the claims agree with each other and with
  this document. Pure data, no cluster.
- **Per-backend unit tests with `shell.Fake`** — the right native command is
  issued with the right arguments for each operation, per backend. This is where
  "does Incus LXC actually work" is answered: the commands are asserted, not the
  daemon's behaviour.
- **Cluster e2e** (`.github/workflows/e2e.yml`) — kind plus emulated KubeVirt for
  the cluster backend. There is no Incus or libvirt equivalent in CI yet; adding
  an Incus job is cheap (a daemon in a container) and is the next CI gap worth
  closing, since Incus is the backend with the most 🔨 that a human can actually
  run on a laptop.
