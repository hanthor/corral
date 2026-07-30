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

Where a VM's compute resources live. Four backends:

- **qemu** — local `qemu-system-x86_64` process managed via systemd user
  units. Networking via user-mode with hostfwd. Access through the host's
  Tailscale IP.
- **kubevirt** — VM runs as a KubeVirt `VirtualMachine` resource on the
  Talos cluster. Managed via `kubectl`/`virtctl`. Access through
  `virtctl` tunnels or port-proxy Service on the tailnet.
- **incus** — an Incus container or VM on a named Incus remote.
- **libvirt** — a libvirt domain reached through a local or remote URI;
  `qemu+ssh://host/system` is the remote-QEMU transport.

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
