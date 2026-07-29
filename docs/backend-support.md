# Backend support matrix

Corral can show several contexts at once. A context is a named connection to
one backend; changing the default context changes only where an unqualified
command goes and never hides the rest of the fleet.

| Capability | local QEMU | KubeVirt | Incus | libvirt | Corral peer |
|---|---:|---:|---:|---:|---:|
| Aggregate inventory | yes | yes | yes | yes | yes |
| Start, stop, delete | yes | yes | yes | yes | relayed |
| Create | yes | yes | yes | yes | remote API |
| SSH | direct when address is known | direct, then console transport | `incus exec`/network | direct when address is known | direct first, relay fallback |
| Browser console | VNC | virtctl VNC | terminal | VNC/virt-viewer | direct first, relay fallback |
| Snapshots, migration, hotplug | backend-limited | yes | not yet unified | not yet unified | advertised by remote |
| Backend doctor | yes | yes | yes | yes | tracked in #135 |

Use `corral context get`, `corral context use NAME`, and the persistent
`--context NAME` / `--backend NAME` flags. Bare VM names work only when unique;
scripts should use the canonical ID printed by `corral list`.

First-party workflow plugins have a separate support matrix in
[first-party-plugins.md](first-party-plugins.md). Plugins must declare their
supported backends; “installed” is not evidence that a workflow works on every
inventory target.

Tailscale is a first-class endpoint discovery/exposure option, not a required
network. KubeVirt ingress remains implementation-agnostic. Federation attempts
advertised/direct guest endpoints first and uses Corral-to-Corral HTTP and
WebSocket relay only when the network cannot reach the guest directly.
