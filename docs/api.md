# Corral API Reference

> Full project docs: [README.md](../README.md), [SPEC.md](../SPEC.md),
> [architecture](architecture.md), [setup guide](kubevirt-proxmox-setup.md)

The Corral web UI serves a REST API at port 8006 (`corral web`), plus
WebSocket bridges for VNC and serial consoles. All responses are JSON unless
noted otherwise.

The same API is available from the on-cluster deployment at
`https://corral.<tailnet>.ts.net`.

## Errors

Every error response has shape `{"error": "<message>"}` with a 4xx or 5xx
status code. `5xx` means the cluster couldn't be reached (kubectl failed);
`4xx` means invalid input.

---

## VM lifecycle

### `GET /api/vms`

List all VMs across all namespaces. Merges live VMI data (IP, node) for
running VMs.

**Response** (array):

```json
[
  {
    "name": "web",
    "namespace": "tailvm",
    "status": "Running",
    "ready": true,
    "running": true,
    "cpu": 2,
    "mem": "4Gi",
    "disk": "20Gi",
    "ip": "10.42.0.15",
    "node": "bihar"
  }
]
```

### `POST /api/vms`

Create a VM.

**Request** fields:

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | `[a-z0-9][a-z0-9-]*` |
| `namespace` | string | no | defaults to `tailvm` |
| `cpu` | int | no | default 2 |
| `mem` | string | no | e.g. `4G`, default `4G` |
| `disk` | string | no | e.g. `20G`, default `20G` |
| `containerDisk` | string | source | OCI containerdisk URI |
| `image` | string | source | catalog entry name (see `/api/images`) |
| `import` | string | source | qcow2/raw URL (CDI import) |
| `iso` | string | source | ISO URL (CDI import) |
| `bootc` | string | source | bootc image URI (needs bootc plugin) |
| `pvc` | string | source | existing PVC name |
| `sshKey` | string | bootc only | SSH public key baked into the disk |
| `node` | string | no | schedule on a specific node |
| `cloudInit` | string | no | cloud-init user-data YAML |
| `instancetype` | string | no | cluster instancetype name |
| `preference` | string | no | cluster preference name |

Exactly one *source* field must be set.

**Response** (201 Created):

```json
{"name": "web", "namespace": "tailvm"}
```

**Bootc response** (202 Accepted, build is async):

```json
{"task": "bootc-web-1712345678"}
```

The task ID can be polled at `GET /api/tasks/{id}`.

### `GET /api/vms/{ns}/{name}`

Get raw VM manifest JSON (the kubevirt VirtualMachine object).

### `POST /api/vms/{ns}/{name}/{action}`

Execute an action on a VM. Valid actions:

| Action | Meaning |
|---|---|
| `start` | Power on |
| `stop` | Graceful shutdown |
| `restart` | Restart |
| `pause` | Freeze (kubevirt only) |
| `unpause` | Resume (kubevirt only) |
| `migrate` | Live-migrate (kubevirt only) — prefer the dedicated endpoint below |

**Response**: `{"status": "ok"}`

### `POST /api/vms/{ns}/{name}/migrate`

Live-migrate as a tracked background task with progress in the activity panel.
The trigger runs synchronously (so "not migratable / cross-vendor" errors come
back immediately); the migration is then watched to completion.

**Body**: `{"targetNode": "bihar"}` (optional — empty lets the scheduler choose,
from among same-CPU-vendor nodes).

**Response**: `202 {"task": "migrate-…"}` — poll `GET /api/tasks/{id}`.

### `POST /api/vms/{ns}/{name}/tags`

Add or remove a tag, persisted as a `corral.dev/tag.<name>` VM label (so tags
survive round-trips and are `kubectl get vm -l`-selectable). Tags are surfaced
on every VM in `GET /api/vms`.

**Body**: `{"tag": "prod", "on": true}`

**Response**: `{"tag": "prod", "on": true}`

### `DELETE /api/vms/{ns}/{name}`

Delete a VM, its PVCs, DataVolumes, hotplug disks, snapshots, proxy
resources, and registry entry.

**Response**: `{"status": "deleted"}`

---

## Containers (CT)

Proxmox-style pet pods — plain Kubernetes pods, not KubeVirt VMs. See
`docs/adr/0005-containers-as-pet-pods.md` for the design.

