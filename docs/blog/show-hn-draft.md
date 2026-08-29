# Show HN draft

> **Title:** Show HN: Corral – Proxmox-style VM manager for KubeVirt, QEMU, Incus and libvirt, one Go binary
>
> **URL:** https://github.com/tuna-os/corral

Post body (HN first comment, from the author):

---

Hi HN — I built Corral because I love how Proxmox *feels* but couldn't justify
running a whole second platform next to the Kubernetes cluster I already have.

Corral is a single static Go binary that gives you the Proxmox experience —
datacenter tree, create wizard, in-browser VNC/serial consoles, VMs and
containers side by side — on top of whatever you've got:

- A cluster: it drives KubeVirt through kubectl/virtctl. No operator, no
  agent, no CRDs of its own to install.
- Just a laptop: the same commands run VMs on local QEMU/KVM as systemd user
  services, and the same dashboard shows them under a "local" node.
- The boxes you didn't want to replace: an Incus server, a libvirt host over
  `qemu+ssh://`, a Proxmox VE cluster. Corral reuses the trust you already set
  up — an existing Incus remote, your OpenSSH agent/keys/config, a PVE API
  token — so there's no second password database to keep in sync, and it never
  mutates kubectl's or Incus's own global config to switch targets.
- Another Corral: `corral peer add homelab https://corral.example.ts.net`
  federates a second instance — the one running *inside* the cluster, say, when
  this laptop has no route to the Kubernetes API. Guest access is direct-first:
  if you can already reach the VM over Tailscale or an ingress, consoles and
  `corral ssh` go straight there instead of hairpinning through two Corral
  servers, and the Corral-to-Corral HTTP/WebSocket relay is the fallback for
  the networks where that's impossible.
- Tailscale: every VM can join your tailnet on first boot, so `corral ssh`
  works from your phone.

It's one fleet rather than five tabs: `corral list`, the TUI and the dashboard
show all of it at once. A context is a *destination* for unqualified commands,
not a mode switch — selecting one never makes the other machines disappear.

The fastest way to judge it (no cluster, no kubectl, nothing):

    brew install tuna-os/tap/corral-vm   # or the curl|sh in the README
    corral --demo                     # the TUI against a built-in fake cluster
    corral web --demo                 # the dashboard on 127.0.0.1:8006

`--demo` runs the real code paths against an in-memory fake cluster —
start/stop/create/delete all work, metrics move, and it's the same binary you'd
point at real infrastructure. I originally built it to develop the UI without
burning a cluster; it turned out to be the best demo tool I have. It's also
how the CI smoke tests drive the frontend.

The part I'm most excited about: bootc integration. Point Corral at a
*bootable container image* (ghcr.io/...) and it runs `bootc install to-disk`
in a builder VM on the cluster, then boots the result as a first-class VM.
Your OS is an OCI image in a registry; `corral bootc upgrade` rolls the VM to
the next build. Proxmox structurally can't do that.

The thing that fell out of running more than one backend at once: disk export
works on all of them now (qcow2, raw.gz, or an Incus tarball, and it refuses a
running guest where that would hand you a torn image), and `corral move <vm>
--to <backend>` composes that into a cross-backend move — preflight, export,
convert, ingest, verify. It is deliberately *cold* and says so everywhere: the
guest stops, and `migrate` still means the live within-one-backend kind. The
preflight refuses before anything is touched, and reports every reason at once
— firmware mismatch, disk bus and virtio drivers, free space, and the fact
that the guest comes up with a new MAC and almost certainly a new IP. The
source is left stopped, never deleted, unless you pass `--delete-source`. Incus
is a fine source and is refused as a destination, because importing a raw disk
into it would look like it worked and then behave unlike every other Incus
instance.

Honest state of things: v0.6.x, about eleven weeks old, one developer plus a
lot of Claude. KubeVirt is still the most exercised path, but local QEMU, Incus
and libvirt have caught up on the basics — inventory, create, lifecycle,
snapshots, export — and local QEMU in the web UI is finished: lifecycle, info,
and a real noVNC console in the browser, no CLI hop. What each backend still
can't do is written down per operation in docs/backend-parity.md, which is
generated from the same table the code reads and fails CI if the two drift, so
"supported" there means a test says so. Windows VMs, GPU passthrough, scheduled
snapshots/backups exist as plugins of varying maturity. I'd love feedback on
the architecture docs (CONTEXT.md, docs/adr/) as much as the code.

Apache-2.0. Happy to answer anything.

---

## Submission notes (not part of the post)

- Submit morning US Eastern, Tue–Thu; have the `--demo` GIF at the top of the
  README before submitting (done).
- Re-check the version and age line immediately before posting. It was
  accurate at v0.6.0 (tagged 2026-08-06) against a repo created 2026-06-10.
- Expected pushback to be ready for: "why not just virt-manager/Cockpit?"
  (answer: cluster, laptop, Incus, libvirt and PVE aggregated as one fleet,
  tailnet-native, bootc), "web UI with no auth?" (answer: binds loopback by
  default, tailnet identity headers when served behind Tailscale,
  CORRAL_ADMINS gate), "KubeVirt is heavy" (answer: true — that's what the
  qemu backend is for), "the non-KubeVirt backends must be stubs" (answer:
  point at docs/backend-parity.md, which is generated from the code and names
  every remaining gap rather than hiding it).
