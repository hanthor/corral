# ADR-0009: Proxmox VE as a backend

**Status:** proposed
**Date:** 2026-07-31

## Context

Corral already speaks Proxmox in one direction: `pkg/proxmox` and the
`corral proxmox` plugin **serve** a subset of the PVE REST API, translating
`/api2/json/…` onto KubeVirt so Proxmox-shaped tooling can point at Corral.
This ADR is the **other direction** — a real Proxmox VE cluster as a Corral
backend, listed and driven alongside qemu, KubeVirt, Incus, and libvirt.

The two must not be confused, and they do not share code beyond types: the
compat layer answers PVE-shaped questions about Corral's fleet; the backend asks
PVE-shaped questions of somebody else's cluster.

Proxmox is the most-requested backend for the obvious reason: it is what a large
share of the homelab and small-shop world already runs, and Corral's entire
vocabulary — VMs and Containers, nodes, snapshots, templates, pools, live
migration — was borrowed from it. The mapping is closer than for any backend
Corral already has.

It arrives against a specific constraint recorded in `docs/backend-parity.md`:
the rich operations are currently reached through `if backend == "kubevirt"`
branches, and adding a sixth arm to 33 switch sites would make Proxmox
"best effort" on arrival. So this ADR specifies the mapping in full, and
sequences the implementation **after** the operation contract that parity work
introduces.

## Decision

### Transport: the HTTPS API with an API token

`https://{host}:8006/api2/json` with an `Authorization: PVEAPIToken=…` header.
Not `pvesh` over SSH, and not ticket-plus-CSRF login:

- Every other backend shells out to a CLI because that CLI *is* the supported
  interface (`kubectl`, `virtctl`, `incus`, `virsh`). Proxmox's supported
  interface is the REST API; `pvesh` only exists on the node itself.
- API tokens are revocable, scopeable per privilege, and do not expire like
  tickets — the same reasoning as ADR-0003 on identity.
- It means the **first backend that is a Go HTTP client** rather than a command
  runner. The `shell.Runner` seam does not apply, so the backend takes an
  `http.RoundTripper` seam instead, and tests use `httptest` the way `pkg/web`'s
  do.

Self-signed certificates are the norm on PVE. The context configuration carries
either a pinned certificate fingerprint or an explicit
`insecure_skip_verify: true` — never a silent skip, and never a global TLS
downgrade.

### Identity: context is a cluster, node is a node, vmid is not the name

| Corral | Proxmox | Notes |
|---|---|---|
| Context | one PVE cluster endpoint | Named in `contexts:` as `backend: proxmox`, carrying host, token, and TLS trust |
| Node | PVE node | Reported per instance, and the target for migration |
| Namespace | *unused* → `""` | PVE has no namespace; the field stays empty rather than being repurposed |
| Name | the VM/CT `name` | PVE names are per-cluster, not unique across clusters, which `InstanceRef` already handles |
| — | `vmid` | Carried as an opaque backend identifier, resolved by name at the edges |

`vmid` is the awkward one. PVE addresses everything by a cluster-wide integer,
Corral addresses everything by name. The backend keeps a name→vmid map
refreshed from `/cluster/resources` and resolves at call time. Bare-name
selectors stay a Corral concern: if two nodes in one cluster hold the same
name, PVE itself refuses, so no ambiguity is introduced.

**PVE pools map to folders** (ADR-0008), not to namespaces. A pool is an
operator's grouping of instances, which is what a folder is; the folder tree
gains an import path (`/pools`) rather than pools becoming a second grouping
concept.

### Feature mapping

Everything Corral ships, and the PVE call that implements it. `{n}` is the node,
`{id}` the vmid, and `qemu` becomes `lxc` for containers throughout.

**Inventory and lifecycle**

| Corral operation | Proxmox |
|---|---|
| List | `GET /cluster/resources?type=vm` — one call for the whole cluster, VMs and CTs together, with node, status, cpu, mem, and tags |
| Create (VM) | `POST /nodes/{n}/qemu` — `cores`, `memory`, `scsi0`, `ide2` for an ISO, `net0`, `cipassword`/`sshkeys`/`ciuser` for cloud-init |
| Create (CT) | `POST /nodes/{n}/lxc` — `ostemplate`, `rootfs`, `unprivileged` |
| Start / Stop | `POST /nodes/{n}/qemu/{id}/status/start` / `/shutdown` (`/stop` only as the forced fallback) |
| Restart | `POST …/status/reboot` — a real reboot, not Corral's stop-then-start |
| Pause / Resume | `POST …/status/suspend` / `/resume` |
| Delete | `DELETE /nodes/{n}/qemu/{id}` with `purge=1` so jobs and backups do not dangle |

**Access**

| Corral operation | Proxmox |
|---|---|
| VNC console | `POST …/vncproxy` (`websocket=1`) then `GET …/vncwebsocket?port=&vncticket=` — bridged to the existing noVNC front end exactly as `vncBridge` bridges `virtctl vnc --proxy-only` |
| Serial / shell | `POST …/termproxy` then the same websocket — xterm.js, unchanged |
| SSH | the guest address from `GET …/agent/network-get-interfaces` (VM) or `GET …/interfaces` (CT), then plain `ssh` |
| RDP | the same guest-dependent probe and IronRDP bridge as ADR-0002; PVE plays no part beyond giving the address |

