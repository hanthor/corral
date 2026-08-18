# First-party Corral plugins

These executables live under `cmd/corral-*`, implement the
`corral.plugin/v1` metadata handshake, and are published by tuna-os through the
curated marketplace.

| Plugin | Purpose | Supported backends | Main capabilities |
|---|---|---|---|
| `auth` | Optional reverse-proxy authentication | all (web transport) | OIDC SSO, htpasswd Basic Auth, passkeys, peer service tokens |
| `backup` | VM disk backup and restore | KubeVirt | KubeVirt export, rclone/S3/R2, schedules |
| `bootc` | Bootable-container VM workflow | KubeVirt | On-cluster disk builds and rebuilds |
| `gpu` | GPU/PCI passthrough | KubeVirt | Device discovery, permitted devices and attachment |
| `proxmox` | Proxmox VE compatibility service | KubeVirt | `/api2/json` HTTP API backed by KubeVirt |
| `schedule` | Lifecycle windows | KubeVirt | Kubernetes CronJobs for VM start/stop |
| `snapsched` | Snapshot retention schedules | KubeVirt | KubeVirt snapshots and pruning CronJobs |
| `windows` | Windows VM creation | KubeVirt | UEFI, TPM, Hyper-V, installer and virtio media |
| `vdi` | Desktop pools (Phase 1) | KubeVirt | Static desktop pools, golden VM cloning, manual assignment |

`corral-vdi` provides Phase-1 static desktop pools governed by RFC-0001 and
documented in [docs/vdi.md](vdi.md) (with epic dependency gating tracked in
[docs/vdi-epic-status.md](vdi-epic-status.md)). `corral-incus` is a compatibility
binary for older installations; Incus is now a built-in backend and is not
published as a marketplace plugin.

First-party source is not automatically trusted at runtime. Marketplace v2
still requires immutable URLs and SHA-256 checksums, validates optional
Ed25519 signatures, displays permissions, and records installed provenance.

The matrix is deliberately honest: core inventory and lifecycle support for a
backend does not imply that every workflow plugin supports it. The extension
issues [#129](https://github.com/tuna-os/corral/issues/129) through
[#134](https://github.com/tuna-os/corral/issues/134) track adapter work. Until
those land, the marketplace and web UI expose the limitation before install.
