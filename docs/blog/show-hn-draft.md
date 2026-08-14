# Show HN draft

> **Title:** Show HN: Corral – Proxmox-style VM manager for KubeVirt and QEMU, one Go binary
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
- Tailscale: every VM can join your tailnet on first boot, so `corral ssh`
  works from your phone.

The fastest way to judge it (no cluster, no kubectl, nothing):

    brew install hanthor/tap/corral   # or the curl|sh in the README
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

Honest state of things: v0.1.x, five weeks old, one developer plus a lot of
Claude. The KubeVirt backend is the most exercised path; local-QEMU-in-the-web-UI
landed this week (lifecycle + info; console is a browser-based VNC view now, no
CLI hop needed). Windows VMs, GPU passthrough, scheduled snapshots/backups exist
as plugins of varying maturity. I'd love feedback on the architecture docs
(CONTEXT.md, docs/adr/) as much as the code.

Apache-2.0. Happy to answer anything.

---

## Submission notes (not part of the post)

- Submit morning US Eastern, Tue–Thu; have the `--demo` GIF at the top of the
  README before submitting (done).
- Expected pushback to be ready for: "why not just virt-manager/Cockpit?"
  (answer: cluster+local in one tool, tailnet-native, bootc), "web UI with no
  auth?" (answer: binds loopback by default, tailnet identity headers when
  served behind Tailscale, CORRAL_ADMINS gate), "KubeVirt is heavy" (answer:
  true — that's what the qemu backend is for).
