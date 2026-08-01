# ADR-0011: Exposing Corral to Prometheus

**Status:** accepted
**Date:** 2026-08-01

## Context

Every backend Corral aggregates already has metrics, and none of them answer the
question an operator of *this* tool has. KubeVirt has kube-state-metrics and
`kubevirt_vmi_*`; Proxmox has `pve-exporter`; libvirt has `libvirt-exporter`;
QEMU-under-systemd and Incus have whatever you build yourself. Standing up four
exporters gives four disjoint views, each labelled in its own vocabulary, and
none of them knows that `web-1` on KubeVirt and `web-2` on Proxmox are the same
application stack — because that fact lives in Corral's folders (ADR-0008) and
nowhere else.

So the value Corral adds is not "metrics about VMs". It is **the fleet as one
series set, labelled by the axes Corral knows about and the backends do not**:
which backend and context an instance is on, which pool an operator put it in,
and whether Corral's own view of the world is healthy.

Two facts constrain the design more than anything else:

- **Corral has no database.** Inventory is assembled per request by fanning out
  to every configured context — `fleet.List` runs `kubectl`, `incus list`,
  `virsh`, `systemctl`, and HTTPS calls to PVE, concurrently. That is fine at a
  human's click rate and actively bad at a scraper's.
- **Partial failure is the normal state.** A context being down is not an
  outage of Corral; `fleet.List` already returns healthy inventory alongside a
  per-context error map. Metrics must preserve that distinction rather than
  reporting a down context as zero VMs.

## Decision

### `GET /metrics` on `corral web`, in the text exposition format

Not a separate `corral exporter` daemon. The web server already holds the
inventory, the folder tree, the task log, and the doctor — an exporter process
would rebuild all four and then disagree with the UI about the fleet, which is
a worse failure than not having one.

Served at `/metrics`, outside `/api`, because that is where scrapers look.

### Served from a cached snapshot, with the cache age exposed as a metric

A scrape must never fan out to the backends. A snapshot is refreshed on a timer
and `/metrics` renders whatever the last one holds, so a scrape is a string
build over in-memory data and costs the cluster nothing regardless of how many
Prometheis point at it.

The cost of that is staleness, so the staleness is itself a metric:

    corral_collection_age_seconds
    corral_collection_duration_seconds
    corral_collection_success

Hiding the age would make a frozen collector indistinguishable from a stable
fleet — the metrics would keep scraping green while reporting an hour-old world.
Publishing it means an operator can alert on `corral_collection_age_seconds >
120` and know the difference. `corral_collection_success` covers the case where
the collector ran and failed outright.

### Labels are the axes Corral knows and the backends do not

    corral_instance_info{name,backend,context,namespace,node,pool,template,bootc}
    corral_instance_running{name,backend,context,namespace,pool}
    corral_instance_ready{...}
    corral_instance_cpu_cores{...}
    corral_instance_memory_bytes{...}
    corral_instances{backend,context,state}       # fleet-level counts
    corral_pool_instances{pool}
    corral_pool_running{pool}

`context` is the name the operator gave that context in config, not the
backend's own context string. The two differ — `fleet.List` keys its per-context
errors by the config name while the instances it returns carry the backend
context — and using the raw value would leave `corral_backend_up{context="kubevirt"}`
and `corral_instance_running{context=""}` describing one cluster under two
labels that no query could relate. A context absent from the config (a peer, or
one removed since) keeps its raw value rather than vanishing.

`pool` is the load-bearing one and the reason this exists rather than four
exporters: it is the only label that spans backends, and it is what makes
`sum by (pool) (corral_instance_running)` mean "is my application stack up"
across a KubeVirt cluster and a Proxmox host at once. An instance in no pool
carries `pool=""` rather than being omitted — dropping it would make the pool
sum silently understate the fleet.

Per-instance series are emitted for every instance, which is the usual
cardinality trade. A fleet Corral is plausibly managing is hundreds of guests,
not millions of HTTP routes; the series count is bounded by the fleet size and
an operator who wants only aggregates can drop the instance metrics at scrape
config. This is worth stating rather than assuming: if someone points Corral at
a 50,000-VM estate, `corral_instances` still works and the per-instance series
are what they should drop first.

