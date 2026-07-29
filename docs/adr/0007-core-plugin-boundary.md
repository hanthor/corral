# ADR-0007: Core owns fleet primitives; plugins own optional workflows

Status: accepted

## Decision

The main binary owns the stable platform seam: contexts and defaults,
canonical instance identity, inventory, lifecycle dispatch, capability
reporting, federation transports, doctor probes, and CLI/TUI/web presentation.
These must work before an extension is installed because every workflow needs
the same unambiguous target selection and safety rules.

Standalone executables own optional or high-dependency workflows: auth, disk
backup, bootc builds, GPU policy, Proxmox compatibility, schedules, snapshot
retention, and Windows installation. They use `corral.plugin/v1`, declare
permissions and supported backends, and may depend on core `pkg/` adapters.
External plugins use the same contract as first-party plugins.

Incus is adopted into core because it is a compute backend, not a workflow.
`corral-incus` remains only as a compatibility executable and is excluded from
the marketplace. OIDC, Basic Auth, and passkeys remain in `corral-auth`; their
identity and cryptography dependencies do not belong in the main CLI binary.
The VDI experiment remains outside this decision and is not implemented by the
platform-completion work.

Some first-party plugin functionality is currently also reachable from the web
binary through shared packages. That is a compatibility surface, not permission
to make a marketplace plugin silently universal. UI controls must follow
instance capabilities and plugin `supportedBackends`. Issues #129–#134 track
the missing adapters.

## Marketplace and contribution model

Marketplaces are signed/checksummed indexes of standalone binaries, not Go
modules loaded into Corral's address space. Multiple sources, provenance,
version pinning, compatibility ranges, immutable artifacts, explicit permission
consent, and atomic rollback make external contribution possible without
granting third-party code the main process's trust. Publication does not imply
runtime sandboxing; operating-system and cluster permissions remain the actual
security boundary.

## Consequences

- A new compute backend requires a core adapter and doctor probe.
- A new optional workflow should begin as a plugin.
- Reusable backend operations belong under `pkg/`, never under `cmd/`.
- Plugins must fail clearly on unsupported selected contexts.
- Core binary growth is evaluated from stripped artifacts; optional auth and
  workflow dependencies remain isolated in their plugin binaries.
