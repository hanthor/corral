# Corral Architecture

> Full project docs: [README.md](../README.md), [SPEC.md](../SPEC.md),
> [API reference](api.md), [testing plan](testing.md), [setup guide](kubevirt-proxmox-setup.md)

## Package map

```
cmd/              Cobra CLI (root, subcommands, TUI)
├── root.go       entrypoint, plugin dispatch, post-quit actions
├── commands.go   list, start, stop, ssh, viewer, logs, info, restart, pause, migrate, scale, adddisk, rmdisk, snapshot
├── create.go     corral create (all flags, both backends, CLI catalog/import/bootc)
├── clone.go      corral clone (kubevirt VM disk + config clone)
├── ct.go         corral ct (create/list/start/stop/delete/console — Containers, pkg/ct)
├── images.go     corral images (list catalog + imported datavolumes)
├── config.go     corral config (show, set auth key)
├── web.go        corral web (--addr, serve embedded SPA)
├── tui.go        Bubble Tea TUI (VMs + Containers side by side; VM and CT action menus, clone input state)
├── tui_views.go  TUI views at parity with the web UI (snapshots via pkg/snapshot, events, template mark, CT scaling)
├── plugin.go     corral plugin (search, install, list, remove)
├── doctor.go     corral doctor (cluster diagnostics)
├── helpers.go    shared CLI helpers (flags, namespace resolution)
├── corral-backup/  plugin: on-demand + scheduled (in-cluster CronJob) S3/R2 backups (built with -tags bootc-free, separate binary)
├── corral-windows/ plugin: UEFI/TPM/virtio-tuned Windows VMs
├── corral-vdi/     plugin: desktop pools (RFC-0001 phase 1) — pkg/vdi
└── corral-bootc/   plugin binary (built with -tags bootc, separate from core)

pkg/              Library code (importable)
├── catalog/      curated OS image catalog (containerdisks)
├── config/       ~/.config/tailvm/config.yaml reader (Tailscale auth key)
├── cronops/      shared CronJob/RBAC manifest builders for scheduled-op plugins (backup, snapsched, schedule)
├── backend/      the parity matrix + conformance tests; see docs/backend-parity.md
├── ct/           Containers (CT) — pet pods, not KubeVirt VMs; see docs/adr/0005
│   └── ct.go       Create/Start/Stop/Delete/List/Exists, distrobox-style persistent-rootfs bootstrap for privileged CTs
├── proxmoxbe/    Proxmox VE as a backend — HTTPS API + token; see docs/adr/0009
│                 (pkg/proxmox is the compat server — the other direction)
├── doctor/       cluster diagnostics + auto-fix (namespace, CDI, KubeVirt, GPU/PCI passthrough, StorageClass perf, etc.)
├── kubevirt/     KubeVirt backend (the heavy lifter)
│   ├── client.go          VM CRUD, SSH via virtctl, cloud-init, registry
│   ├── client_test.go     integration tests (need kubectl context)
│   ├── features.go        scale, volumes, snapshots, clone, export, guest-info, metrics, capabilities, templates
│   ├── options.go         boot options (run strategy, firmware, machine type, boot order) — read-modify-apply
│   ├── upload.go          ISO/template upload via virtctl image-upload
│   ├── bootc_core.go      bootc interface seam (always compiled — nil when tag absent)
│   └── bootc.go           bootc implementation (//go:build bootc — Job + kernel-boot VM)
├── plugin/        extension system (krew-style, marketplace fetch, install/remove)
├── qemu/          QEMU backend (local VMs via systemd user service)
├── registry/      ~/.local/share/tailvm/registry.json — VM name → backend/namespace/password
├── vdi/           desktop pools (RFC-0001 phase 1) — clone-based pool create, label-based assignment
├── types/         shared types (VM, CreateOpts, RegistryEntry)
└── web/           Proxmox-style web UI
    ├── server.go         HTTP server, mux, VM list/create/action/delete/info/export, nodes, tasks, WS bridges (VM VNC/serial + CT exec)
    ├── server_test.go
    ├── ct.go             CT list/create/start-stop/delete HTTP handlers
    ├── features.go       capabilities, scale, volumes, expand, snapshots, clone, guest-info, events, metrics, templates,
    │                     doctor, plugins, NADs, NICs, images, instancetypes, datavolumes, boot options, ISO upload
    └── static/
        ├── index.html    dark SPA shell, create dialog (6 source types), CT create dialog, build dialog
        ├── app.js        API client, tree (Server/Namespace/Pool views, VMs+CTs merged), VM/CT detail panels
        │                 (Summary/Hardware/Options/Snapshots/Events/Console/Terminal), create wizards,
        │                 image library + import, bootc build streaming, mobile drawer
        ├── icons.js      inline Heroicon SVGs
        └── style.css     dark theme, responsive, dialog/modals, cards, tables

marketplace/      Plugin registry (hosted at tuna-os/corral)
└── index.json    plugin entries (name, description, version, platform download URLs)

deploy/           On-cluster manifests
└── corral-web.yaml  Namespace, SA, ClusterRole, Deployment, Service, Tailscale Ingress

docs/             Documentation
├── api.md           REST API reference
├── architecture.md  This file
├── adr/             Architecture Decision Records (0001-0005)
└── kubevirt-proxmox-setup.md  From-scratch cluster setup guide

scripts/          CI and Docker helper scripts
Containerfile     Alpine + corral binary + kubectl + virtctl (for on-cluster deployment)
main.go           Entrypoint: cmd.Execute()
```

