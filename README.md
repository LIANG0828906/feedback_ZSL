# Service Registry and Health Probe Component

A dependency-free Go 1.22 domain component for service registration, lease
settlement, health probing, catalog selection, incremental recomputation and
crash-safe SQLite checkpoints. It also ships a deterministic sample generator
and a local diagnostic CLI.

## Quick start

```sh
go test ./...
go vet ./...

go run ./cmd/registryctl gen --out samples --seed 42
go run ./cmd/registryctl compare --samples samples --seed 42
go run ./cmd/registryctl verify --seed 42

go run ./cmd/registryctl run \
  --project samples/v1.json \
  --checkpoint tmp/checkpoint.db \
  --report tmp/run.json
```

`samples/v1.json`, `samples/v2.json` and `samples/v3.json` describe the same
project in three schemas. Each contains the same per-tick truth report.

## What is implemented

- Three strict schema loaders:
  - v1: flat instance rows, ticks, absolute thresholds;
  - v2: nested service instances, milliseconds, remaining thresholds;
  - v3: composite seconds+millis, optional lease watermark.
- Canonical normalization of field names, instance order, duration units,
  threshold semantics and optional watermark validation.
- Lease registration, heartbeat renewal, remaining-time settlement, expiry and
  removal-pending state.
- Deterministic probe scheduling with per-instance in-flight suppression,
  timeout failure, absolute/remaining threshold equivalence and the
  `initial`/`healthy`/`unhealthy`/`half_open` state machine.
- Global, node, rack and per-instance queue budget enforcement; deferred queue
  sequence and max-delay drop.
- Catalog candidates, exclusions and stable ordering independent of display
  weight scale.
- Incremental edit/recompute with committed-history protection, cached prefix
  reuse, invalidation source and propagation closure.
- Real SQLite 3 checkpoints implemented with only the Go standard library.
- Stable `E_xxx` errors and a repeatable anomaly injection suite.

## Package map

| Path | Purpose |
| --- | --- |
| `registry/` | Integrable domain API: loader, engine, catalog, incremental edits. |
| `samplegen/` | Fixed-seed deterministic v1/v2/v3 sample and truth generator. |
| `checkpoint/` | SQLite snapshot persistence and resume compatibility. |
| `internal/sqlitefile/` | Minimal pure-Go SQLite 3 file writer/reader. |
| `anomaly/` | Eleven-case anomaly injection suite. |
| `cmd/registryctl/` | CLI diagnostics. |
| `samples/` | Checked-in deterministic samples with embedded truth. |

## Domain API

```go
document, err := registry.LoadDocumentFile("samples/v1.json")
run, err := registry.RunProject(document.Project, registry.RunOptions{})
truth := run.Truth()
```

Incremental edit:

```go
previous, snapshots, err := registry.ComputeProject(project, nil, true)
edited, err := registry.IncrementalRecompute(
    previous,
    snapshots,
    project,
    registry.Edit{
        Kind: registry.EditProbePeriod,
        InstanceID: "api-a",
        EffectiveTick: 10,
        Value: 200,
    },
    9, // committed through tick 9
)
```

Checkpoint:

```go
store := &checkpoint.Store{Path: "tmp/checkpoint.db"}
err = store.Save(&checkpoint.Checkpoint{Snapshot: snapshot, Steps: committedSteps})
```

## CLI

```text
registryctl gen        --out samples [--seed 42]
registryctl run        --project FILE [--checkpoint FILE] [--until TICK]
                       [--no-resume] [--report FILE] [--step-delay-ms N]
                       [--crash-after-commit TICK]
registryctl compare    [--samples DIR] [--seed 42]
registryctl incremental --project FILE --instance ID --field FIELD ...
registryctl anomaly    [--seed 42] [--checkpoint FILE]
registryctl checkpoint dump --path FILE
registryctl verify     [--seed 42]
```

Business errors are JSON envelopes with stable `E_xxx` codes. Exit codes are
`0` success, `1` internal, `2` business error, `64` usage, and `130`/`143` for
graceful SIGINT/SIGTERM after committing the last completed tick.

## Error codes

Representative stable codes:

| Code | Meaning |
| --- | --- |
| `E_SCHEMA_UNKNOWN` | Missing/unknown schema version. |
| `E_SCHEMA_FIELD` | Unknown or incompatible field combination. |
| `E_SCHEMA_LOCATION` | Malformed/truncated JSON. |
| `E_DUPLICATE_SERVICE` / `E_DUPLICATE_INSTANCE` | Duplicate identifiers. |
| `E_INVALID_DURATION` | Invalid lease/probe/tick duration. |
| `E_INVALID_HEARTBEAT` | Invalid heartbeat interval. |
| `E_INVALID_THRESHOLD` | Invalid health threshold. |
| `E_THRESHOLD_AMBIGUOUS` | Threshold mode cannot be determined. |
| `E_LEASE_UNIT_AMBIGUOUS` | Legacy lease value needs a watermark. |
| `E_LEASE_WATERMARK_CONFLICT` | Watermark conflicts with normalized TTL. |
| `E_INVALID_BUDGET` | Invalid concurrency/buffer/delay budget. |
| `E_EDIT_COMMITTED_HISTORY` | Edit would change committed history. |
| `E_PROBE_MAX_DELAY` | Deferred probe exceeded max delay. |
| `E_PROBE_BUFFER_FULL` | Per-instance event buffer is full. |
| `E_CHECKPOINT_TRUNCATED` | Checkpoint file is incomplete. |
| `E_CHECKPOINT_CORRUPT` | SQLite or checksum validation failed. |
| `E_CHECKPOINT_VERSION_MISMATCH` | Checkpoint requires full recompute. |
| `E_VERIFY_FAILED` | A diagnostic comparison found a mismatch. |

Every error carries `service_id`, `instance_id`, tick/time, JSON path,
constraint or checkpoint path as applicable.

## More detail

- [Schema versions and normalization](docs/SCHEMA.md)
- [Engine, checkpoint and failure design](docs/DESIGN.md)
- [Actual verification results](VERIFICATION.md)
