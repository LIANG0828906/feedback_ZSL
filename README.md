# Inference Gateway (v3/v4 Canary, Single Host, Fully Offline)

Single-process FastAPI gateway that hosts two ONNX ranker models behind
isolated batching queues, routes traffic by tenant/client with adjustable
canary weights, validates feature protocols per request, and automatically
rolls the canary back when latency or error SLOs are breached.

Everything runs offline on one Windows 10/11 (or Server 2022) host with CPU
only. The same code runs unchanged on Linux/macOS for CI.

## Components

- `gateway/models.py` - per-version ONNX registry with `loading`/`ready`/`failed`
  state machine, isolated failures and runtime reload.
- `gateway/router.py` - stable-hash canary routing with tenant default weights,
  live weight changes, fallback and a per-client decision audit trail.
- `gateway/protocol.py` - feature count/order/type validation keyed on the
  client-declared protocol version; errors carry a field path.
- `gateway/pipeline.py` - one bounded queue, batcher loop and concurrency
  semaphore per model version; admission-controlled backpressure returns
  `QUEUE_FULL`/`QUEUE_WAIT_TIMEOUT` instead of blocking forever.
- `gateway/stats.py` - rolling-window per-version/per-tenant percentiles,
  error rates, timeouts and the canary circuit breaker.
- `gateway/metrics.py` - stable Prometheus metric names/labels on a separate
  metrics port (also mirrored at `/metrics`).
- `gateway/app.py` - FastAPI composition root; business failures always return
  HTTP 200 envelopes, never 503.
- `gateway/generator.py` - deterministic fixed-seed builder for both ONNX
  models, the protocol mapping and 40 sample requests (4 deliberately
  mis-ordered). Repeated runs produce byte-identical artifacts.
- `scripts/mixed_traffic_demo.py` - end-to-end acceptance driver.
- `tests/` - 38 pytest cases covering every acceptance criterion.

## Setup (Windows PowerShell)

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.runtime.txt
# Optional: regenerate the deterministic artifacts (needs requirements.txt)
.\.venv\Scripts\python -m gateway.generator --output generated
```

Linux/macOS:

```bash
python3.11 -m venv .venv
./.venv/bin/pip install -r requirements.runtime.txt
./.venv/bin/python -m gateway.generator --output generated
```

## Run

```bash
# Defaults: API 127.0.0.1:8000, metrics 127.0.0.1:9090, models in artifacts/models
python -m gateway.main --config config.example.json
# If 8000/9090 are taken:
GATEWAY_PORT=8010 METRICS_PORT=9091 python -m gateway.main --config config.example.json
```

## HTTP API

All business responses are HTTP 200. Error envelopes use stable codes
(`gateway/errors.py`), e.g. `FEATURE_COLUMN_ORDER_MISMATCH` with
`"field":"columns[1]"`.

- `POST /v1/rank` - score one request; body fields:
  `request_id`, `tenant_id`, `client_id` (optional), `protocol_version`,
  `columns`, `features`.
- `GET /v1/status` - JSON summary (sorted keys, relative model files only):
  model load states, protocol versions, queue stats, rolling stats and the
  latest circuit-breaker rollback record.
- `GET /v1/routing/trace?client_id=...` - per-client routing decisions.
- `POST /v1/admin/weight` - `{"tenant_id": null, "weight": 0.0..1.0,
  "reset_circuit_breaker": false}`; applies to subsequent requests immediately.
- `POST /v1/admin/reload` - `{"version": "v4"}` reloads a failed model.
- `POST /v1/admin/slow-calls` - `{"version": "v4", "count": N,
  "delay_ms": 200}` injects deterministic slow inference for SLO testing.
- `GET /metrics` and the standalone metrics port - Prometheus text format.

## Error Code Semantics

- `MODEL_LOADING` / `MODEL_LOAD_FAILED` - not-ready and failed versions.
- `READY_WAIT_TIMEOUT` - readiness wait exceeded; distinct from load failure.
- `QUEUE_FULL` - bounded admission capacity reached immediately.
- `QUEUE_WAIT_TIMEOUT` - enqueued but not served within the version timeout.
- `FEATURE_PROTOCOL_MISMATCH` - declared protocol differs from target model.
- `FEATURE_COUNT_MISMATCH` / `FEATURE_COLUMN_ORDER_MISMATCH` /
  `FEATURE_TYPE_MISMATCH` - validation failures with field paths; no inference
  is executed.

## Demo and Tests

```bash
python -m pytest tests/ -q
python scripts/mixed_traffic_demo.py --base-url http://127.0.0.1:8000
```

The demo replays the 40 samples, injects slow v4 inference and shows the P99
circuit-breaker trip, weight convergence to zero and uninterrupted v3
serving.

## Resource Footprint

Designed for 2 logical CPUs and 1.25 GiB RAM: one ONNX session per version,
CPU execution provider, default concurrency 1 per version and bounded queues.
No replicas, containers or GPUs are required.
