# Corral Roadmap

**Last updated**: 2026-08-23 | **Maintainer**: tuna-os (hanthor) / architect agent

---

## Mission

Corral herds VMs — and containers — into your tailnet. A single Go binary with
a CLI, TUI, and web dashboard, it manages VMs across five backends (qemu,
kubevirt, incus, libvirt, proxmox) and exposes every guest over the Tailscale
network your devices already share. It is also the org's emerging CI boot-gate
tool: [tunaos#1273](https://github.com/tuna-os/tunaos/pull/1273) adopts Corral
to gate ISO builds on boot evidence.

---

## Current Status (2026-08-23)

- Go rewrite of the legacy Python `tailvm`; on-disk/`tailvm-` prefixes retained
  during the transition ([SPEC.md](SPEC.md)).
- Five backends supported; `bootc` creation mode builds a bootable-container
  disk on-cluster and runs it as a KubeVirt VM.
- ✅ **Backend move parity closed 2026-08-11**: the Proxmox export adapter
  (#163) and Incus image publishing (#164) both landed, and the Incus backend
  gained end-to-end coverage (#123). `move` is no longer the weakest surface —
  see [docs/backend-parity.md](docs/backend-parity.md).
- Plugin marketplace live (`marketplace/index.json`); first-party plugins
  documented in [docs/first-party-plugins.md](docs/first-party-plugins.md).
  Plugin artifacts are published roughly daily (latest `plugins-dbb31c0e`,
  08-21).
- ⚠️ **Distribution is the weakest surface now** (#210): the README's `curl |
  sh` path resolves to a `corral-darwin-*` asset that does not exist in any of
  this repo's 70 releases, so macOS fails outright; the installer does not
  verify the `SHA256SUMS` it publishes alongside the binary; **v0.6.0 was
  tagged 08-06 with no GitHub Release**, so the Releases page still shows
  v0.5.0 as the newest product release, under 62 `plugins-<sha>` artifact
  releases.
- 🟡 **Org CI adoption is blocked, not drafting**: tunaos#1273 has been open
  since 08-10 and is currently `CONFLICTING`. It needs a rebase, not more
  design.
- **No milestones on the repo**; work is tracked through issues and labels
  (`needs-triage`, `ready-for-agent`, …) plus this file.

### Priorities

| Priority | Item | Tracking | Status |
|----------|------|----------|--------|
| P0 | Fix the advertised install path — macOS 404, unverified download, product releases invisible under plugin artifacts | #210 | 🔴 Open |
| P1 | Unblock the tunaOS boot-gate adoption — rebase the conflicting PR | tunaos#1273 | 🔴 Blocked on conflicts since 08-10 |
| P1 | VDI plugin epic — Windows/Linux desktop pools | #69, [docs/vdi-epic-status.md](docs/vdi-epic-status.md) | 🟡 In progress |
| P2 | Programmatic output — `--json` across commands | #205 | 🔴 Open |
| P2 | Stable plugin API contract + marketplace schema v2 | [docs/plugin-marketplace.md](docs/plugin-marketplace.md) | 🟡 In progress |
| P3 | Dependency dashboard / renovate hygiene | #97 | 🔴 Open |

---

## Quarterly Goals

### Current Quarter (2026 Q3 — July–September)

**Theme**: Backend parity, then a front door worth the parity.

| Goal | Owner | Tracking | Status |
|------|-------|----------|--------|
| Proxmox export adapter (move source parity) | architect | #163 | ✅ Done — closed 08-11 |
| Incus image publishing (move destination parity) | architect | #164 | ✅ Done — closed 08-11 |
| Incus E2E suite | quality | #123 | ✅ Done — closed 08-11 |
| Install path works on every OS the installer claims to support | — | #210 | 🔴 Open |
| One visible product release channel, separate from plugin artifacts | — | #210 | 🔴 Open — v0.6.0 tagged, unreleased |
| Merge tunaOS boot-gate adoption | ci-maintainer | tunaos#1273 | 🔴 Conflicting — rebase needed |
| Document stable plugin API + marketplace schema v2 | guide | docs/plugin-marketplace.md | 🟡 In progress |

### Next Quarter (2026 Q4 — October–December)

**Theme**: Enterprise readiness

- VDI plugin GA (desktop pools for Windows/Linux) — #69
- Plugin marketplace growth: schema v2, signed releases, SBOM
- Supply-chain hardening aligned with org Q4 (package signing/SBOM, tunaos#1187)
- Backend support matrix + upgrade/migration documentation (5 backends × tailnet)

---

## Technical Debt Backlog

| Item | Issue | Priority | Effort |
|------|-------|----------|--------|
| Installer downloads an unverified binary despite publishing `SHA256SUMS` | #210 | P1 | S |
| 62 per-commit plugin releases crowd the product release feed | #210 | P1 | M |
| `pkg/proxmox` (compat server) vs `pkg/proxmoxbe` (client) naming confusion | CONTEXT.md | P2 | S |
| Legacy `tailvm` prefix migration completion | SPEC.md | P2 | M |

---

## How to Contribute

Issues are triaged with `needs-triage` / `ready-for-agent` / `ready-for-human`
labels (see `docs/agents/issue-tracker.md`). The open surface is small right
now — #205 (`--json` output) is the most self-contained pick, #69 is the large
one, and #210 is the highest-impact for anyone who cares about how the project
reaches new users.

## Roadmap Governance

Maintained by the strategist agent; updates after major milestones or
quarterly. Propose changes via PR to this file with an issue reference.

**Currency rule**: a tracker cited in this file that closes must move its row in
the same PR, or the row must name a successor. The 08-10 revision of this file
listed three P0s that all closed the following day and stayed marked open for
twelve days, while pointing newcomers at one of them as a `help wanted` pick.

---
*Generated by strategist agent at ACMM L6 — full mode (ISSUES_AND_PRS).*