## Design decisions

### No client-go — shell-out to kubectl/virtctl

Corral never imports `client-go`. Every cluster interaction shells out to
`kubectl` or `virtctl`. This keeps the binary under ~12 MB, avoids a
~60 MB SDK dependency, and respects whatever kubeconfig the operator already
has. The trade-off: slower per-call latency (process spawn). For a
single-operator tool this is fine; for a multi-tenant API server it would be
unacceptable.

### Single binary, three UIs

`corral` is one Go binary that serves three interfaces:

1. **CLI** — Cobra subcommands (`corral create`, `corral ssh`, etc.)
2. **TUI** — Bubble Tea interactive terminal UI (bare `corral`)
3. **Web** — Embedded SPA served at `corral web --addr :8006`

All three share the same `pkg/kubevirt` backend and
`~/.local/share/tailvm/registry.json`. A VM created in the browser is
immediately `corral ssh`-able and vice versa.

### Embedded SPA — no JS build step

The web UI is vanilla JavaScript and CSS embedded via `//go:embed`. There is
no Node, no bundler, no build step. xterm.js and noVNC are loaded from CDN
at runtime (they're only needed on the console pages and are too large to
embed). Heroicons are inline SVGs.

### Backend transparency

After `corral create`, the registry stores `backend: "kubevirt"` or
`backend: "qemu"`. Every subsequent command (`start`, `ssh`, `viewer`, etc.)
reads the registry to determine the backend, falling back to live probing if
the registry entry is missing. The user never specifies `--kubevirt` again.

### Bootc as a plugin

Bootc (building a container image into a VM disk on-cluster) is an optional
plugin compiled behind a `//go:build bootc` tag. The core binary's
`bootc_core.go` defines the interface seam with nil implementations; the
tagged `bootc.go` replaces them via `init()`. This keeps the core binary
lean for users who only need containerdisks and ISOs, while the full
pipeline is available via `corral plugin install bootc` or by building with
`-tags bootc`.

The web UI checks `GET /api/capabilities` → `bootc: true/false` and
shows/hides the bootc source option accordingly.

### KubeVirt LiveUpdate strategy

Corral creates VMs with `cpu.maxSockets` + `memory.maxGuest` headroom and
masquerade networking so they *can* hotplug/migrate. It gates live operations
on real viability:

- Live migration / hotplug: needs `vmRolloutStrategy: LiveUpdate`, masquerade
  networking, migratable storage (RWX), **and a target node with the same CPU
  vendor** (you cannot live-migrate Intel→AMD).
- Disk hotplug: needs the `HotplugVolumes` feature gate.
- Snapshots: need a `VolumeSnapshotClass` (Longhorn CSI).
- Online expand: needs `allowVolumeExpansion: true`.

The UI queries `GET /api/capabilities` and greys out controls the cluster
can't support, rather than failing at submit time.

### Secrets stay local

- Cloud-init passwords are generated per-VM and stored in
  `~/.local/share/tailvm/registry.json` (mode 0600).
- The Tailscale auth key comes from `~/.config/tailvm/config.yaml` or the
  `TS_AUTHKEY` environment variable (seeded from Bitwarden by the dotfiles
  Ansible role).
- SSH public keys are read from `~/.ssh/id_ed25519.pub` (fallbacks:
  `id_rsa.pub`, `id_ecdsa.pub`).