### `GET /api/cts`

List all Containers across every namespace.

**Response**: `200`
```json
[{"name": "devbox", "namespace": "corral-vms", "node": "karnataka",
  "phase": "Running", "ready": true, "image": "debian:bookworm",
  "cpu": 2, "mem": "2Gi", "privileged": true}]
```

`phase` is the pod phase, or `"Stopped"` when no pod exists (the CT's
durable identity is its data PVC, which survives Stop). `node` is empty
when stopped/unscheduled.

### `POST /api/cts`

Create a Container.

**Body**:
```json
{"name": "devbox", "namespace": "corral-vms", "image": "docker.io/library/debian:bookworm",
 "cpu": 2, "mem": "2Gi", "disk": "10Gi", "storageClass": "", "privileged": true}
```

`privileged: true` seeds a persistent, mutable root filesystem onto the
data volume and `chroot`s into it (distrobox-style — package installs and
dotfiles survive Stop/Start). `false` (default) mounts the volume at
`/data` only.

**Response**: `201 {"name": "devbox", "namespace": "corral-vms"}`

### `POST /api/cts/{ns}/{name}/{action}`

`action` is `start` or `stop`. Stop deletes the pod but keeps the data
volume; Start recreates the pod from the spec recorded on the volume's
annotation.

**Response**: `{"status": "ok"}`

### `DELETE /api/cts/{ns}/{name}`

Delete a Container: its pod, Service (if a tailnet proxy was applied), and
data volume.

**Response**: `{"status": "deleted"}`

---

## Hardware operations

### `POST /api/vms/{ns}/{name}/scale`

Change CPU count and/or memory. Body: `{"cpu": 4, "mem": "8G"}`. Live-hotplugs
when the VM is live-migratable; otherwise does a stop→patch→start.

**Response**: `{"status": "ok"}`

### `POST /api/vms/{ns}/{name}/volumes`

Hotplug a new disk. Body: `{"size": "10Gi"}`.

**Response**: `{"pvc": "web-disk-2"}`

### `DELETE /api/vms/{ns}/{name}/volumes/{vol}`

Detach a hotplugged disk. `{vol}` is the PVC name.

**Response**: `{"status": "removed"}`

### `POST /api/vms/{ns}/{name}/expand`

Expand an existing PVC. Body: `{"pvc": "web-disk-1", "size": "40Gi"}`. Needs a
StorageClass with `allowVolumeExpansion: true`.

**Response**: `{"status": "ok"}`

### `POST /api/vms/{ns}/{name}/nics`

Add a secondary NIC. Body: `{"nad": "lan", "iface": "eth1"}`. Needs Multus + a
NetworkAttachmentDefinition in the VM's namespace.

**Response**: `{"status": "ok"}`

---

## Snapshots

### `GET /api/vms/{ns}/{name}/snapshots`

List snapshots for a VM. Needs the `Snapshot` feature gate.

**Response**: array of `{name, phase, creationTime}`

### `POST /api/vms/{ns}/{name}/snapshots`

Create a snapshot. Body: `{"name": "before-upgrade"}` (optional; auto-named).

**Response**: `{"name": "before-upgrade"}`

### `POST /api/vms/{ns}/{name}/snapshots/{snap}/restore`

Restore a VM to a snapshot. The VM is shut down during restore.

**Response**: `{"status": "restoring"}`

### `DELETE /api/vms/{ns}/{name}/snapshots/{snap}`

Delete a snapshot.

**Response**: `{"status": "deleted"}`

---

## Clone & template

### `POST /api/vms/{ns}/{name}/clone`

Clone a VM (definition and disks). The VM must be stopped. Body:
`{"target": "web-clone"}`. Needs the `Snapshot` feature gate + a
`VolumeSnapshotClass`.

**Response**: `{"target": "web-clone"}`

### `POST /api/vms/{ns}/{name}/template`

Mark or unmark a VM as a template. Body: `{"on": true}`.

**Response**: `{"isTemplate": true}`

---

## Image library & catalog

### `GET /api/images`

The built-in OS image catalog — curated, ready-to-boot containerdisks.

**Response** (array):

```json
[
  {
    "name": "fedora",
    "description": "Fedora 42 cloud",
    "containerDisk": "quay.io/containerdisks/fedora:42",
    "defaultUser": "fedora"
  }
]
```

