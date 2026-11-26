# Multi-tenant quota ledger and admission kernel

A deterministic, dependency-free Go kernel for the quota-ledger, bounded-latency
gate, burst contention, incremental invalidation, concurrency/backpressure, and
crash-recovery acceptance chain.

The module is `quotakernel` and targets Go 1.22+. It uses only the Go standard
library. The local checkpoint store is the task's allowed file-backed
SQLite-equivalent: entity state is fsynced first and the global committed-tick
pointer is atomically renamed second.

## Quick verification

```bash
go version
gofmt -w .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

go run ./cmd/quotaaccept all \
  --seed 42 \
  --dir samples/seed-42 \
  --report artifacts/acceptance-report.json
```

Or run generation and verification separately:

```bash
go run ./cmd/quotaaccept generate --seed 42 --dir samples/seed-42
go run ./cmd/quotaaccept verify \
  --dir samples/seed-42 \
  --report artifacts/acceptance-report.json \
  --checkpoint artifacts/checkpoint-verify
```

The report contains 19 top-level checks covering all eight requirement groups.
Generated JSON samples and truth files are under `samples/seed-42/`.

## Forced-kill recovery demo

```bash
go build -o artifacts/bin/quotaaccept ./cmd/quotaaccept

# The child exits with 137 (SIGKILL) after tick 4 entities are durable but
# before committed.json advances from tick 3.
./artifacts/bin/quotaaccept run \
  --seed 42 \
  --checkpoint artifacts/crash-demo \
  --kill-at-tick 4
echo $?

./artifacts/bin/quotaaccept recover \
  --dir samples/seed-42 \
  --checkpoint artifacts/crash-demo \
  --report artifacts/crash-recovery.json
```

A real CLI subprocess test performs this SIGKILL flow automatically. Graceful
`SIGINT`/`SIGTERM` handling is also tested:

```bash
./artifacts/bin/quotaaccept run --tick-delay 5s \
  --checkpoint artifacts/signal-demo \
  --exit-report artifacts/exit-report.json
# send SIGINT or SIGTERM; the process stops at the tick boundary and writes
# artifacts/exit-report.json.
```

## Package map

- `model`: normalized project, event, ledger, queue, gate, report, snapshot,
  constraint and business-code types.
- `fixed`: 64-bit decimal fixed-point/PPM conversion, checked multiplication,
  saturation, overflow flags and tolerance checks.
- `schema`: `v1`, `v2`, and `v3` DTOs and loaders.
- `generate`: fixed-seed deterministic projects, three-schema projections,
  critical conversion cases and reference truth.
- `engine`: order-independent per-tick ledger settlement, priority classes,
  guarantee protection, queues, attribution and invariants.
- `gate`: O(1) fast admission decision, tenant-owned safe balance check and
  deterministic latency-budget degradation.
- `incremental`: dependency ticks, cache manager, propagation closure and
  incremental/full report equality.
- `runtime`: bounded worker pool, stable backpressure codes and per-tenant
  latency observations.
- `checkpoint`: atomic file-backed durable snapshots, checksums, version
  matching and crash-window hooks.
- `sampleio`: deterministic sample/truth JSON IO.
- `accept`: end-to-end acceptance report.
- `cmd/quotaaccept`: `generate`, `verify`, `all`, `run`, and `recover` commands.

## Declared rules

- Rate scale: parts per million, exactly representing per-mille and
  per-ten-thousand inputs.
- Capacity scale: bytes; KiB conversion uses `1024`.
- Settlement tolerance: one byte for generated integer cases.
- Gate budget: 10 ms per decision in the sample rules; tests also force an
  over-budget fake clock and assert deterministic downgrade to `QUEUE`.
- Queue bound: eight items per tenant by default; a one-item fixture verifies
  `BP_TENANT_QUEUE_FULL`.
- Worker budget: four by default; a one-worker/one-slot test verifies
  `BP_WORKER_BUDGET_EXHAUSTED`.
- Wait TTL: five ticks for generated tenants.

See `TECHNICAL_NOTES.md` for semantics and `VALIDATION.md` for recorded local
verification results.
