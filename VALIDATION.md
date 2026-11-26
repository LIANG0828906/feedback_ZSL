# Actual validation record

Recorded in the working container on 2026-11-26.

## Environment

```text
go version go1.22.12 linux/amd64
Linux/WSL Ubuntu environment
module: quotakernel (no third-party dependencies)
```

## Static checks and unit/acceptance tests

Commands:

```bash
gofmt -w .
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

Actual result:

```text
gofmt: clean
go vet: passed
ok  quotakernel          (CLI SIGKILL/SIGINT + semantic scenario tests)
ok  quotakernel/fixed
go test -race: passed for all packages
```

The root package tests include:

- generated-sample end-to-end acceptance;
- real `go build` of `cmd/quotaaccept`;
- real SIGKILL at the entity-before-pointer crash window;
- restart/recovery and truth comparison;
- graceful SIGINT exit report;
- bidirectional reserve/settlement delta;
- guarantee encroacher attribution;
- finite incremental closure convergence.

## Deterministic sample verification

Commands:

```bash
go run ./cmd/quotaaccept generate --seed 42 --dir samples/seed-42
go run ./cmd/quotaaccept verify \
  --dir samples/seed-42 \
  --report artifacts/acceptance-report.json \
  --checkpoint artifacts/checkpoint-verify
```

Actual result after the final sample regeneration:

```text
report=artifacts/acceptance-report.json checks=19/19 ok=true
```

Checks:

```text
PASS schema-equivalence:cluster-main
PASS truth-settlement:cluster-main
PASS order-independence:cluster-main
PASS schema-equivalence:burst-contention
PASS truth-settlement:burst-contention
PASS order-independence:burst-contention
PASS schema-equivalence:gate-reconcile
PASS truth-settlement:gate-reconcile
PASS order-independence:gate-reconcile
PASS optional-burst-degradation
PASS fixed-point-critical-cases
PASS gate-latency-and-determinism
PASS concurrent-ledger-hashes
PASS concurrent-event-submission
PASS worker-backpressure
PASS tenant-queue-backpressure
PASS single-node-occupancy
PASS incremental-invalidation
PASS checkpoint-recovery
```

The machine-readable report includes conversion diagnostics, per-tenant gate
latency observations, incremental propagation proofs, and recovery details.

## Incremental evidence

From `artifacts/acceptance-report.json` during the final full verification:

```text
weight_event:evt-08-bravo-weight-up  recomputed ticks 4..15; reused ticks 1..3
tenant_quota:t-charlie @ tick 5      recomputed ticks 5..15; reused ticks 1..4
node_capacity:node-b @ tick 5        recomputed ticks 5..15; reused ticks 1..4
```

Each edited incremental result is compared item-by-item with a fresh full
recomputation. Persistent declaration changes remain in snapshots through the
horizon; the proof marks the closure as horizon-bounded rather than silently
claiming convergence. A same-value touch test verifies exact hash convergence at
tick 4 and cache reuse from tick 5 onward.

## Forced-kill recovery

Commands:

```bash
go build -o artifacts/bin/quotaaccept ./cmd/quotaaccept
./artifacts/bin/quotaaccept run \
  --seed 42 \
  --checkpoint artifacts/crash-demo-final \
  --kill-at-tick 4

./artifacts/bin/quotaaccept recover \
  --dir samples/seed-42 \
  --checkpoint artifacts/crash-demo-final \
  --report artifacts/final-recovery.json
```

Actual result:

```text
CRASH_EXIT=137
crash hook: entity state for tick 4 fsynced; sending SIGKILL before committed pointer
POINTER_TICK=3
recovery ok mode=version-matched-checkpoint committed_tick=3 recomputed=12
RECOVERY_MODE=version-matched-checkpoint TRUTH_COMPARED=12
```

The orphan `ticks/000000000004.entities.json` file is intentionally present but
ignored because `committed.json` still names tick 3.

## Backpressure and diagnostics

- Worker exhaustion returns `BP_WORKER_BUDGET_EXHAUSTED`; the acceptance check
  occupies the only worker and its one buffered slot, then verifies the next
  submission is rejected and counted.
- Queue exhaustion returns `BP_TENANT_QUEUE_FULL` with
  `C-QUEUE-BOUND`.
- Gate runs compare worker degrees 1, 2, 4, and 8. Actual per-tenant timing
  observations are in `artifacts/acceptance-report.json`; timing values are not
  used for ledger, admission, queue, or gate-record equality.

## Reproduction notes

- No Docker, external service, SQLite driver download, or network access is
  required for `go test` or the CLI after source is present.
- `artifacts/bin/quotaaccept` is a local demonstration binary and can be rebuilt
  or deleted.
- Sample JSON and truth files are deterministic under seed `42`.
