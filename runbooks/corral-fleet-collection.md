# Runbook: Corral Fleet Metrics & Backend Collection Health

## Summary

Corral exposes Prometheus metrics under `GET /metrics` when enabled with `corral web --metrics`. Collection runs asynchronously in the background:
- Fleet inventory updates every 30 seconds (`metricsInterval`).
- Doctor checks run every 5 minutes (`doctorInterval`).

This runbook covers alerts related to metrics collection age, collection failures, and backend context unreachability.

---

## Alerts & Thresholds

| Metric / Alert | Threshold | Severity | Description |
|---|---|---|---|
| `corral_collection_success == 0` | 1 scrape failure | Critical | Fleet snapshot collection failed or collection has not run |
| `corral_collection_age_seconds > 120` | > 120s | Warning | Snapshot is stale (background collector loop may be frozen) |
| `corral_backend_up == 0` | per context | Warning | A configured backend context (KubeVirt, Proxmox, QEMU, Incus) failed to answer during snapshot fan-out |
| `corral_backend_error == 1` | per context | Warning | Fleet collection reported explicit error for target context |

---

## SLO & SLI Definitions

### Fleet Collection Availability (SLO: 99.5%)
- **SLI:** Ratio of successful background fleet snapshot collections over total collection attempts:
  $$\text{SLI}_{\text{collection}} = \frac{\sum \text{corral\_collection\_success == 1}}{\sum \text{total collection ticks}}$$
- **Target:** 99.5% over a 30-day rolling window.

### Fleet Metrics Freshness (SLO: 99.0%)
- **SLI:** Percentage of time `corral_collection_age_seconds <= 60` during active scraping.
- **Target:** 99.0% over a 30-day rolling window.

---

## Incident Response & Triage

### 1. `corral_collection_success == 0`
**Symptoms:**
- PromQL query `corral_collection_success` returns `0`.
- Scraper receives HTTP 200 with `corral_collection_success 0` body.

**Triage Steps:**
1. Check if `corral web` was started with `--metrics`.
2. Inspect `corral web` stdout/stderr logs for panic or connection errors.
3. Verify local store initialization (`registry.Store`).
4. If background ticker deadlocked, restart the `corral web` service:
   ```bash
   systemctl restart corral-web
   ```

### 2. `corral_backend_up{context="<name>"} == 0`
**Symptoms:**
- Specific backend context shows `corral_backend_up = 0` and `corral_backend_error = 1`.

**Triage Steps:**
1. Identify the failing context and backend from labels:
   ```promql
   corral_backend_up == 0
   ```
2. Test reachability to target backend CLI/API manually:
   - For **KubeVirt**: `kubectl --context <ctx> get vmi`
   - For **Proxmox**: Check PVE API endpoint HTTPS reachability & API token validity.
   - For **QEMU/libvirt**: `virsh -c qemu:///system list`
   - For **Incus**: `incus list`
3. Verify network policies, firewall rules, or credentials for the unreachable backend.

---

## Escalation

- **Primary:** Fleet Operations On-Call
- **Secondary:** Platform Engineering (`#corral-ops`)
