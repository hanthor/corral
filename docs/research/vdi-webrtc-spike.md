# WebRTC for ephemeral Linux desktop pools — spike decision record

**Status:** Defer implementation; keep VNC/RDP/TTY as the supported paths

**Date:** 2026-08-31

**Scope:** Ephemeral Linux desktop sessions only. Windows, VM-console
replacement, and a production WebRTC dependency are explicitly excluded.

## Executive decision

Do not add WebRTC or Selkies to Corral yet. Selkies is a credible, actively
maintained container desktop streamer and its WebRTC mode can improve
interactive video over high-latency links. It is not, however, a drop-in
transport for Corral's KubeVirt VM consoles: it captures a desktop from
inside a container, owns the display/input/audio stack, and introduces ICE,
TURN, media-image, GPU-device, and authentication operations that Corral
would have to operate.

The external QEMU D-Bus display path is a better long-term integration seam
for VM-backed pools, but it is not a shipped Corral/KubeVirt path. QEMU
currently documents `-display dbus` and its `qemu-vnc` D-Bus consumer; this
spike found no supported KubeVirt abstraction that exposes that bus to a
Corral service. Building against it now would couple Corral to custom
virt-launcher/QEMU configuration.

**Recommendation:** defer product implementation until an ephemeral
container prototype is run on a representative GPU-enabled cluster and the
KubeVirt/QEMU D-Bus path has a supported exposure story. Do not make this a
required VDI path. Revisit if the exit criteria below are met.

## Evidence reviewed

The spike was time-boxed to source and documentation review plus a runnable
prototype plan. The checked-out Selkies `main` revision was
`7bcdd54e93b6323078cdcd93f4a8954bef0b3468` (2026-08-31). Its documentation
states:

- the reference container carries the desktop, browser and audio stack;
- WebRTC is opt-in (`SELKIES_MODE=webrtc`), while the default WebSocket mode
  uses one TCP port;
- H.264 is the WebRTC video encoder; hardware encoding uses NVENC or VA-API
  when the host GPU and driver are exposed, otherwise software encoding is
  possible;
- keyboard, mouse, gamepad, microphone and audio are separate media/input
  paths; and
- containerized Kubernetes deployments without host networking need STUN and
  usually an externally operated TURN service. Relay traffic adds latency.

References:

