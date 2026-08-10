# VDI plugin — desktop pools

`corral-vdi` is the Phase 1 implementation of
[RFC-0001](rfc/0001-vdi-plugin.md): static desktop pools, built by cloning
an already-built VM, with manual (CLI-driven) assignment. No broker, no
self-serve web page, no idle reclaim yet — those are later phases, tracked
in [issue #69](https://github.com/tuna-os/corral/issues/69). What's here is
real and cluster-tested: create a pool, hand a member to a user, connect,
release it, delete the pool.

## Install

```bash
corral plugin install vdi
```

Or build from source (same repo, no separate checkout):

```bash
go build -o ~/.local/share/corral/plugins/corral-vdi ./cmd/corral-vdi
```

## Mental model

A pool is **not** a new kind of object — it's just VMs with a
`corral.dev/vdi-pool=<name>` label. There's no pool CRD, no controller
watching anything, no reconciliation loop. `corral vdi pool create` clones
N VMs and labels them; `corral vdi pool list` reads the labels back;
`corral vdi pool delete` deletes the labeled VMs. If you're ever unsure
what a command actually did, `kubectl get vm -n <ns> -l corral.dev/vdi-pool`
shows you the truth directly — there's no other state to go stale.

Assignment works the same way: `corral.dev/vdi-assigned-to=<user>` label
plus a `corral.dev/vdi-claimed-at` annotation (RFC3339 timestamp) on the
member VM. Nothing lives outside the K8s API.

## Prerequisites

- A **golden VM** — an already-built, already-working VM you want to make
  copies of. Build it the normal way:
  - Desktop Linux: `corral bootc create mydesktop --image ghcr.io/ublue-os/bluefin:latest`
  - Windows: `corral windows create mydesktop --iso <url>` (see the create
    dialog's ISO presets in the web UI, or `docs/api.md`'s note on
    `POST /api/vms` with `windows: true`)
  - Anything else: `corral create mydesktop --kubevirt ...`
- The golden VM's clone needs whatever the source needs — same
  StorageClass availability, same node placement constraints. `corral
  doctor` catches most of this before you find out the hard way.
- **KubeVirt's clone feature** needs a `VolumeSnapshotClass` for
  persistent-disk VMs (the same requirement `corral clone`/snapshot
  already has — `corral doctor` flags a missing one).

## Walkthrough

```bash
# 1. Build (or reuse) a golden VM. This one's a normal corral bootc VM.
corral bootc create golden-desktop --image ghcr.io/ublue-os/bluefin:latest
corral start golden-desktop
# ...customize it however you like (install packages, configure things)...
corral stop golden-desktop   # clone from a stopped VM for a clean disk state

# 2. Create a pool of 3 clones.
corral vdi pool create devpool --from golden-desktop --size 3
#   pool "devpool" created: 3 members
#     devpool-1
#     devpool-2
#     devpool-3

# 3. See what's in it.
corral vdi pool list
#   devpool  (ns/corral-vms, 3 members)
#     devpool-1                free                     stopped
#     devpool-2                free                     stopped
#     devpool-3                free                     stopped

# 4. Hand one to a user. This also starts it if it was stopped.
corral vdi assign devpool alice
#   assigned devpool-1 → alice
#   connect:  corral vdi connect devpool-1

# 5. Connect. Prints every reachable path — pick whichever fits the guest.
corral vdi connect devpool-1
#   VNC (browser or client):  corral web  →  open devpool-1  →  Console
#   RDP (if the guest answers on 3389):  corral viewer devpool-1  (or a native RDP client via virtctl port-forward)
#   SSH (Linux guests):  corral ssh devpool-1

# 6. When alice is done, release it. This stops the VM too — pooled
#    desktops don't stay running unclaimed.
corral vdi unassign devpool-1

# 7. Tear the whole pool down when you're finished with it.
corral vdi pool delete devpool
```

## What "connect" actually does today

`corral vdi connect <member>` prints instructions — it does **not** yet
pick a protocol and open a session for you. That one-click behavior is
Phase 2 territory (see the RFC), and specifically depends on
[ADR-0002](adr/0002-browser-rdp-via-ironrdp.md) phase 2 (in-browser RDP)
landing first, so Windows members get the same one-click experience VNC
already gives Linux members. Until then:

| Guest | How to actually connect |
|---|---|
| Any VM (VNC always works) | `corral web` → open the VM → Console tab (noVNC in the browser) |
| Linux VM with RDP configured | `GET /api/vms/{ns}/{name}/rdp` (or the Summary panel) tells you if 3389 answers; connect with a native RDP client via `virtctl port-forward` |
| Windows VM | Same RDP path as above — `corral-windows`-created VMs expose RDP through the proxy service if you passed `--rdp` at create time |
| Any VM with SSH | `corral ssh <member>` |

## Troubleshooting

- **`golden VM "X" not found`** — the `--from` VM doesn't exist in the
  target namespace. `corral vdi pool create` doesn't search other
  namespaces; pass `-n` to match wherever the golden VM actually lives.
- **`timed out ... waiting for the clone to produce VM`** — KubeVirt's
  clone controller didn't produce the target VM within 2 minutes. Check
  `kubectl get virtualmachineclone -n <ns>` for the clone object's phase;
  a stuck clone is almost always a StorageClass/VolumeSnapshotClass issue
  (see Prerequisites above) or the source VM's PVC not being
  snapshottable. This is a real, live-found failure mode — see the RFC's
  commit history for the bug that motivated the wait/timeout logic.
- **`pool has no free members`** — every member is currently assigned.
  Either `corral vdi unassign` one, or `corral vdi pool create` a bigger
  pool (there's no live resize yet — delete and recreate, or create a
  second pool).
- **Assigned member won't start** — `corral vdi assign` surfaces the
  underlying `virtctl start` error directly (e.g. insufficient cluster
  capacity, a feature-gate the golden VM's spec needs). The assignment
  label is still set even if start failed — `corral vdi unassign` to back
  out and retry once the underlying problem's fixed.

## Current limitations (Phase 1 — by design, not bugs)

- **No self-serve.** Assignment is a CLI/admin action — there's no
  end-user "get a desktop" page yet.
- **Assignment is still operator-driven, but claims are concurrency-safe.**
  `corral vdi assign` acquires a Kubernetes Lease before writing the member
  labels, so simultaneous CLI invocations cannot claim the same desktop. A
  broker or authenticated self-serve page is still Phase 2 work.
- **No idle/logout reclaim.** Unassigned-but-still-running members don't
  happen (unassign always stops), but there's nothing that notices "alice
  hasn't touched devpool-1 in 3 hours" and reclaims it automatically.
- **No live resize.** Growing or shrinking a pool means deleting and
  recreating it, or manually cloning one more member and hand-labeling it.
- **No GPU-gating on pool create yet.** If your golden VM needs a GPU,
  every clone needs one too — nothing stops you from creating a pool
  bigger than your cluster's GPU capacity.
- **CT-backed pools aren't implemented.** Only VM (KubeVirt) golden
  sources work today; Containers (CT) pooling is exploratory (RFC's Phase
  4 territory).

None of this is hidden — see [RFC-0001](rfc/0001-vdi-plugin.md) for the
full phased plan and [issue #69](https://github.com/tuna-os/corral/issues/69)
for what's tracked next.