SPICE is out of scope: no browser client Corral can drive.

**Data and shape**

| Corral operation | Proxmox |
|---|---|
| Snapshots | `POST …/snapshot`, `GET …/snapshot`, `POST …/snapshot/{name}/rollback`, `DELETE …/snapshot/{name}` — a new `pkg/snapshot` adapter. Consistency: `Filesystem` when taken with `vmstate=1` or the guest agent's `fs-freeze`, `Crash` for a running VM without either, `Offline` when stopped |
| Migrate | `POST …/migrate` with `target` and `online=1`; `GET …/migrate` for preconditions, which is a better pre-flight than Corral's current `liveMigratable` guess |
| Clone | `POST …/clone`, `full=0` for linked or `1` for full |
| Template | `POST …/template` — PVE has the concept natively, so the mark is real rather than a Corral label |
| CPU / memory | `POST …/config` with `cores`/`memory`; hotplug where the guest has it enabled, restart otherwise — the same honest note the hardware form already shows |
| Disks | `POST …/config` with `scsiN`, `PUT …/resize`, `unlink` to remove |
| GPU | `POST …/config` with `hostpciN` |
| Export / backup | `POST /nodes/{n}/vzdump` then download from the storage — mapping to the existing export task and its progress reporting |
| Tags | the config's own `tags` field, comma-separated — native, unlike the label emulation elsewhere |
| Events | `GET /nodes/{n}/tasks` plus `GET /nodes/{n}/tasks/{upid}/log` |
| Metrics | `GET …/status/current` for the instant, `GET …/rrddata?timeframe=hour` for the CPU sparkline the web UI already draws |

**Containers**

PVE containers are LXC, and they map onto Corral's CT concept directly —
`unprivileged` is 1:1 with the Privileged checkbox ADR-0005 already models, and
`pct exec` equivalents come through `termproxy`. A PVE CT is closer to Corral's
CT than Corral's own pet-pod is to LXC, so this is the one place the Proxmox
backend is *more* natural than the reference backend.

**Not mapped:** HA groups, firewall rules, replication jobs, Ceph management,
cluster join. Corral does not have those concepts, and inventing them for one
backend is how parity dies.

### Everything asynchronous returns a UPID

Most PVE mutations return a task id (`UPID:node:…`) rather than completing
inline. The backend treats a UPID the way the web layer already treats its own
tasks: register it with `taskBegin`, poll `GET /nodes/{n}/tasks/{upid}/status`,
and surface progress through the existing task-log endpoint. This is why
migration, export, and clone get real progress reporting on this backend for
free, and it is worth noting that the *existing* backends' progress reporting is
weaker than what PVE hands over.

### Capabilities are declared by what is implemented

The backend must not get a hardcoded row in `types.CapabilitiesForBackend`.
It declares the operation interfaces it implements, and the capability flags are
derived — the mechanism `docs/backend-parity.md` step 2 introduces. Until that
exists, the Proxmox row in `pkg/backend.Matrix` stays entirely `Possible`, which
is exactly what the matrix is for.

Per-instance capabilities are then narrowed by what the cluster actually reports:
no guest agent means no SSH and no RDP probe, a storage without snapshot support
means no snapshots for instances on it, one node means no migration.

## Consequences

- A new package `pkg/proxmoxbe` (the name matters: `pkg/proxmox` is the compat
  server and must not become ambiguous), plus a context type and doctor checks
  for reachability, token validity, TLS trust, and privilege coverage.
- The first backend needing an HTTP client seam; tests use `httptest` with
  recorded PVE payloads rather than `shell.Fake`.
- The Proxmox compat layer becomes able to front a real Proxmox cluster, which
  is either delightful or a support hazard depending on how it is documented.
- Peers and Proxmox stack: a Corral aggregating a PVE cluster can itself be a
  peer, so a PVE instance can appear in another Corral's tree. `InstanceRef`
  already carries this.
- CI: a real PVE cannot run in GitHub Actions. Coverage is `httptest` against
  recorded payloads, plus a documented manual matrix run against a real cluster
  before release — the same honesty `docs/testing.md` applies to KVM hardware.

## Alternatives considered

**SSH plus `pvesh`/`qm`/`pct`.** Fits the existing `shell.Runner` seam and every
backend's shape, and needs no token. Rejected: it requires root SSH to a node,
breaks the moment that node is the one being migrated away from, has no
cluster-wide entry point, and gives up the task ids the API returns.

**Reuse `pkg/proxmox`'s types for both directions.** Tempting for symmetry, and
wrong: the compat layer's shapes are what Corral *emits* to satisfy old clients,
and coupling them to what a real PVE *returns* would make one break the other.
Shared code, if any, is a payload package neither side owns.

**Terraform/Ansible provider as the transport.** Rejected — a dependency on
somebody else's state model to perform stateless operations.

## Not in scope

The operation contract itself. This ADR depends on it and describes what
Proxmox needs from it, but the contract is a parity concern for every backend
and belongs in its own change (`docs/backend-parity.md`, step 2).
