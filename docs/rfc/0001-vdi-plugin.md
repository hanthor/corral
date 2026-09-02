# RFC-0001: VDI plugin — Windows/Linux desktop pools on Corral

**Status:** Phases 0 and 1 implemented. Browser RDP via IronRDP/RDCleanPath
and the `corral-vdi` static-pool CLI are shipped. Phases 2–4 remain a
design proposal; this RFC defines their corrected scope but does not commit
their implementation. Setup guide: [docs/vdi.md](../vdi.md), epic status: [docs/vdi-epic-status.md](../vdi-epic-status.md).
**Date:** 2026-07-02
**Updated:** 2026-07-29
**Author:** grilled out of a live session with James Reilly + Claude

## Summary

Add a `corral-vdi` plugin that turns Corral's existing VM/Container machinery
into a small, self-hosted **Virtual Desktop Infrastructure**: pools of
Windows or Linux desktops, assigned to users on request, reached through the
browser, and reclaimed by explicit release or a conservative session policy.
Not a Citrix/Horizon replacement — a homelab-to-small-team-scale VDI that
keeps optional broker/auth concerns in plugins and works through Tailscale or
another private ingress, following the scope discipline that keeps Corral's
core lean.

## Why now

Corral already has almost every *component* a VDI stack needs, built for
other reasons:

| VDI need | Already exists in Corral as |
|---|---|
| Remote display in the browser | noVNC bridge (`/api/vnc/{ns}/{name}`), xterm.js serial bridge |
| Browser RDP | IronRDP over the RDCleanPath bridge, plus RDP detection and raw websocket transport (ADR-0002 phases 1 and 2) |
| Windows guest provisioning | `corral-windows` plugin — UEFI/TPM/virtio, driver ISO |
| Full desktop Linux images | `corral bootc` — builds Universal Blue/Bluefin/TunaOS desktop images on-cluster from a container image |
| Lightweight ephemeral Linux sessions | Containers (CT) — `pkg/ct`, distrobox-style persistent rootfs |
| Reachability | direct-first console routing, Tailscale exposure, ingress-agnostic KubeVirt Services, and Corral-peer relay fallback |
| Identity | trusted Tailscale identity or the optional OIDC auth-gateway plugin (ADR-0003/ADR-0006) |
| Cluster capability gating | `corral doctor` — GPU/PCI passthrough check, StorageClass checks |
| Replica pools of identical VMs | KubeVirt's own `VirtualMachinePool` CRD |

What's **missing** is the thin layer that turns "a pool of VMs" into "VDI":
**assignment** (which user gets which desktop), **lifecycle policy**
(power on demand, reclaim on idle/logout), and **one connect button** that
picks the right protocol per desktop. That layer — not new console
plumbing, not a new hypervisor integration — is the actual scope of this
RFC.

## What we're explicitly *not* building

Grounded in the research (full brief: link in the tracking issue) — these
are deliberate exclusions, not oversights:

- **Not a new remote-display protocol.** noVNC/RDP-over-websocket stay. SPICE
  is dead upstream (QEMU is dropping SPICE server support entirely as of a
  2025 kubevirt-dev thread) — do not build on it.
- **Not a general-purpose connection broker product.** oVirt's engine+portal
  architecture and Leostream (commercial, KubeVirt-aware since 2025) are the
  reference points for the *pattern* (pool + ticket + assignment), not
  something to run alongside Corral as a second control plane.
- **Not solving Windows licensing.** Pooled/multi-session Windows needs
  Enterprise E3/E5 or RDS CALs — a customer-facing constraint this RFC flags
  and moves on from, not a technical problem Corral can solve.