User-defined custom sources are appended, each flagged `"custom": true`.

### `GET /api/sources`

The user-defined custom sources only (for management UI). Same entry shape as
`/api/images`, all `"custom": true`. Persisted in the `corral-sources`
ConfigMap so they survive web-pod restarts.

### `POST /api/sources`

Add or replace a custom source (idempotent by name).

```json
{"name": "my-image", "kind": "containerDisk", "uri": "ghcr.io/me/img:tag", "description": "optional"}
```

`kind` is one of `containerDisk` (boots directly), `url` (qcow2/raw, CDI
import), or `iso` (installer ISO). **Response**: `{"status": "ok", "name": "my-image"}`.

### `DELETE /api/sources/{name}`

Remove a custom source. **Response**: `{"status": "removed"}`.

### `GET /api/datavolumes`

List imported images (CDI DataVolumes). These are ISO/qcow2/raw images
imported from URLs.

**Response**: array of `{name, namespace, size, phase, progress, source}`

### `POST /api/datavolumes`

Start a CDI import. Body:

```json
{"name": "jammy", "namespace": "tailvm", "url": "https://.../jammy.qcow2", "size": "10Gi"}
```

**Response**: `{"name": "jammy"}`

### `DELETE /api/datavolumes/{ns}/{name}`

Delete an imported image (DataVolume).

**Response**: `{"status": "deleted"}`

---

## Bootc builds

### `GET /api/tasks/{id}`

Poll a bootc build task. Returns live build log.

**Response**:

```json
{"status": "running", "log": "Pulling image...\nInstalling...", "error": ""}
```

Statuses: `"running"` → `"done"` (success) or `"error"` (failure).

---

## Guest info

### `GET /api/vms/{ns}/{name}/guestinfo`

Guest-agent info (OS, hostname, users, filesystems). Returns `503` if the
agent isn't reachable.

### `GET /api/vms/{ns}/{name}/events`

Recent K8s events for the VM (Proxmox-style task log).

### `GET /api/vms/{ns}/{name}/metrics`

Live CPU/memory/disk metrics. Returns null values if the metrics-server isn't
available.

### `GET /api/vms/{ns}/{name}/metrics/history`

Retained CPU samples for the Summary-panel sparkline. The server samples every
running VM every ~15s into a bounded in-memory ring buffer (~1h). Returns
`[{"t": <epoch-ms>, "cpu": <millicores>}, …]` — an empty array when
metrics-server is absent.

---

## Infrastructure

### `GET /api/whoami`

The caller's tailnet identity and privilege, for the UI to show the logged-in
user and switch to read-only for non-admins. Identity comes from the Tailscale
ingress headers; admin is governed by `CORRAL_ADMINS` (see
[ADR-0003](adr/0003-identity-source.md)).

**Response**: `{"login": "alice@github", "name": "Alice", "admin": true, "enforced": false}`

Mutating requests (non-GET) are rejected with `403` for non-admins when an
allowlist is configured.

### `GET /api/nodes`

List cluster nodes with readiness, roles, kubelet version, and architecture.

**Response** (array):

```json
[{"name": "bihar", "ready": true, "roles": "control-plane,master", "kubelet": "v1.36.1", "arch": "amd64"}]
```

### `GET /api/capabilities`

Cluster capability flags — the UI gates controls on these.

**Response**:

```json
{
  "storageClass": "longhorn",
  "canExpand": true,
  "canSnapshot": true,
  "bootc": false
}
```

| Field | Meaning |
|---|---|
| `storageClass` | Default StorageClass for new VM disks |
| `canExpand` | Whether `allowVolumeExpansion: true` exists |
| `canSnapshot` | Whether a VolumeSnapshotClass is available |
| `bootc` | Whether the bootc plugin is compiled in |

### `GET /api/instancetypes`

Cluster instancetypes and preferences for the create wizard.

**Response**:

```json
{"instancetypes": ["u1.medium", "u1.large"], "preferences": ["fedora", "ubuntu"]}
```

### `GET /api/nads`

Multus NetworkAttachmentDefinitions available for secondary NICs.

### `GET /api/doctor`

Run all Corral cluster diagnostics. Returns a list of checks (name, status,
message).

### `POST /api/doctor/fix`

