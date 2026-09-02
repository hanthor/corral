# Ephemeral CT-backed VDI pools

CT-backed pools are declarative pools of Kubernetes Containers (CTs), not
KubeVirt VMs. The pool definition is stored in a cluster-visible ConfigMap
named `ct-pool-<pool>` with the image, resource shape, storage class, and
desired member count. Each member has a stable `<pool>-<n>` identity and a
PVC named `<member>-data`.

## Prerequisites and security

- Kubernetes access to create ConfigMaps, Pods and PVCs in the target
  namespace, plus Lease objects in `coordination.k8s.io/v1`.
- A pullable Linux container image and sufficient CPU/memory/PVC capacity.
- A StorageClass supporting the selected access mode (`ReadWriteOnce` by
  default). Unprivileged CTs persist `/data`; privileged CTs persist a
  seeded root filesystem and require the explicitly requested privileged
  security context.
- Network policy allowing the CT's intended services. CTs do not acquire a
  graphical VM framebuffer; the normal CT console is terminal/exec-based.
- No card PINs, cloud credentials, or other tenant secrets in the pool image
  or ConfigMap. Use Kubernetes Secrets and least-privilege service accounts.

Ephemeral is mandatory in this first reconciler. Persistent CT pool semantics
are rejected explicitly because a replacement must not destroy tenant state.

## Reconciliation model

`pkg/vdi.Reconcile` is safe to run repeatedly. It creates missing members,
starts a stopped member whose PVC still exists, removes unassigned members
outside the desired size, and records counts/errors in the pool ConfigMap's
`status` field. Assigned members are protected from scale-down and drift
repair. Partial failures leave the definition and status in the cluster so a
later reconcile can retry safely.

Pool membership is represented on the member PVC with
`corral.dev/ct-pool=<pool>` and `corral.dev/ct-member=<member>`. Assignment
uses the same per-member Kubernetes Lease primitive as VM pools; the
`corral.dev/vdi-assigned-to` PVC label is presentation state. Lease creation
is first-writer-wins, and stale recovery uses resource-version compare-and-swap.

Releasing a CT deletes its pod and data PVC, releases the owner-checked Lease,
and immediately reconciles the pool to recreate the member from the declared
image. If deletion or recreation fails, the error is returned and the
cluster-visible definition/status remains available for retry.

CT pool reconciliation does not provide graphical WebRTC/VNC streaming,
input-idle detection, or a persistent CT mode. Those require separate design
and security work.
