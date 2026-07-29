# ADR-0006: Optional authentication is a gateway plugin

**Status:** accepted
**Date:** 2026-07-29

## Context

ADR-0003 uses identity headers asserted by Tailscale ingress. Corral now also
supports ingress-agnostic and peer deployments, where that single trust path
does not always exist. Pulling OIDC, sessions, and WebAuthn into the core
would increase the main binary and couple every deployment to optional auth.

## Decision

Ship `corral-auth` as a separate reverse-proxy plugin in front of `corral web`.
It uses `coreos/go-oidc` for discovery and ID-token verification, OAuth2
authorization code flow with PKCE and nonce, and Gorilla encrypted cookie
sessions. It removes client-supplied identity headers and sets the existing
identity contract only after authentication. Go's reverse proxy preserves
API streaming and WebSocket upgrades.

Tailscale ingress remains a supported first-class identity adapter. Deployments
choose either a trusted Tailscale-only path or the auth gateway; KubeVirt and
the core web server remain ingress-agnostic.

Passkeys belong in this plugin using `go-webauthn`, but require a credential
store, RP ID/origin, enrollment bootstrap, and recovery policy. They are not
represented as complete until those pieces ship.

## Consequences

- The main Corral binary does not link OIDC/session dependencies.
- The gateway can be upgraded independently and reused for Corral peers.
- The upstream Corral listener must not be publicly reachable when trusting
  gateway identity headers.