Run the fixable diagnostics (e.g. create missing namespaces).

**Response**: `{"fixed": ["created namespace tailvm"]}`

---

## Extensions (plugins)

### `GET /api/plugins`

Marketplace entries merged with locally-installed state.

**Response** (array):

```json
[{"name": "bootc", "description": "...", "version": "0.1.0", "installed": false, "inStore": true}]
```

### `POST /api/plugins/{name}/install`

Install a plugin from the marketplace.

**Response**: `{"installed": "bootc"}`

### `DELETE /api/plugins/{name}`

Remove an installed plugin.

**Response**: `{"status": "removed"}`

---

## Console & export (WebSocket)

### `GET /api/vnc/{ns}/{name}` (WebSocket)

Upgrade to WebSocket → bridges `virtctl vnc --proxy-only`. Use with noVNC in
the browser.

### `GET /api/tty/{ns}/{name}` (WebSocket)

Upgrade to WebSocket → bridges `virtctl console`. Use with xterm.js in the
browser.

### `GET /api/vms/{ns}/{name}/export`

Download a VM disk backup. The VM must be stopped (RWO disk is busy while
running). Triggers `virtctl vmexport` and streams the result.

| Query | Format | Content-Type |
|---|---|---|
| *(default)* | gzipped raw (`name.img.gz`) | `application/gzip` |
| `?format=qcow2` | compressed qcow2 (`name.qcow2`) | `application/octet-stream` |

qcow2 needs `qemu-img` on the server (it converts the raw export); if absent the
request returns `501` and the default raw.gz still works.

---

## Pools and cross-backend move

Pools are the user-defined folders of ADR-0008; paths travel in the body or the
query string, never in the route, because a nested path contains slashes.

| Method | Route | Notes |
|---|---|---|
| `GET` | `/api/folders` | Tree with members resolved against the live fleet, plus `unfoldered` |
| `POST` | `/api/folders` | `{path}` — nesting with `/`, max depth 8 |
| `DELETE` | `/api/folders?path=…` | Members are unfoldered, never deleted |
| `POST` | `/api/folders/move` | `{from, to}` — re-parent a pool |
| `POST` | `/api/folders/members` | `{path, ref}` — what a drag-and-drop lands on |
| `DELETE` | `/api/folders/members?ref=…` | |
| `POST` | `/api/folders/action` | `{path, action}` — fans out with per-member results |

Moving an instance between backends (ADR-0010) is two endpoints, because the
decision and the doing are separate. **A move stops the guest** — it is not a
live migration, and `corral migrate` remains the live, same-backend operation.

| Method | Route | Notes |
|---|---|---|
| `GET` | `/api/move/destinations` | Which backends can receive, and why the others cannot |
| `POST` | `/api/move/preflight` | Returns the plan. Touches nothing — safe to call on every drag |
| `POST` | `/api/move` | Commits, after re-running the preflight server-side |

Body for both: `{ref, toBackend, toContext?, toNamespace?, name?, scratch?, deleteSource?}`.

A refused preflight is a **200** — the refusals are the answer, and an error
status would make a UI show "request failed" where it should show three reasons.
A refused commit is a **409** carrying the same list. The source is stopped and
kept unless `deleteSource` is set.

---

## Metrics

| Method | Route | Notes |
|---|---|---|
| `GET` | `/metrics` | Prometheus text exposition. Requires `corral web --metrics` |

Served from a cached snapshot refreshed on a background timer, so a scrape never
fans out to the backends. `corral_collection_age_seconds` is published for that
reason: without it, a frozen collector is indistinguishable from a stable fleet.
The `pool` label is the one that spans backends —
`sum by (pool) (corral_instance_running)` answers "is my application stack up"
across a KubeVirt cluster and a Proxmox host at once. See ADR-0011 for the full
series list and label rationale.

Without `--metrics` the endpoint still answers 200 with
`corral_collection_success 0`: a scraper that receives a 503 records nothing,
and "up but not collecting" is the state worth alerting on.

---

## Static assets

The embedded SPA is served at the root:

| Route | File |
|---|---|
| `/` | `index.html` |
| `/style.css` | `style.css` |
| `/app.js` | `app.js` |
| `/icons.js` | `icons.js` |

All other paths fall through to the Go 1.22 `http.ServeMux` 404.