### Backend reachability is a first-class metric, not an absence

    corral_backend_up{context,backend}
    corral_backend_error{context,backend}     # 1 when the last collection failed

`fleet.List`'s error map becomes series rather than being flattened into
"fewer VMs". A KubeVirt context that is unreachable must not look like a
KubeVirt context with nothing in it — that is precisely the alert you want, and
it is invisible in the naive rendering.

### Doctor checks are gauges

    corral_check{name,backend,context,severity} 1|0

`pkg/doctor` already produces a structured list of named checks with severities
and a fixable flag. Exposing them means "KubeVirt's CDI is missing" can page
someone instead of waiting to be noticed in a browser tab, and the severity
label lets an alert rule distinguish `required` from `info` without a hardcoded
list of check names.

Doctor runs on its own, slower timer than the inventory: its checks shell out
more heavily and change far less often.

### Task counters

    corral_tasks_total{action,status}

Derived from the existing task-log ring, which means it is a gauge over a
bounded window rather than a true monotonic counter — the ring drops its oldest
entries. That is a real caveat and the metric is named and documented as what
it is, because a counter that silently decreases breaks `rate()` in a way that
is hard to debug from the graph.

### No client_golang

The text exposition format is a stable, line-oriented format that this package
writes in about a hundred lines. `client_golang` brings a global registry, a
protobuf dependency, and a collector model built around instrumenting a process
as it runs — incrementing counters at the point of work. Corral's metrics are
none of that: they are a projection of a snapshot, computed all at once, with
no state of their own to register. The library would be a dependency taken on
to do the easy part while fitting the model badly.

The commitment this makes is that escaping is our problem — label values here
include instance names and doctor detail strings, which contain quotes and
backslashes. It is one function, and it has tests that feed it exactly those.

## Consequences

- Grafana dashboards become possible without shipping one: the label set is the
  API, and `pool` is what makes a cross-backend dashboard expressible at all.
- The snapshot timer is a background goroutine doing real work (a full
  `fleet.List`) whether or not anyone is scraping. It is off unless
  `--metrics` is passed, so a `corral web` on a laptop does not poll five
  backends forever.
- Alerting on Corral's own health becomes possible — `corral_collection_success
  == 0` and `corral_backend_up == 0` are the two that matter, and neither is
  expressible today.
- Authentication is the web server's existing concern. `/metrics` sits behind
  the same middleware as everything else, which means a tailnet-gated Corral has
  a tailnet-gated metrics endpoint and a scraper needs to be on the tailnet.
  That is the right default; a `--metrics-anonymous` escape hatch is deliberately
  not provided until someone has the deployment that needs it.

## Alternatives considered

**A separate `corral exporter` binary.** Rejected: it would duplicate inventory,
folders, and doctor, and then disagree with the UI. The two views of the fleet
must come from one collection.

**Scrape-time collection.** Rejected. A 15-second scrape interval would run
`kubectl get vms` against every context four times a minute forever, and two
Prometheis would double it. The cache is not an optimisation here; it is what
makes the endpoint safe to expose.

**Push to a Pushgateway.** Rejected: Corral is a long-lived server, not a batch
job, and the Pushgateway's staleness semantics are exactly the trap that
`corral_collection_age_seconds` exists to avoid.

**Reusing the KubeVirt metrics that already exist.** They are good, and an
operator running only KubeVirt should use them. They cannot answer a
cross-backend question, and cross-backend is the only question Corral is
uniquely placed to answer.

## Not in scope

Historical storage (Prometheus is the historian), alert rules and dashboards as
shipped artifacts, per-guest CPU and memory *utilisation* — the existing CPU
ring is KubeVirt-only, and a `corral_instance_cpu_used` that silently covers one
backend of five would be worse than absent. When the `Observer` family covers
every backend, that becomes a straightforward addition on the same snapshot.
