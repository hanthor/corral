# VDI Epic Status & Dependency Chain (#69)

This document records the dependency chain, current implementation state, and hardware/media gating status for the VDI epic ([issue #69](https://github.com/tuna-os/corral/issues/69)).

## Status Summary

The VDI epic is gated on hardware and media prerequisites (`#129` and `#132`). The core Phase 1 desktop pool primitives (`pkg/vdi` and `cmd/corral-vdi`) and supporting platform adapters (`pkg/snapshot`, `pkg/export`, `pkg/lifecycle`, `pkg/schedule`, `pkg/doctor`) have landed and are fully covered by unit tests in CI.

## Hardware & Media Blockers

1. **GPU / Device Passthrough (`#129`)**:
   - Software-rendered desktops are a demonstration, not high-performance VDI.
   - Blocked on hosts with spare passthrough-capable devices; binding a GPU to `vfio-pci` on a workstation takes its physical display.
   - CI environment lacks dedicated GPU passthrough devices.

2. **Windows Guests (`#132`)**:
   - Blocked on non-redistributable Windows ISOs and `virtio-win` drivers (cannot be hosted in public CI repositories).
   - Long unattended installation cycles (30–60 minutes) per verification pass.

3. **Lifecycle at Pool Scale (`#133`)**:
   - Partially unblocked. `pkg/lifecycle` handles canonical `InstanceRef` power operations, and `pkg/schedule` manages windowed start/stop execution across contexts.

## Landed Primitives & Adapters

- **Phase 1 VDI Plugin (`pkg/vdi`, `cmd/corral-vdi`)**: Static pools, golden VM cloning via `kubevirt.Client.Clone`, label-based assignment (`corral.dev/vdi-pool`, `corral.dev/vdi-assigned-to`), and CLI lifecycle (`pool create/list/delete`, `assign`, `unassign`, `connect`).
- **In-Browser RDP Proxy**: RDCleanPath proxy supporting IronRDP (ADR-0002 Phase 2).
- **Disk Snapshot Adapters (`#134`)**: Multi-backend snapshot/restore/retention in `pkg/snapshot`.
- **Export Adapters (`#131`)**: Per-backend disk image export in `pkg/export`.
- **Peer Diagnostics (`#135`)**: Multi-site reachability and console routing diagnostics in `pkg/doctor`.

## Roadmap Recommendation

Keep issue `#69` tracked as an epic pending hardware/media resolution for `#129` and `#132`. `pkg/vdi` remains active in the codebase for static pool creation and manual assignment.
