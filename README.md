# feedback_ZSL

# ConnectLab

ConnectLab is an offline, deterministic local service for reproducing and
attributing HTTP connection resets across:

```text
local client -> forward proxy 127.0.0.1:18080 -> upstream 127.0.0.1:18081
```

It runs on Python's standard library only. SQLite, asyncio TCP/HTTP, JSON, and
Prometheus text exposition are built in; no internet access or third-party
package installation is required.

## What It Delivers

- Controlled short-connection, keep-alive reuse, and large-upload traffic with
  configurable count and concurrency.
- Per-request client/proxy/upstream connection identifiers, start time, path,
  body size, outcome, and complete event timeline.
- An explicit missing-hop model. A missing event is marked as
  `missing_hop`, `missing_request_event`, or `missing_required_event`; it is
  never interpreted as a normal close.
- Three deterministic fault classes plus an unclassified reset class.
- Config-driven fault switching with path scoping, duration, and ratio.
- Pool-reuse failure detection, including whether the proxy knew the old
  connection was closed before sending on it.
- Atomic SQLite batch commits, immutable batch IDs, restart-safe history, and
  stable duplicate-batch errors.
- Batch deadlines, per-hop timeouts, incomplete-request retention, JSON status
  queries, comparison queries, and Prometheus metrics.

## Quick Start

Python 3.11 or newer is recommended.

```powershell
python -m connectlab init
python -m connectlab serve
```

The service starts all three logical roles in one process but on real local
TCP ports:

- API: `127.0.0.1:18190`
- Observer forward proxy: `127.0.0.1:18080`
- Test upstream: `127.0.0.1:18081`

Use the same `CONNECTLAB_CONFIG` environment variable or `--config` argument in
every terminal when using a non-default config.

### Run A Batch

```powershell
python -m connectlab batch `
  --batch-id demo-baseline `
  --count 24 `
  --concurrency 4 `
  --path short `
  --path keepalive `
  --path upload
```

The CLI calls the HTTP API. A batch response contains `batch_id`, `status`, and
the attribution summary.

### Switch A Fault

```powershell
python -m connectlab inject --id demo-syn --type syn_drop --path short --ratio 1 --duration-ms 600000
python -m connectlab batch --batch-id demo-syn --count 8 --concurrency 2 --path short

python -m connectlab inject --id demo-upstream --type upstream_disconnect --path keepalive --ratio 1 --duration-ms 600000
python -m connectlab batch --batch-id demo-upstream --count 12 --concurrency 1 --path keepalive

python -m connectlab inject --id demo-timeout --type proxy_read_timeout --path upload --ratio 1 --duration-ms 600000
python -m connectlab batch --batch-id demo-timeout --count 3 --concurrency 1 --path upload --per-hop-timeout-ms 500
```

Switching injection does not edit proxy or upstream source. The active profile
is snapshotted into each batch.

SYN loss is simulated by a loopback egress fault gate inside the observer
proxy. It does not change sysctl, firewall, WFP, routing, or any other global
network parameter.

### Inspect Results

```powershell
python -m connectlab status --batch-id demo-syn
python -m connectlab metrics
python -m connectlab latest-injection
```

The status JSON contains stable, sorted fields for:

- `attribution_summary`
- `batch_injection` and `latest_injection`
- every request's `event_sequence` and `coverage`
- `unexplained_reset_sequences`

## Fault Attribution

| Injection type | Attribution type | Evidence | Attribution hop / edge |
| --- | --- | --- | --- |
| `syn_drop` | `syn_not_reached_upstream` | proxy `syn_dropped`, `syn_ack_received=false`, no upstream event | `proxy` / `proxy_to_upstream` |
| `upstream_disconnect` | `upstream_early_disconnect` | upstream `reset`/`write_failed` before a valid response | `upstream` / `proxy_to_upstream` |
| `proxy_read_timeout` | `proxy_read_timeout` | proxy timer event in `upstream_response_head` phase | `proxy` / `proxy_to_upstream` |
| Any other reset | `unexplained_reset` | no matching known signature; full sequence retained | first abnormal hop / `unknown` |

For keep-alive reuse, an upstream disconnect after a response can leave a
closed socket in the proxy pool. The next request records:

- `reuse_attempt` with `proxy_aware_before_send`
- proxy `reset` with `reuse_failure=true`
- `reuse_failure` with `fault_class=reuse_failed_connection`

The request is counted in both `upstream_early_disconnect` and the
`reuse_failures` metric. Its attribution subtype is
`reuse_failed_connection`.

## Event Model

Each event has:

- `hop`: `client`, `proxy`, or `upstream`
- `connection_id`: `c-*`, `p-down-*`, `p-up-*`, or `u-*`
- `peer_connection_id` and `correlation_id` for cross-hop joins
- `kind`: connect, request, response, close, reset, timeout, reuse, etc.
- `initiator`: `client`, `proxy`, `upstream`, `peer`, `timer`, or fault gate
- UTC timestamp and monotonic timestamp
- structured `details`

