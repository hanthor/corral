# Corral — Domain Context

## Concepts

### VM

A virtual machine managed by Corral. It has a canonical identity composed of
peer, backend, context, namespace, and name. Human names need only be unique
inside their backend universe; bare-name commands are accepted only when the
aggregated fleet resolves them unambiguously.

### vmid

A Proxmox-style numeric VM identifier (range 100–999999999). Assigned to
KubeVirt VMs created through the Proxmox API compatibility layer.
Bidirectionally mapped to K8s VM names via the `corral.io/proxmox-vmid`
label. Pre-existing VMs without the label derive their vmid from a CRC32
hash of their name.

### Backend

Where a VM's compute resources live. Five backends:

- **qemu** — local `qemu-system-x86_64` process managed via systemd user
  units. Networking via user-mode with hostfwd. Access through the host's
  Tailscale IP.
- **kubevirt** — VM runs as a KubeVirt `VirtualMachine` resource on the
  Talos cluster. Managed via `kubectl`/`virtctl`. Access through
  `virtctl` tunnels or port-proxy Service on the tailnet.
- **incus** — an Incus container or VM on a named Incus remote.
- **libvirt** — a libvirt domain reached through a local or remote URI;
  `qemu+ssh://host/system` is the remote-QEMU transport.
- **proxmox** — a guest on a real Proxmox VE cluster, driven over PVE's HTTPS
  API with a revocable token (`pkg/proxmoxbe`, ADR-0009). The only backend
  that is an HTTP client rather than a command runner. PVE's two workload
  types map onto Corral's: a qemu guest is a VM, an lxc guest is a CT.
  Not to be confused with `pkg/proxmox`, which is the compat *server* —
  Corral answering PVE's API on its own fleet.

### Context

An independent backend universe: a kubeconfig context, Incus remote, or
libvirt URI. Corral stores its own default and never changes kubectl/Incus
global state. `--context` is a one-shot override.

`local` (qemu) is always present. Every other context is either configured
explicitly or discovered: the legacy `kubevirt` one appears only when this host
has a kubeconfig, Incus only with a remote set, libvirt only with a URI. A
context that is offered is a context Corral will list, doctor, and report
failures for — so a qemu-only or Incus-only host must not be handed a cluster
target it can never reach.

### Peer

Another `corral web` API aggregated into the local dashboard. Direct guest
connectivity is preferred; peer relay is the fallback. A peer keeps its
identity on every VM so duplicate names remain distinct.

### Console

Remote access to a VM's display: **VNC** (noVNC, port 5900) and **RDP**
(port 3389, guest-dependent — see ADR-0002). Bridged from the browser via a
websocket that proxies to `virtctl port-forward` / `virtctl vnc
--proxy-only` (`pkg/kubevirt.ConsoleDialer`). Exposed on the tailnet via
`ApplyProxy` at VM-creation time regardless of guest OS — exposure doesn't
imply the guest is listening; `GET /api/vms/{ns}/{name}/rdp` probes that
separately. Serial console access (`virtctl console`, xterm.js) is a related
but separate bridge, not yet folded into this concept.

### Container (CT)

A Proxmox-style **Container**, backed by a plain Kubernetes pod rather than
a KubeVirt VM — a "pet pod": a pod with a **PVC-backed persistent volume**,
an init process, and sshd, presented in the Proxmox CT shape. Not a
relabelled Deployment/cattle pod — it's meant to be long-lived and
console-able like a VM, just without a hypervisor underneath. Lives in
`pkg/ct` (a third backend alongside qemu/kubevirt, but never a KubeVirt
resource). Full design: `docs/adr/0005-containers-as-pet-pods.md`.