- **Not GPU virtualization plumbing.** KubeVirt + NVIDIA's GPU Operator
  already handle vGPU/mediated-device passthrough; `corral doctor` already
  checks for it (this session's GPU/PCI passthrough check). Reuse, don't
  rebuild.

## Prior art, and how this differs

- **oVirt** (community-maintained, still shipping releases into 2026):
  proves the pool+ticket broker pattern. Non-Kubernetes-native — not
  reusable directly, pattern only.
- **Leostream**: commercial, added OpenShift Virtualization/KubeVirt support
  ~end of 2025. Validates "external broker drives KubeVirt API" as a real,
  chosen architecture by someone solving the same problem commercially.
  Closed-source — differentiate from, don't depend on.
- **Kasm Workspaces**: as of v1.16 provisions *real VMs* via Harvester/
  KubeVirt in addition to its container-native workspaces — independently
  converging on the same "containers for ephemeral, VMs for persistent"
  split this RFC proposes for Corral's CT + bootc combination. Strongest
  external validation found that the dual-workload approach isn't a hack.
- **Selkies** (Google-originated, actively developed): GPU-accelerated
  WebRTC desktop streaming, container-first. Not a drop-in noVNC
  replacement for VM consoles (streams from inside a pod, not a VM
  framebuffer) but its prebuilt desktop container images (KDE Plasma,
  Wine/Proton, GPU passthrough) are a strong candidate for what runs
  *inside* a Corral CT-based ephemeral desktop pool — reuse the images,
  not the whole project.
- **FOSDEM 2026 "VDI and KubeVirt"** talk (KubeVirt maintainer, USB
  redirection/console work): proposes QEMU's D-Bus display interface as a
  pluggable remote-display seam so external bridges (Guacamole, Selkies,
  custom) can attach without owning the whole console pipeline. No shipped
  code yet — track this, because if it lands upstream it may be a better
  long-term seam than `virtctl vnc --proxy-only` for phase 3+ below.

## Design

### Concepts (candidates for CONTEXT.md once this settles)

- **Desktop Pool** — a named group of identical desktops (VM- or CT-backed),
  a template (bootc image / Windows ISO+answer-file / CT image), a target
  size, and a reclaim policy. Built on KubeVirt's `VirtualMachinePool` for
  the VM-backed case; a thin equivalent for CT-backed pools (no upstream
  CRD exists for pooled pods — this is new code, small).
- **Assignment** — an exclusive claim from an authenticated identity to one
  pool member. Identity can come from trusted Tailscale ingress or the OIDC
  auth gateway; VDI consumes the resulting identity contract rather than
  depending on either provider directly. Phase 1's labels are presentation
  state, not a concurrency primitive. Self-service assignment must use an
  atomic Kubernetes operation (a per-member `Lease` or resource-versioned
  compare-and-swap) so simultaneous claims cannot receive the same desktop.
- **Session** — the active connection associated with an Assignment. An open
  console websocket proves only that a connection exists, not that the user
  is actively providing keyboard or mouse input. Disconnect time, explicit
  release, maximum session duration, and an optional guest-reported activity
  signal are distinct inputs to reclaim policy.
- **Connect** — one button/command that resolves a desktop's *actual*
  reachable protocol (RDP probe already exists; extend the same idea to
  "is this a VNC-only guest, RDP-capable, or a CT with just a terminal")
  and opens the right client path — today: existing noVNC/xterm.js bridges
  plus in-browser IronRDP. Routing is direct-first: use an advertised guest or
  ingress endpoint when reachable, then fall back to a Corral-peer console
  relay for complicated network topologies.

### Phased plan

**Phase 0 — in-browser RDP prerequisite. Implemented.**
ADR-0002 phase 2 shipped: the web UI embeds IronRDP and Corral implements the
RDCleanPath transport it requires. Windows and RDP-enabled Linux desktops can
therefore use the same one-click browser experience as VNC desktops.

**Phase 1 — static pools, manual assignment (CLI, no broker yet). Implemented.**
`corral vdi pool create <name> --from <golden-vm> --size N` clones an
*already-built* VM (built the normal way — `corral bootc`/`corral-windows`/
`corral create` — then customized and stopped) N times via
`kubevirt.Client.Clone`, labeled as pool members. `corral vdi assign <pool>
<user>` hand-wires a claim (a K8s label/annotation, nothing fancier).
`corral vdi connect <member>` prints the existing VNC/RDP/SSH paths for
that member. Full setup guide: [docs/vdi.md](../vdi.md).

Landed slightly differently than first drafted above: `--from <existing-vm>`
(clone a golden VM) rather than `--template <image>` (build N from
scratch) — cloning reuses `corral clone`'s already-tested primitive
directly and matches how real VDI systems build pools (golden image once,
clone many), instead of re-running a full bootc build or Windows ISO
install N times. A real bug was found and fixed during live verification:
`Clone()` returns as soon as the `VirtualMachineClone` CRD is applied, not
once the target VM actually exists — `CreatePool` originally raced ahead
and tried to label a VM that didn't exist yet on a real cluster. Fixed
with a poll-wait (`waitForVM`, 2min timeout) between clone and label.

**Phase 2 — atomic self-service claims, sessions, and reclaim. Proposed.**
An authenticated user hits "Get a desktop," atomically claims an available
member, powers it on if necessary, and is redirected to the best reachable
console. Claim creation must be compare-and-swap safe; the existing
list-then-label Phase 1 implementation is not sufficient under concurrent
requests.

The first reclaim policy is deliberately conservative:

1. explicit "Release desktop" is authoritative;
2. console disconnect starts a configurable grace period;
3. reconnect during the grace period cancels reclaim;
4. a maximum session duration is an optional administrative ceiling; and
5. true input-idle reclaim requires an optional guest/protocol activity
   signal and is not inferred merely from websocket age.

After reclaim, persistent pools stop the VM and retain its disks. Ephemeral
pools destroy and recreate the member from the golden source. CT-backed pools
remain a later sub-slice because they need their own reconciliation primitive.
This phase is the actual VDI broker rather than another VM-management command.

**Phase 3 — capacity-aware GPU pools and native USB redirection. Proposed.**
Pool creation inspects the golden VM's host-device requests and compares the
requested pool size with allocatable matching devices. It must distinguish
exclusive passthrough from mediated/vGPU capacity; a generic "GPU present"
doctor result is not enough to promise concurrent desktops.

USB redirection is split by client type. A native CLI command can wrap
`virtctl usbredir` for smartcards and security keys. Browser USB redirection
is a separate experimental feature requiring WebUSB permission UX and a
purpose-built bridge; it is not implied by the CLI transport and is not part
of the initial Phase 3 commitment.

**Phase 4 (exploratory, not committed) — WebRTC streaming for ephemeral
Linux pools.** Selkies-style container images inside Corral CTs for
GPU-accelerated, low-latency ephemeral desktops, as an alternative to
noVNC for that specific pool type. The time-boxed
[WebRTC spike decision record](../research/vdi-webrtc-spike.md) currently
defers implementation: it requires a representative cluster comparison
against VNC before proceeding. Track the FOSDEM D-Bus-display proposal
before committing engineering time here — it may change the right
integration seam.

## Feasibility, honestly

- **Phases 0 and 1 are shipped.** Phase 1 was low-risk assembly of existing
  Corral machinery (bootc, corral-windows, corral ct, KubeVirt's own
  `VirtualMachinePool`), not new hard problems.
- **Phase 2 claim selection is straightforward, but concurrency is not
  optional.** A Lease or compare-and-swap claim must precede VM startup.
- **Phase 2's genuinely hard part is input-idle detection.** Websocket
  open/close is useful session-presence data, but it is not user activity.
  Start with explicit release plus disconnect grace; add guest cooperation
  only when the operational value justifies per-OS integration.
- **Phase 3's GPU story is constrained by AMD's current driver/firmware
  support, not by KubeVirt.** Verified directly against AMD's GIM/SR-IOV
  driver release notes (2026-07): officially supported hardware is
  exclusively MI-series Instinct datacenter accelerators plus one Radeon
  PRO workstation card — no APUs, no consumer/integrated GPUs, at all.
  `karnataka`'s AMD Strix Halo APU is a full-GPU-passthrough-to-one-VM
  device today, not a multi-tenant vGPU one. An AMD engineer has said
  client-GPU SR-IOV is "in the roadmap," no committed timeline — worth
  rechecking before committing to Phase 3, not assuming permanently
  impossible. Fine for "one nice accelerated desktop" today either way,
  not a GPU-accelerated multi-user pool on this hardware.
- **Overall scope**: this is realistically a personal/small-team VDI, not
  an oVirt/Kasm/Leostream competitor — the phased plan is sized for what
  one person plus Corral's existing components can actually ship, not for
  matching enterprise VDI feature parity.

## Open questions (for the grilling session)

1. **Claim primitive**: use a standard Kubernetes `Lease` per member, or a
   purpose-built Assignment CRD? A Lease gives atomic acquisition and expiry
   without introducing a new API, while a CRD gives clearer domain state and
   validation. A local registry or unguarded ConfigMap is ruled out because
   claims must be cluster-visible and concurrency-safe.
2. **CT-backed pool primitive**: KubeVirt gives us `VirtualMachinePool` for
   free; nothing upstream gives us a pooled-pod equivalent. Build our own
   (small — a label-based reconcile loop) or is this premature for CT pools
   specifically vs. starting VM-only?
3. **Reclaim defaults**: choose disconnect grace, maximum session duration,
   and whether an administrator may force-release a connected session. Input
   idle remains unavailable without guest/protocol cooperation.
4. **Licensing UX**: does Corral just document the Windows licensing
   constraint (current lean) or actively refuse to create pools above some
   size without an explicit `--i-have-licenses` flag?
5. ~~**Scope of "plugin"**~~ — **resolved for Phase 1**: single
   `corral-vdi` binary (`pool`/`assign`/`unassign`/`connect` subcommands),
   as leaned toward above. Revisit if/when Phase 2's broker becomes a
   genuinely separate long-running process (unlike Phase 1's one-shot CLI
   commands) — that's a real reason to split, not just default caution.
6. **Broker placement**: keep Phase 2's long-running broker inside
   `corral-vdi`, or expose VDI routes through a peer/gateway protocol? It must
   preserve the lean core and work with both Tailscale and ingress-agnostic
   deployments.

## Sources

Full research brief (KubeVirt VDI ecosystem, remote-display protocols,
session-brokering prior art, Windows-on-KubeVirt GPU/USB state, Linux
desktop-pooling patterns) — see the tracking issue for the complete,
sourced version. Key links: [FOSDEM 2026 VDI+KubeVirt talk](https://fosdem.org/2026/schedule/event/CFCCDQ-vdi-and-kubevirt/),
[KubeVirt VirtualMachinePool docs](https://kubevirt.io/user-guide/user_workloads/pool/),
[KubeVirt USB redirection](https://kubevirt.io/user-guide/compute/client_passthrough/),
[NVIDIA GPU Operator + KubeVirt](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-kubevirt.html),
[Selkies](https://github.com/selkies-project/selkies),
[Kasm VDI on Kubernetes](https://kasm.com/vdi-kubernetes),
[oVirt project update, Sept 2025](https://blogs.ovirt.org/2025/09/ovirt-project-update/).
