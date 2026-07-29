# ADR-0003: Identity source for the web UI (Tailscale headers, not OIDC)

**Status:** accepted for Tailscale-only deployments; extended by ADR-0006
**Date:** 2026-06-21

## Context

corral-web mutates a real cluster (start/stop/delete VMs, create disks, live
migrate). Until now it was unauthenticated: anyone who could reach the pod had
full control. We want to (1) show *who* is using the UI and (2) split
read-only from admin, without standing up an identity provider.

corral-web is reachable **only** through the Tailscale operator Ingress
(`ingressClassName: tailscale`). Tailscale already authenticates every
connection at the network layer (WireGuard + tailnet ACLs) and the operator
proxy forwards the caller's verified identity as request headers:

- `Tailscale-User-Login` — the tailnet login (e.g. `alice@github`)
- `Tailscale-User-Name` — display name

Because the proxy terminates the tunnel and sets these headers itself, a client
cannot spoof them: traffic that didn't traverse the Tailscale proxy never gets
them, and the proxy overwrites any client-supplied values.

## Decision

**Use the Tailscale ingress identity headers as the identity source.** Do not
build or require OIDC for this slice.

- The server reads `Tailscale-User-Login` / `-Name` per request (`caller()`).
- An admin allowlist is configured out-of-band via the `CORRAL_ADMINS`
  environment variable (comma/space separated tailnet logins).
- Authorization is **deny-by-default for mutations when an allowlist exists**:
  - `CORRAL_ADMINS` **unset/empty** → single-user / open mode: every caller is
    an admin (preserves prior behaviour; safe for a personal cluster).
  - `CORRAL_ADMINS` **set** → only listed logins may perform mutating requests;
    everyone else is read-only.
- Enforcement is an `adminGate` middleware: safe methods (GET/HEAD/OPTIONS)
  always pass; mutating methods (POST/DELETE/PATCH/PUT) require an admin, else
  `403`. This is **defense in depth** — the SPA also hides mutating controls,
  but the server is the boundary.
- `GET /api/whoami` exposes `{login, name, admin, enforced}` so the UI can show
  the logged-in user and switch to read-only mode.

### Trust boundary

The headers are trusted **only** because the deployment guarantees all traffic
arrives via the Tailscale operator proxy (no other Ingress/Service path to the
pod). If corral-web is ever exposed by a second path, this assumption breaks
and the gate must move behind an authenticating proxy. Documented as a risk on
the deployment manifest.

## Alternatives considered

- **OIDC (Dex/Keycloak/cloud IdP):** the "correct" long-term answer and the
  path to real per-VM RBAC (see ADR-0001's K8s-RBAC → Proxmox-privilege
  mapping). Rejected for this slice: it needs an IdP, client registration, and
  session handling — disproportionate when Tailscale already proves identity.
  Revisit when corral-web grows beyond a single tailnet or needs SSO.
- **mTLS client certs:** strong but operationally heavy (cert distribution);
  redundant with Tailscale's existing device identity.
- **Kubernetes `TokenReview` / impersonation:** ties UI auth to kube tokens,
  which tailnet users don't hold; out of scope.

## Consequences

- Zero new infrastructure; works the moment `CORRAL_ADMINS` is set.
- Identity is only as strong as the tailnet ACLs and the single-ingress
  assumption above.
- A natural upgrade path remains: swap `caller()` for an OIDC-session lookup
  later without changing the gate or the UI contract.
