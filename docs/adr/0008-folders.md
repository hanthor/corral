# ADR-0008: Folders — a fleet hierarchy Corral owns

**Status:** proposed
**Date:** 2026-07-31

## Context

Corral aggregates a heterogeneous fleet: QEMU VMs on the workstation,
KubeVirt VMs and pet-pod CTs on one or more clusters, Incus instances on
remotes, libvirt domains behind SSH URIs, and whole other Corrals joined as
peers. The UIs group that fleet by *where it runs* — datacenter → node → VM
in the web tree, context filters in the TUI.

Operators do not think about their fleet that way. They think in
**application stacks** ("the web stack is `web-prod`, `db-prod`, and the
`files` CT"), in **SLA tiers** ("these six can reboot whenever; these two
need a window"), and in **environments**. None of those line up with a
backend, a context, or a node — a stack routinely spans a cluster VM, a
local dev VM, and a CT.

What exists today does not cover it:

- **Tags** (#tags, `corral.dev/tag.<name>` labels) are KubeVirt-only. A
  qemu VM, an Incus instance, and a libvirt domain cannot carry one, which
  is exactly the heterogeneity a stack has. They are also flat and
  multi-valued — good for filtering, wrong for "what is the scope of this
  reboot".
- **Contexts** are backend universes, discovered rather than declared. An
  operator cannot put two contexts' instances in one group.
- **Bulk actions** exist in the web UI, but only over an ad-hoc
  multi-select that vanishes on reload. There is no way to name a group and
  come back to it.

The requested shape is a folder tree: nestable, drag-and-drop, with bulk
actions per folder ("reboot all"). The longer-term prize — and explicitly
not this ADR's scope — is that a named, durable group is the natural place
to hang a **policy**: a backup schedule, a snapshot retention rule, a
permitted downtime window.

The design question this ADR has to answer is not the UI. It is **where
folder membership lives**, because that decides whether folders can span
backends at all, whether two operators see the same tree, and whether the
future policy layer has anything durable to attach to.

## Decision

### Folders are Corral's own object, keyed by canonical instance reference

A folder is a **path** (`prod`, `prod/web-stack`) and a set of members,
each a `types.InstanceRef` — the identity that already carries peer,
backend, context, namespace, and name. That reference is what makes a
folder able to hold a KubeVirt VM, a local qemu VM, an Incus container, and
a CT at once: it is the one identity every backend already produces.

```yaml
folders:
  - path: prod
  - path: prod/web-stack
    members:
      - kubevirt/talos/corral-vms/web-prod
      - kubevirt/talos/corral-vms/db-prod
      - ct/talos/corral-vms/files
  - path: lab
    members:
      - qemu/local//dev-fedora
```

Decisions that follow from that shape:

- **Nesting is the path, not a parent pointer.** `prod/web-stack` is a
  child of `prod` because of its name. Re-parenting — which is what a drag
  is — is a rename of a prefix, which is one write rather than a tree
  walk.
- **A folder exists independently of its members.** An empty folder is a
  declared object, so it can be created before anything is dragged into it
  (and a drag has somewhere to land). This is why the document lists
  folders rather than being a map from instance to folder.
- **One folder per instance.** Folders are a tree, not tags: the scope of
  "reboot this folder" and of any future window or retention rule has to be
  unambiguous. Tags stay as they are — orthogonal, multi-valued, for
  filtering. Membership in two stacks is a modelling error the UI should
  refuse, not represent.
- **Stale members are tolerated on read, pruned on write.** A VM that is
  stopped, unreachable, or on a context that is currently down must not
  silently lose its folder — a partial fleet is a normal state for Corral
  (see the partial-fleet warnings). A member that resolves to nothing is
  shown as missing; it is dropped only when the operator edits that folder.

### Membership lives in Corral's own state, not in backend labels

Locally, the folder document lives in `~/.config/corral/config.yaml`
alongside contexts and peers. When `corral web` runs in-cluster, it lives
in a ConfigMap — **the same split image sources already use**, so there is
precedent, a working pattern, and no new storage concept.

This is the decision the scoping conversation deferred to this ADR, and it
is chosen over the alternatives below because it is the only option that
covers the whole fleet on day one. Backend-native labels can hold a folder
for a KubeVirt VM and a CT, and config keys can for Incus, and domain
metadata can for libvirt — but a local qemu VM managed through systemd
units has nowhere to put one, and local VMs are half of why Corral exists.
A grouping feature that silently cannot hold a local VM is not a grouping
feature.

The cost is stated plainly: **the tree is per-Corral, not per-instance.**
Two operators with separate configs see separate trees, and an instance
carries no record of its folder if you look at it through `kubectl`. That
is acceptable because a folder is an *operator's* view of the fleet, the
same way contexts and peers already are — and the shared case is already
served by the in-cluster deployment, where the ConfigMap is the shared
tree.

### Bulk actions fan out server-side, with per-member results

`POST /api/folders/{path}/{action}` applies a power action to every member
and returns a per-member outcome, rather than the UI looping over per-VM
endpoints. Three reasons:

1. Partial failure is the normal case in a heterogeneous folder (an action
   valid for a KubeVirt VM may be refused for an Incus container), and one
   response that says which members did what is honest where a pile of
   toasts is not.
2. Capability gating already exists per instance; a fan-out endpoint can
   report "not applicable here" without the UI re-deriving it.
3. The eventual scheduler needs a server-side executor for exactly this
   operation. Building it now means the policy layer inherits it instead of
   growing a parallel path.

Actions are gated by the same `types.InstanceCapabilities` the single-
instance paths use, and destructive ones (delete) are **not** offered as a
folder operation in this slice.

### Surfaces

- **TUI**: folders group the fleet list, collapsible, with the same
  capability-aware action menu opening on a folder to act on its members.
  Keyboard first; the mouse support added alongside this makes click-to-
  expand and drag-to-move possible later.
- **Web**: folders become a branch of the existing tree, with drag-and-drop
  re-parenting (a `PUT` of the member's folder path) and the folder's own
  action toolbar.

## Consequences

- One new package (`pkg/folder`) owning the document, path validation,
  re-parenting, and membership; the surfaces stay thin over it.
- `GET /api/v1/inventory` gains a folder path per instance, so a client can
  render the tree without a second round trip.
- The Proxmox compat layer has a natural mapping to lean on later —
  PVE **pools** are close to this shape — but this ADR does not claim it.
- Peers: a folder may hold another Corral's instances, since an
  `InstanceRef` carries the peer. The folder document stays local to the
  Corral that renders the tree; peers are not asked about their folders.

## Alternatives considered

**Backend-native labels** (`corral.dev/folder=prod/web-stack` on KubeVirt
VMs and CT pods, Incus config keys, libvirt domain metadata). Durable with
the instance and shared between operators for free. Rejected as the primary
store: local qemu has no home for it, so the feature would be unavailable
exactly where Corral is most often run first, and every backend needs its
own read/write/list path.

**Hybrid** — labels where they exist, config for the rest. Most durable,
but two sources of truth to reconcile, and reconciliation bugs in a
grouping feature show up as instances silently jumping folders. If sharing
demand grows, the config document can later be *mirrored* to labels as a
one-way export without changing the model.

**Tags as folders** — synthesise a hierarchy from tag values like
`folder/prod/web`. No new storage, but it inherits the KubeVirt-only limit,
allows an instance in two folders, and overloads a filtering primitive with
tree semantics.

## Not in scope

Backup and downtime policy. A folder is the object those will attach to,
and `pkg/snapshot`'s contract (per-instance consistency reporting and typed
refusals) is the mechanism they will drive, with the snapsched plugin
consuming both. Designing the policy object — windows, retention, what
happens when a member cannot honour it — is a separate ADR, and should not
constrain the folder shape beyond the "one folder per instance" and
"server-side fan-out" decisions above, which exist so that it can.
