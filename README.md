# Two-tier ONNX inference router

This repository implements a local, two-tier inference router for a Windows 10/11
host with two logical CPUs and 1.25 GiB RAM. It owns the replica manifest, health
view, request-level routing, idempotency, second-tier failover, SQLite persistence,
Prometheus metrics, JSON status APIs, and deterministic load/fault scenarios.

The default topology is:

- Tier `l1` / group `embedding`: three replicas (`l1-a`, `l1-b`, `l1-c`)
- Tier `l2` / group `ranking`: two replicas (`l2-a`, `l2-b`)
- Each replica loads its own 64-in/64-out ONNX file and listens on loopback.
- The router listens on `127.0.0.1:18080` (gRPC) and `127.0.0.1:18081` (HTTP).

## Setup

Use Python 3.11. Install the exact requested dependencies:

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install --upgrade pip
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe scripts\generate_models.py --out models --seed 20261119
```

On Linux/macOS the equivalent commands use `.venv/bin/python`.

The model generator writes valid ONNX MatMul models without requiring an extra
ONNX package. Generated files are deterministic for a fixed seed.

## Run the stack

```powershell
.\.venv\Scripts\python.exe scripts\run_stack.py --fresh-db
```

This starts five replica processes, waits for their health RPCs, then starts the
router. Press `Ctrl+C` to stop everything. The SQLite database defaults to
`data/router.sqlite3`.

Useful endpoints:

- `POST /v1/infer`: JSON inference (`request_id`, `session_id`, `features`)
- `GET /v1/requests/{request_id}`: committed final result, actual calls, events
- `GET /status`: replica availability, routing preference, last eviction
- `GET /stats`: rolling end-to-end and per-layer statistics
- `GET /metrics`: Prometheus text exposition
- `GET /healthz`: router liveness

Example inference:

```powershell
$body = @{
  request_id = "req-demo-1"
  session_id = "session-demo-1"
  features = @(0.1) * 64
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18081/v1/infer `
  -ContentType application/json -Body $body
```

The gRPC equivalent is `inference.RouterInference/Infer`.

## Configuration

The checked-in manifest is `config/router.json`. Every shard declares:

```json
{
  "layer": "l2",
  "replica_group": "ranking",
  "replica_id": "l2-a",
  "model_path": "../models/l2-a.onnx",
  "listen_address": "127.0.0.1:18111",
  "input_dim": 64,
  "output_dim": 64
}
```

Layer entries declare the topological dependency (`depends_on`), the replica
window P95 threshold, and the per-call timeout. Service settings include the
120 ms total request budget and the maximum number of second-tier attempts.

Startup validation happens before any socket is opened. Errors are one-line,
sorted JSON on stderr and exit with status 2. Stable codes include:

| Code | Meaning |
| --- | --- |
| `CONFIG_FILE_NOT_FOUND` | Router/worker config is absent |
| `CONFIG_JSON_INVALID` | Config is not valid JSON |
| `CONFIG_SCHEMA_INVALID` | Required field/type/range is invalid |
| `CONFIG_DUPLICATE_LAYER` | Layer ID is repeated |
| `CONFIG_LAYER_UNKNOWN` | Dependency or shard references an unknown layer |
| `CONFIG_LAYER_CYCLE` | Layer dependency graph contains a cycle |
| `CONFIG_DUPLICATE_REPLICA` | Replica ID is repeated |
| `CONFIG_DUPLICATE_REPLICA_GROUP` | A group ID is reused across layers |
| `CONFIG_DUPLICATE_LISTEN` | Two replicas use the same address |
| `CONFIG_MODEL_MISSING` | Declared ONNX file does not exist |
| `CONFIG_MODEL_INVALID` | ONNX Runtime cannot load the file |
| `CONFIG_DIMENSION_MISMATCH` | Model, manifest, or adjacent tiers disagree |

Error payloads identify `replica_id` and the failing `field` where applicable.

## Runtime semantics

### Routing and health

- Requests choose one replica from each tier in topological order.
- Each replica keeps a bounded latency deque. Once the minimum sample count is
  reached, window P95 above the layer threshold evicts the replica.
- Active health probes run every 500 ms. Two consecutive failures evict a
  replica; two consecutive successes after eviction restore it.
- Eviction clears the latency deque, and recovery clears it again. Old samples
  are never used to justify routing after recovery.
- Selection excludes evicted replicas and uses round-robin order within the
  remaining set.

### Idempotency and persistence

- A new request is atomically inserted into SQLite as `in_progress` before any
  model call.
- Concurrent duplicates join the same in-process future. They never execute a
  second pipeline.
- A completed request returns its stored output forever. Reusing its ID does
  not add calls or rewrite the outcome.
- On startup, committed `in_progress` rows are replayed from their persisted
  input features before the router starts serving.
- A final commit writes the result, actual per-tier calls, retry switches, and
  request-triggered eviction events in one SQLite transaction. The update is
  guarded by `status = 'in_progress'`, so a final result cannot be overwritten.
- Probe-triggered eviction/recovery events have no request ID and are committed
  independently.

SQLite tables are `requests`, `request_calls`, and `events`. WAL mode and a
10-second busy timeout are enabled.

### Second-tier failover

The first tier is called exactly once. If the selected second-tier replica times
out or is unreachable, the router excludes it and tries another replica in the
same group while the 120 ms total budget still has room. Both attempts carry the
same request and session IDs, and only one second-tier output is adopted.

If all attempts fail, the response has no output and no tier replica IDs. It
returns a stable error code plus the actual attempted replica list. Relevant
codes are `L2_TIMEOUT`, `L2_CALL_FAILED`, `L2_ATTEMPTS_EXHAUSTED`,
`L2_BUDGET_EXHAUSTED`, and `L2_UNAVAILABLE`.

## Observability

Prometheus metric names and label keys are stable:

- `inference_layer_available_replicas{layer}`
- `inference_replica_calls_total{layer,replica}`
- `inference_replica_timeouts_total{layer,replica}`
- `inference_replica_errors_total{layer,replica}`
- `inference_replica_evictions_total{layer,replica}`
- `inference_replica_latency_seconds{layer,replica,quantile}`
- `inference_e2e_latency_seconds{quantile}`
- `inference_e2e_p99_seconds`
- `inference_requests_total{outcome}`
- `inference_retry_switches_total`

`/stats` computes P50/P95/P99 over the configured request window. If P99 exceeds
`stats.p99_threshold_ms`, it reports the tier and replica with the largest total
actual call latency contribution, plus actual call counts, timeouts, switches,
and eviction records. A tier with no actual call is omitted.

`/status` and `/v1/requests/{id}` use `json.dumps(..., sort_keys=True)`, sort
replica arrays by replica ID, and therefore have stable field ordering.

## Deterministic load and fault scenario

Run the complete validation:

```powershell
.\.venv\Scripts\python.exe scripts\run_scenario.py `
  --output reports\scenario.json
```

The script:

1. Restricts the process tree to two logical CPUs (when the platform permits).
2. Runs 120 requests/second with 64 fixed-seed features.
3. Injects 250 ms into `l2-a` during load, checks eviction and recovery.
4. Fails both second-tier replicas to verify a stable error with two L2 attempts
   and unchanged session IDs.
5. Verifies final idempotency and SQLite call-count stability.
6. Restarts the router with an `in_progress` row and checks replay.
7. Stops all second-tier workers and verifies `L2_UNAVAILABLE`.
8. Reports peak aggregate RSS, temporary disk use, and all assertions.

Standalone tools:

```powershell
# fixed-seed 120 RPS load
.\.venv\Scripts\python.exe scripts\load_test.py `
  --requests 1200 --rate 120 --seed 42 --output reports\load.json

# inject or clear processing delay for one replica
.\.venv\Scripts\python.exe scripts\fault_inject.py `
  --target 127.0.0.1:18111 --delay-ms 250
```

The last local run on two pinned CPUs produced:

- Normal load: 600/600 successful, P50 `2.9 ms`, P95 `7.2 ms`, P99 `13.3 ms`
- One delayed L2 replica: 960/960 successful, P99 `50.3 ms`, 46 retries
- Peak RSS across all processes: `511 MiB`
- Temporary disk delta: about `0.1 MiB`

These are local measurements, not a cross-machine guarantee.

## Tests

```powershell
.\.venv\Scripts\python.exe -m unittest discover -v
```

The suite covers configuration rejection codes, dimension checks, P95 eviction
and history reset, probe recovery, transaction idempotency, concurrent duplicate
merging, second-tier session-preserving failover, no-partial-output failures,
and statistics based only on actual calls.