- **Privilege**: unprivileged by default, privileged opt-in — 1:1 with
  PVE's "Privileged" checkbox, but also the gate for the distrobox mode
  below (needs CAP_SYS_ADMIN + CAP_SYS_CHROOT, which unprivileged pods
  don't get).
- **Two storage modes**, chosen by Privileged:
  - **Unprivileged** (default): the PVC mounts at `/data` only; the rest of
    the filesystem is the image's own ephemeral layer. Package installs,
    anything outside `/data`, don't survive Stop (Kubernetes gives every
    pod restart a fresh filesystem from the image — there's no
    docker/podman-style "stopped container keeps its writable layer" here).
  - **Privileged — distrobox on k8s**: on first boot, the container seeds
    the PVC with a full copy of the image's own root filesystem (`cp -a
    --one-file-system /. $PVC`), then `chroot`s into it. The PVC *is* the
    rootfs from then on, so `apt`/`dnf`/`apk` installs, dotfiles, anything
    under `/` all survive Stop/Start — the actual distrobox experience
    (enter, install stuff, come back later, it's still there), not just a
    scratch `/data` dir. Needs a full OS image (debian/ubuntu/fedora — has
    `chroot` + coreutils' `cp -a`), not alpine/busybox.
- **Images**: any OCI image; no init process or sshd baked in by corral —
  the curated corral-owned `ct-*` catalog (with a `ct: true` capability
  flag riding the existing catalog/sources plumbing) is a follow-up content
  task, not part of the CT mechanism itself.
- **Console**: no framebuffer → exec/attach → xterm, reusing `/api/tty`
  (which now detects VM-vs-CT by name and dispatches to `virtctl console`
  or `kubectl exec` accordingly). For a privileged CT, the exec session
  re-`chroot`s into the PVC-backed rootfs on entry — a fresh `kubectl exec`
  joins the container's namespaces but starts from the un-chrooted image
  root (chroot only changes the calling process's own apparent root, not
  something a sibling exec session inherits), so landing inside the
  persistent rootfs takes a second chroot, not just plain `sh`.
- **Networking**: reached via a plain K8s Service selecting the CT pod
  directly (simpler than the VM port-proxy, which exists specifically to
  work around KubeVirt VMs not having a stable pod selector — a CT's own
  pod *is* the selector target).
- **Resources**: cores → pod CPU limit, memory → pod memory limit. PVE
  "swap" is dropped (no honest map).
- Snapshot (VolumeSnapshot of the PVC) and migrate (reschedule to a node
  that can mount the PVC) are later slices, not in the first CT
  implementation.

### Registry

The file `~/.local/share/tailvm/registry.json` (mode 0600). Maps canonical
instance references to backend, context, namespace, cloud-init password, and
other local metadata. Live backend inventory is authoritative for existence;
the registry retains credentials and creation metadata and reads legacy
name-keyed entries for migration compatibility.

### Snapshot

A point-in-time capture of an instance, taken through the backend's own
mechanism: KubeVirt `VirtualMachineSnapshot`, `virsh snapshot-*`, `incus
snapshot`, or a qcow2 internal snapshot via `qemu-img`. `pkg/snapshot` is the
adapter seam (`snapshot.For(ref)`); plugins consume it rather than reaching for
a backend directly.

Every snapshot reports its **consistency**, because the backends genuinely
differ and the difference decides whether a restore boots cleanly:

- **offline** — the instance was stopped. Nothing was in flight.
- **filesystem** — the guest filesystem was frozen for the capture. KubeVirt
  does this itself when the guest agent is connected.
- **crash** — captured live with nothing quiesced; restoring is equivalent to
  booting after a power cut.

Backends refuse rather than degrade silently: the local QEMU adapter will not
snapshot a running VM (qemu-img writing into a disk the guest has open corrupts
it) or a raw disk, and reports both as a typed `*snapshot.Unsupported` carrying
the remedy. A refusal is a 400 in the API — retrying cannot help — where a
backend that tried and failed is a 502.

**Retention** (`snapshot.Prune`) only ever deletes snapshots a schedule created
(the `auto-` prefix), oldest first, and is scoped by the instance reference —
so two contexts running a same-named instance prune independently.

### Bootc build and import

Building a bootable-container image into a disk, and putting that disk on a
backend, are separate steps (`pkg/bootc`): a `Builder` produces a disk, a
`Target` turns a disk into an instance. They pair freely — build on the cluster
and run there, or build locally with podman and run under local QEMU or
libvirt.

**The backend is a property of the image, not a choice.** Probing the image
filesystem with `podman cp` — never executing it, since Universal Blue images
ship Rust uutils that `podman --entrypoint` cannot dispatch:

- **bootupd present** → ostree backend, `xfs`, `--generic-image`
- **systemd-boot, no bootupd** → composefs backend, `btrfs`, `--composefs-backend`
- **neither** → ostree

`--generic-image` is load-bearing rather than cosmetic. bootupd installs the
bootloader to `EFI/<vendor>/` plus an efibootmgr NVRAM entry, and a fresh VM
has empty NVRAM — so without the removable `EFI/BOOT/BOOTX64.EFI` fallback the
disk builds perfectly and then boots to nothing.

The local builder runs `bootc install to-disk --via-loopback` in a privileged
podman container. The cluster builder cannot use loopback — pod security
contexts there lack loop module access, which is what `e141dbb` worked around —
but a privileged container on a real host has loop devices.

**composefs images are refused locally.** After `bootc install` they need the
kernel and initrd re-extracted over bootc's EROFS zero-filled ESP copies, and
the root key injected into `state/os/default/var/roothome/.ssh` because
`--root-ssh-authorized-keys` is a no-op on composefs. Only the cluster builder
implements that; half of it would produce a desktop image that builds and does
not boot. Incus is refused too — an Incus VM boots from Incus's own image store
and cannot adopt a foreign pre-partitioned disk.

### Windows guests

Windows needs three things no other guest does, and each is delivered
differently per backend (`pkg/windows`):

| | Firmware | Drivers | Answer file |
|---|---|---|---|
| kubevirt | cluster-provided UEFI + TPM | virtio-win containerdisk | ConfigMap presented as a CD-ROM |
| libvirt | `<os firmware='efi'>` + emulated TPM 2.0 (swtpm) | virtio-win ISO as a second CD-ROM | an ISO Corral builds on the host |
| incus | `security.secureboot` + Incus's own TPM | **slipstreamed** into the installer image | slipstreamed alongside |

Incus is the outlier: a VM gets one CD-ROM, so there is nowhere to attach a
driver disc during Setup. The upstream answer is `distrobuilder
repack-windows`, which Corral drives — refusing with the exact command when
distrobuilder is absent, rather than producing an installer that cannot see
its own disk.

Getting firmware wrong does not fail fast. Windows Setup runs for forty
minutes and then refuses at a compatibility check that says nothing about
libvirt, so `corral windows requirements` reports what a backend needs
*before* anything is created, and the adapters declare it as data rather than
burying it in conditionals.

`<os firmware='efi'>` is deliberate over a hardcoded OVMF path: the path
differs across distributions and a wrong one fails with a message about a
missing file rather than about firmware.

The virtio-win ISO comes from the Fedora virt group's stable channel, the same
artifact Red Hat ships with RHEL. Corral does not mirror it — pointing at
upstream means a user can verify what they are downloading.

### Device passthrough

Handing a host device — usually a GPU — to a guest, through `pkg/device`'s
adapter seam. Backends model it differently and the difference is structural:

| Backend | Discovery | Attachment |
|---|---|---|
| libvirt | `virsh nodedev-list --cap pci` + `nodedev-dumpxml` | `<hostdev managed='yes'>` by PCI address |
| incus | `GET /1.0/resources` | `incus config device add … gpu pci=…` (or `pci address=…`) |
| kubevirt | node allocatable extended resources | `spec.domain.devices.gpus` by **resource name**, not address |
| qemu | — | unsupported: Corral's local backend has no vfio wiring |

KubeVirt is the odd one out: devices are allowlisted cluster-wide in the
KubeVirt CR and advertised by a device plugin, so a VM attaches a *resource
name* and no PCI address exists to speak of. `corral gpu enable` edits that
allowlist and is KubeVirt-only by nature.

This is the most destructive thing Corral does, so mutation is deliberately
conservative. Every attachment surfaces its consequences **before** it happens
— restart required, live migration blocked, host loses the device, guest gains
DMA — and `corral gpu attach` asks for confirmation. Passing through the host's
**boot display** (`/sys/bus/pci/devices/<addr>/boot_vga`) is refused outright,
by a check in the shared package rather than per adapter: it would leave the
machine with no console, recoverable only by physical access.

`managed='yes'` on libvirt is deliberate — the device is unbound from the host
driver when the domain starts and rebound when it stops, rather than being
taken away from the host while the guest is off.

### Export

Copying an instance's disk out, through `pkg/export`'s adapter seam. Backends
produce genuinely different artifacts and the difference matters to whoever
restores from one, so the format is named rather than flattened:

| Backend | Formats (native first) | Route |
|---|---|---|
| kubevirt | `raw.gz`, `qcow2` | `virtctl vmexport` off the primary PVC |
| qemu | `qcow2`, `raw.gz` | `qemu-img convert` off the VM's disk |
| libvirt | `qcow2`, `raw.gz` | `virsh domblklist` → `qemu-img convert` |
| incus | `incus-tar` | `incus export` — an instance archive, **not** a disk image; restorable only with `incus import` |

Exports carry the same **consistency** vocabulary as snapshots. Local QEMU
refuses a running VM outright (copying a disk the guest is writing to produces
a torn image); libvirt cannot stop the domain on the caller's behalf, so it
exports and discloses `crash`. libvirt also refuses a remote hypervisor whose
disk files are not on this filesystem, rather than letting `qemu-img` fail with
a baffling "no such file".

Bulk data never relays through a peer when it doesn't have to: a peer export is
a 307 to the peer's own URL, so the client fetches it directly. A token-gated
peer keeps relaying, because a credential in a redirect the browser follows
would leak.

Credentials stay out of core. `pkg/export` writes a local file and knows
nothing about object storage; `corral-backup` owns the rclone config and the
permission declaration that goes with it.

### Schedule

An instance's autostart/shutdown window, stored against its canonical
reference — a bare name is meaningless when three contexts each have a `dev`.
`pkg/schedule` owns the record, the cron evaluation, and the tick;
`pkg/lifecycle` dispatches the resulting start/stop on any backend.

Two execution models, because the honest answer differs by backend:

- **In-cluster** (`--in-cluster`, KubeVirt only): a Kubernetes CronJob per
  boundary. The cluster fires it, so it works with the workstation closed.
- **Local** (everything else): `corral schedule tick`, driven by a systemd user
  timer, evaluates what is due and dispatches it. A laptop VM, an Incus remote,
  and a libvirt host have no CronJob controller to do it for them.

The portability limit is inherent: a locally scheduled boundary only fires
while that machine is awake and its timer is running. An instance that must
start at 09:00 regardless belongs on the cluster path.

Corral parses cron itself for the local path, accepting only the portable
subset (`*`, `n`, `a-b`, `*/n`, `a-b/n`, and comma lists). Extensions like
`@daily` or `L` are refused rather than half-supported — an expression that
means one thing to Kubernetes and another to the tick is worse than one that
won't be created. Day-of-month and day-of-week are OR'd when both are
restricted, matching cron(8).

A context that can't be reached is reported and retried next tick; it never
deletes the schedule, and it never stops the other contexts from running.

### Plugin

A krew-style extension binary (`corral-<name>`) installed via
`corral marketplace install <name>` from one or more marketplaces. Dispatched when
`corral <name>` is invoked and the subcommand isn't a built-in.

### Proxmox API compatibility layer

A package (`pkg/proxmox`) that translates the Proxmox VE REST API
(`/api2/json/...`) onto KubeVirt operations. Served as both:
- A standalone plugin binary (`corral-proxmox`, marketplace)
- Embedded in `corral web` at `/api2/json/...` (always available
  when the web server is running)

Enables Proxmox ecosystem tools (Terraform bpg/proxmox, Ansible,
proxmoxer) to manage KubeVirt VMs without modification.

### Node

A Kubernetes node in the Talos cluster. Exposed in the Proxmox API as a
Proxmox "node" with properties derived from `kubectl get node` output
(CPU capacity, memory, Ready condition).

### Storage class

A Kubernetes StorageClass. Mapped to Proxmox storage types:
- Longhorn (`driver.longhorn.io`) → `lvmthin`
- local-path (`rancher.io/local-path`) → `dir`

### Access control (Proxmox)

A read-only view of K8s RBAC translated into Proxmox shapes:
- **Users**: extracted from ClusterRoleBinding subjects (`root@pam` always
  present, ServiceAccounts mapped to `name@sa`)
- **Groups**: extracted from ClusterRoleBinding group subjects
- **Roles**: four fixed roles (Administrator, Operator, Viewer, NoAccess)
  mapped from the K8s cluster-admin/admin/view/default privilege levels

Auth enforcement is delegated to tailnet membership + K8s RBAC. Proxmox
privilege strings are presentation-only.

### Marketplace

A versioned JSON index with immutable HTTPS artifacts, SHA-256 digests,
optional Ed25519 signatures, permissions, compatibility ranges, publisher
provenance, and supported-backend declarations. Multiple sources are merged;
duplicate names require source-qualified selection.

## Glossary

| Term | Definition |
|---|---|
| vm | Virtual Machine |
| vmid | Proxmox numeric VM identifier |
| backend | qemu, kubevirt, incus, or libvirt |
| context | kubeconfig context, Incus remote, or libvirt URI |
| peer | remote Corral web API aggregated into this dashboard |
| ct | Container — a pet pod, not a VM |
| console | VNC/RDP bridge to a VM's display |
| registry | `~/.local/share/tailvm/registry.json` |
| plugin | krew-style corral-* binary |
| proxmox api | `/api2/json/` compatibility layer |
| node | K8s cluster node |
| storage class | K8s StorageClass |
| RBAC mapping | K8s ClusterRoles → Proxmox privileges |