The proxy never writes an open record for a connection ID after that ID has
been closed. Duplicate terminal events on the same hop/connection are
suppressed. SQLite also has a unique terminal-event index as a second guard.

## HTTP API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness |
| `POST` | `/v1/batches` | Execute and commit a batch |
| `GET` | `/v1/batches` | List committed batches |
| `GET` | `/v1/batches/{id}` | Attribution/status JSON |
| `GET` | `/v1/batches/{id}/events` | Persisted event query |
| `POST` | `/v1/injections` | Atomically switch injection |
| `GET` | `/v1/injections/latest` | Latest injection config |
| `GET` | `/v1/compare?batch_a=A&batch_b=B` | Compare reset rates and attribution |
| `GET` | `/metrics` | Prometheus text exposition |

Example batch body:

```json
{
  "batch_id": "batch-001",
  "count": 24,
  "concurrency": 4,
  "paths": ["short", "keepalive", "upload"],
  "per_hop_timeout_ms": 1500,
  "request_timeout_ms": 5000,
  "batch_timeout_ms": 20000,
  "target": "local-a"
}
```

Duplicate committed `batch_id` returns HTTP `409` with stable code
`BATCH_ALREADY_EXISTS`. A currently running duplicate returns
`BATCH_ALREADY_RUNNING`. Committed batches are never updated.

All `POST /v1/batches` work is committed in one transaction containing the
batch row, request rows, events, attribution conclusions, and injection
snapshot. A crash before commit leaves no partial batch.

## Prometheus Metrics

Metrics are gauges scoped by `batch_id`:

```text
connectlab_batch_requests_total{batch_id,path}
connectlab_batch_attributions_total{batch_id,fault_type,hop}
connectlab_batch_unexplained_resets_total{batch_id}
connectlab_batch_timeouts_total{batch_id}
connectlab_batch_connection_reuses_total{batch_id}
connectlab_batch_reuse_failures_total{batch_id}
```

The three fault/hop label pairs are emitted for every committed batch, including
zero counts, so dashboard queries remain stable.

## Timeouts And Deadlines

- A connect or response wait that exceeds the configured per-hop timeout is
  recorded on that hop and returned with all events available from other hops.
- A batch deadline cancels remaining work, preserves completed request
  conclusions, and marks unfinished requests as `incomplete`. The batch status
  becomes `partial_timeout`.
- The service never waits unboundedly for an upstream response.

## Configuration And Alternate Local Instances

`config/connectlab.json` controls listener addresses, pool limits, timeouts,
samples, and target profiles. `config/proxy.json` and `config/upstream.json` are
generated role-specific config snapshots. `data/request_sequence.json` and the
three seeded body files are generated by `python -m connectlab init`.

Add another profile under `targets` and submit a batch with `"target":
"local-b"`; the proxy validates that the target belongs to the configured
profiles. Restarting with a config that points at a different local
proxy/upstream pair uses the same event and attribution pipeline.

## Envoy 1.30

`config/envoy.yaml` is a runnable Envoy 1.30 baseline for the fixed-upstream
forward-proxy lab listener on `127.0.0.1:18080`, upstream `127.0.0.1:18081`,
and admin `127.0.0.1:18082`. `scripts/start_envoy.ps1` starts a locally
installed Envoy executable and writes JSON access logs.

The default `embedded` observer proxy is used for the acceptance path because
stock Envoy access logs cannot deterministically report a dropped local SYN,
the exact pool-reuse state before send, or a common close initiator for both
downstream and upstream sockets. The observer proxy exposes the same local
forward-proxy socket contract while recording those connection-state details.
No internet access is used by either path.

## Verification

```powershell
python scripts/verify_scenarios.py
```

The integration verifier starts isolated local ports and checks:

- baseline success across all three paths
- alternate target profile `local-b` healthy and fault-consistent
- SYN drop attribution and explicit missing upstream hop
- upstream early disconnect plus stale pooled-connection reuse failure
- proxy read-timeout attribution with available upstream events
- path-scoped injection leaving unaffected paths clean
- unexplained-reset classification and full sequence retention
- duplicate batch rejection
- Prometheus metric names and labels
- batch deadline partial completion
- restart-safe batch history and latest injection configuration

The latest verified result is in `artifacts/verification.json`.

Unit tests:

```powershell
python -m unittest discover -s tests -v
```

## Resource Profile

- No external runtime services or packages.
- Default concurrency cap is 16 and default sample upload is 4 MiB.
- Batch execution uses at most the configured worker count plus three service
  listeners.
- SQLite uses WAL mode but each batch is committed atomically.
- Generated runtime data stays under `var/`, `logs/`, and `data/`; the
  verifier removes its temporary SQLite database and injection file.