- [Selkies start guide](https://github.com/selkies-project/selkies/blob/main/docs/start.md)
- [Selkies settings](https://github.com/selkies-project/selkies/blob/main/docs/settings.md)
- [Selkies firewall/TURN guidance](https://github.com/selkies-project/selkies/blob/main/docs/firewall.md)
- [Selkies secure mode](https://github.com/selkies-project/selkies/blob/main/docs/secure-mode.md)
- [Selkies license inventory](https://github.com/selkies-project/selkies/blob/main/docs/licensing.md)

The checked-out QEMU `staging` revision was
`d2e570cc0f97b936902a5b1b86b73c0f5998b475` (2026-08-28). QEMU has a
D-Bus display interface and a documented `qemu-vnc` helper:

- [QEMU D-Bus display interface](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/interop/dbus-display.rst)
- [QEMU VNC D-Bus helper](https://gitlab.com/qemu-project/qemu/-/blob/master/docs/tools/qemu-vnc.rst)

Corral's current architecture uses `virtctl vnc --proxy-only`, serial
console bridges, and the existing RDP bridge. Those paths remain unchanged
by this record.

## Prototype design (no production dependency)

The smallest useful experiment is one **ephemeral Linux container desktop**,
not a VM console. Run it in a disposable namespace with a Selkies reference
image and an explicit `SELKIES_MODE=webrtc` setting. Do not add Selkies to
Corral's image catalog or runtime.

The prototype must record:

1. cold-start time, reconnect time, and time to first frame;
2. glass-to-glass latency at idle and during pointer/scroll movement;
3. video bitrate, packet loss, jitter and RTT for direct ICE and TURN relay;
4. CPU/GPU utilization, encoder selection, memory and container footprint;
5. keyboard, pointer, clipboard, resize and disconnect/reconnect behavior;
6. speaker audio and microphone behavior, including permission boundaries;
7. authentication handoff from the broker identity to the streamer; and
8. behavior with no GPU, VA-API, and an assigned GPU device.

A run is valid only when the same desktop is measured through the existing
Corral VNC path as a baseline. A WebRTC result without a baseline is not
evidence of improvement.

### Reproducible disposable run

The following is an experiment recipe, not a Corral feature. Pin the image
to a reviewed digest before running it in a real cluster:

```bash
# In a disposable namespace, with a reviewed digest of the documented image:
read -rsp 'temporary spike password: ' SELKIES_PASSWORD; echo
kubectl create namespace vdi-webrtc-spike
kubectl -n vdi-webrtc-spike run selkies-spike \
  --image=ghcr.io/selkies-project/selkies/desktop:main-ubuntu26.04@<reviewed-digest> \
  --port=8080 \
  --env=SELKIES_MODE=webrtc \
  --env=SELKIES_BASIC_AUTH_USER=spike \
  --env=SELKIES_BASIC_AUTH_PASSWORD="$SELKIES_PASSWORD"
# Expose only through a private port-forward for the first run.
kubectl -n vdi-webrtc-spike port-forward pod/selkies-spike 8080:8080
```

The illustrative command requires an image digest and uses a temporary
password held in a shell variable; never put a real password in shell
history. A proper follow-up should use a checked-in Deployment/Secret
template in a separate experiment repository, not Corral production
manifests.

This environment did not have Podman/Docker, a Kubernetes cluster, a browser,
or a GPU available, so the live connection and numerical measurements were
not claimed as completed by this spike. That is an explicit result: the
prototype needs a representative cluster and must not be simulated as a
production decision.

## Comparison of integration seams

| Dimension | Selkies inside ephemeral container | External QEMU D-Bus display bridge |
|---|---|---|
| First-frame/latency | Potentially excellent with H.264/WebRTC and a local GPU; TURN can add latency | Depends on bridge and encoder; preserves guest display semantics |
| Bandwidth | Adaptive WebRTC media, but video plus audio and ICE/TURN operations | Bridge must define encoding and transport; no automatic WebRTC benefit |
| GPU | Requires `/dev/dri`, driver/libva or NVENC image compatibility and scheduling | Uses the VM's virtual/assigned display; bridge still needs an encoder |
| Input/audio | Selkies owns input injection and PulseAudio/PipeWire capture in the image | Must translate QEMU display/input/audio interfaces and permissions |
| Authentication | Selkies tokens/basic auth plus Corral/broker identity mapping | Corral controls the bridge boundary, but must secure the D-Bus socket |
| Ingress/relay | STUN/TURN, UDP policy, relay capacity and credentials are required | Can reuse Corral's existing WebSocket ingress/peer relay, but media encoding remains work |
| Operational footprint | Large desktop/media image, container privileges/devices, TURN | Custom virt-launcher/QEMU exposure and a new bridge service |
| Pool fit | Good candidate for CT-backed ephemeral pools once CT reconciliation exists | Better candidate for VM-backed pools, but upstream seam is unsettled |

## Security and operations

A WebRTC data channel is not an authorization boundary. The broker must mint
short-lived, member-scoped credentials and revoke them on release. The
streamer must reject input from viewer sessions server-side, not merely hide
browser controls. TURN credentials must be short-lived and relay traffic
must be capacity-limited. Browser microphone, webcam, clipboard and file
transfer permissions must be disabled unless explicitly required.

GPU access changes the threat model: exposing `/dev/dri` or vendor devices to
a pooled container requires a node/device policy, compatible image drivers,
and a way to prevent one tenant from using the device outside the desktop
process. A no-GPU software fallback is required for correctness, but it is
not assumed to meet latency or density targets.

The current Corral peer relay and private-ingress model can carry signaling,
but it does not automatically provide UDP media reachability or TURN. A
WebRTC implementation must document relay topology, egress cost, metrics,
credential rotation and failure behavior before it is considered for a
multi-user pool.

## Proceed / defer / reject criteria

**Proceed** only if a representative run demonstrates, against VNC, a
material interactive improvement (target: p95 input-to-photon under 100 ms
on the intended network), no unacceptable input/audio regressions, a
measurable bandwidth or quality advantage, and an operable private
STUN/TURN/authentication design. The image must run without privileged host
access beyond explicitly scheduled GPU devices, and the result must fit the
pool's startup and density budget.

**Defer** (the current decision) if the advantage depends on a custom GPU
image, TURN is not operationally bounded, the broker/session identity cannot
be mapped safely, or the D-Bus path has no supported KubeVirt exposure.

**Reject** WebRTC for this pool type if the measured no-GPU path cannot meet
interactive targets, if audio/input or browser permission behavior is
unacceptable, or if operating the media and relay plane costs more than the
latency/bandwidth benefit. Rejection here does not affect VNC/RDP/TTY.

## Follow-up

No Corral production implementation follows from this spike. The next
artifact, if the required cluster becomes available, is a disposable
experiment repository containing the pinned image, namespace policy, TURN
configuration, measurement script and VNC comparison report. Only after
that report passes the proceed criteria should Corral add a `streaming`
capability or a broker route for ephemeral Linux pools.