- **No secrets in git, no secrets in cluster state.** The on-cluster
  deployment has no registry — it reads everything from cluster objects.

### Namespace conventions

- Default namespace: `tailvm` (overridable with `--namespace` or
  `-n`).
- VM names must match `[a-z0-9][a-z0-9-]*` (Kubernetes DNS label).
- On-disk state uses `tailvm` prefix (`~/.local/share/tailvm/`,
  `~/.config/tailvm/`) for backward compat with the legacy Python `tailvm`.

## Data flow

### VM create (containerdisk)

```
corral create web --kubevirt --image fedora
  │
  ├─ catalog.Find("fedora")              → containerDisk URI
  ├─ kubevirt.NewClient(ns).CreateVM(opts) → kubectl apply VM manifest
  │   ├─ cloud-init: SSH key + Tailscale auth key + password
  │   └─ containerDisk registry URL
  └─ registry.Set("web", {backend, namespace, password})
```

### VM create (bootc)

```
corral bootc create dev --image quay.io/centos-bootc/...
  │
  ├─ kubevirt.BootcBuildDisk()           → block PVC + short-lived builder VM
  │   ├─ builder VM: bootc install to-disk (composefs/ostree auto-detected)
  │   └─ Progress streams to stderr / task log
  ├─ kubevirt.GenerateBootcVM()          → VM manifest (UEFI-boots the disk)
  ├─ kubevirt.Apply(vm)                  → kubectl apply
  └─ registry.Set("dev", {backend, namespace})
```

Same flow for the web UI, but the build runs as a background task polled at
`GET /api/tasks/{id}` with a live log displayed in the browser.

### Web UI SSH

```
Browser: click SSH in VM detail panel
  │
  ├─ GET /api/vms/{ns}/{name}            → get VM info
  ├─ spawn: virtctl ssh {user}@{ns}/{name}
  │   └─ xterm.js + WebSocket terminal in browser
  └─ (or copy the virtctl ssh command to clipboard)
```

The web SSH delegates to the browser's ability to open terminal URLs — it
doesn't proxy SSH through the server.

### Web UI console

```
Browser: Console tab → VNC or Serial
  │
  ├─ VNC: WebSocket → /api/vnc/{ns}/{name}
  │   ├─ server: virtctl vnc --proxy-only → WS bridge
  │   └─ browser: noVNC canvas
  └─ Serial: WebSocket → /api/tty/{ns}/{name}
      ├─ server: virtctl console → WS bridge
      └─ browser: xterm.js terminal
```

## Build & CI

```
go build -o corral .          # core binary (no bootc)
go build -tags bootc -o corral .  # with bootc pipeline

# Container image (for on-cluster deployment)
docker build -t ghcr.io/tuna-os/corral .
# → Alpine + corral + kubectl + virtctl

# CI (.github/workflows/ci.yml)
# → go test ./..., go vet ./..., docker build, push to ghcr.io
```

## On-cluster deployment

The `deploy/corral-web.yaml` manifest:
1. Creates the `tailvm` namespace (privileged pod-security)
2. ServiceAccount + scoped ClusterRole (VM lifecycle, subresources,
   snapshots, CDI, PVCs, nodes, events, metrics, instancetypes, etc.)
3. Deployment (ghcr.io/tuna-os/corral:latest, port 8006)
4. ClusterIP Service (port 80 → 8006)
5. Tailscale Ingress (`ingressClassName: tailscale`) → TLS at
   `corral.<tailnet>.ts.net`

Tailnet membership *is* the authentication — never bind a public interface.
Authorization is an `adminGate` middleware: `CORRAL_ADMINS` (tailnet logins)
gates mutating requests, with everyone else read-only (see
[ADR-0003](adr/0003-identity-source.md)). Unset = single-user/open.

## Plugin system

Plugins are standalone executables named `corral-<name>`, discovered in
`~/.local/share/corral/plugins` (and `$PATH`). When you run `corral <name>`,
the root command checks if `<name>` is a known subcommand; if not, it
dispatches to the plugin. `CORRAL_PLUGIN=<name>` is exported so plugins know
how they were invoked.

The marketplace is the `marketplace/index.json` file in the repo, fetched
from GitHub raw at runtime. Each entry has a name, description, version, and
platform-specific download URLs. Installation downloads the binary, sets the
executable bit, and places it in the plugin dir.
