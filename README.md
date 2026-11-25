# thermalcache

`thermalcache` is a deterministic, local-first Go domain component for tiered
cache heat evaluation, migration planning, migration-time read/write gating,
SQLite checkpoints, crash recovery, and incremental invalidation.

The repository is self-contained:

- Go 1.22+ and the standard library only.
- SQLite is accessed through a small cgo binding to the system
  `libsqlite3.so.0`; there is no database service, driver download, or runtime
  network dependency.
- Fixed-seed inputs, three schema encodings, checkpoint recovery, and a
  repeatable demo are included.

## Quick Start

On Ubuntu/WSL:

```bash
sudo apt-get update && sudo apt-get install -y golang-go
go version

go test ./...
go run ./cmd/thermalcache -seed 42 -out artifacts
```

The current environment uses Go 1.22.12 and the system
`/usr/lib/x86_64-linux-gnu/libsqlite3.so.0`. The development headers are not
required because the cgo file declares only the SQLite C ABI it uses.

The demo writes:

- `artifacts/generated/v1.json`, `v2.json`, `v3.json`
- `artifacts/demo_output.json`
- `artifacts/verification.txt`
- `artifacts/checkpoint.sqlite`
- `artifacts/partial-checkpoint.sqlite`

The JSON result and verification text are deterministic for a fixed seed.
SQLite bytes are implementation-dependent, but the persisted logical state is
deterministic.

## Package Layout

| File | Responsibility |
| --- | --- |
| `model.go` | normalized project, nodes, objects, events, validation |
| `schema.go` | strict v1/v2/v3 loaders and unit/time normalization |
| `generator.go` | fixed-seed canonical fixture and three encoders |
| `scoring.go` | signed fixed-point score, overflow saturation, decisions |
| `engine.go` | logical timeline, state machine, layer evaluation |
| `migration.go` | batch selection, constraints, reservations, commit/rollback |
| `gate.go` | source-layer reads, pending writes, target visibility |
| `checkpoint.go` | SQLite schema, atomic writes, load and restore |
| `incremental.go` | prefix reuse, dependency-suffix recomputation, audit |
| `sqlite_cgo.go` | cgo ABI binding to system `libsqlite3.so.0` |
| `cmd/thermalcache` | local repeatable acceptance demo |

## Three Schemas

All three encodings normalize to `Project` and are canonicalized in the same
order: tier, rack, node id; objects by id; events by logical time and stable
tie-breakers. The normalized fingerprint excludes only the schema label, so it
is a direct semantic-equivalence check.

| Aspect | v1 | v2 | v3 |
| --- | --- | --- | --- |
| discriminator | `schema: "compact/v1"` | `schema_version: "normalized/v2"` | `schemaVersion: "racked/v3"` |
| field style | camelCase | snake_case | camelCase |
| time | ticks + `tickMs` | milliseconds | milliseconds |
| capacity | bytes | KiB | KiB |
| nodes | flat table | flat table | rack groups |
| objects | flat table | flat table | objects nested by tier |
| watermark | optional `heatWatermark` | optional `heat_watermark` | optional `heatWatermark` |

`LoadProject` rejects mixed/unknown fields with `E_SCHEMA_UNKNOWN_FIELD`; an
unrecognized discriminator returns `E_SCHEMA_UNSUPPORTED`.

See `artifacts/generated/` for complete examples.

## Heat Model

The score is a signed fixed-point integer. `ScoreConfig.Bits` defines the
declared signed width, `Scale` is the fixed-point scale, and no floating point
is used.

For each logical tick:

```text
accessTotal = indexed access counts through this step
              + matching access events
ageTicks    = floor((now - lastAccessMs) / tickMs)
raw         = accessTotal * countWeight * scale
score       = raw / (1 + ageTicks)
```

The result is saturated to `2^(Bits-1)-1`; `ScoreResult.Saturated` and
`ScoreResult.Overflowed` explicitly report clipping. TTL expiration is
`now-lastAccessMs > ttlMs`; expired objects score zero and request cold layer.

Threshold policy:

- Default `promote_inclusive`: cold promotes at `score >= threshold`; hot
  demotes at `score <= demoteThreshold`.
- `demote_inclusive`: cold promotes only at `score > threshold`; hot demotes at
  `score <= demoteThreshold`.
- If promote and demote thresholds are equal, a cold object promotes.
- An object-specific watermark, when present, replaces the promote threshold.

Each object tick emits the score, current/answer/desired layer, numeric
`ReasonCode`, stable reason id, threshold, saturation flags, and migration
state.

## Migration And Gating

Automatic migrations are selected from heat decisions under:

- target-layer capacity;
- `max_batch_objects`;
- `max_batch_bytes`.

Selection is deterministic: higher `priority` first; promotions break ties by
higher score, demotions by lower score; object id is the final tie-breaker.
Deferred objects carry a constraint code such as `C_BATCH_OBJECTS`,
`C_BATCH_BYTES`, `C_CAPACITY_HOT`, `C_CAPACITY_COLD`, or
`C_BUFFER_BACKPRESSURE`.

Staged bytes are tracked as reservations, not actual target usage. Actual
occupancy changes only at commit, so every object is charged to exactly one
layer. A batch result includes occupancy before/after and remaining batch
object/byte capacity.

During migration:

- reads answer from the source layer at the committed source version;
- writes are accepted at source, increment the pending-write count, and remain
  invisible at target;
- an explicit target-tier request is rejected with
  `E_GATE_TARGET_NOT_VISIBLE`;
- commit applies pending writes by advancing target version to source version
  and only then switches the answer layer;
- interruption before a commit rolls back all uncommitted staged objects and
  releases reservations;
- a partial commit is durable: committed objects stay at target, remaining
  objects resume from source.

## Checkpoints

`Engine.Checkpoint(path)` writes one SQLite transaction in this semantic order:

1. object and layer state;
2. migration batch and partial-commit state;
3. runtime capacity state;
4. global committed tick and cursors last.

`checkpoint_write_log` records these sections so the ordering is verifiable.
`LoadCheckpoint` verifies the SQLite header, `PRAGMA quick_check`, metadata, and
row completeness. Truncation returns `E_CHECKPOINT_TRUNCATED`; schema or
fingerprint mismatch returns `E_CHECKPOINT_VERSION_MISMATCH`.

`NewEngineFromCheckpoint` is the hard-kill recovery path. It uses only persisted
rows. Pre-commit batches with no committed object roll back; partial batches
continue from their last commit point.

## Incremental Invalidation

`Simulator.Apply` clones and edits the project, then:

- finds the earliest logical dependency (`AtMs`);
- reuses cached ticks strictly before that point;
- restores the in-memory state snapshot at the cutoff;
- recomputes only the dependent suffix and downstream batches;
- emits the edit source, reused/recomputed ticks, affected objects, tiers,
  batch ids, and closure;
- runs a separate clean full recomputation and sets `FullMatch` as an audit.

Edits cover object access counts/watermark/last-access, layer capacity,
appended access events, and removed access events.

## Concurrency And Backpressure

Layer evaluation runs under `workerBudget`, bounded by `maxWorkers`. Results
are invariant to worker count and goroutine scheduling.

Each layer has `perLayerBuffer`; exceeding it produces an observable
`E_MIGRATION_BACKPRESSURE` entry and deferred objects instead of dropping or
unbounded allocation. Per-layer evaluation timing is recorded in
`LayerMetric.DurationNs` for diagnostics only.

## Stable Business Codes

| Code | Meaning |
| --- | --- |
| `E_SCHEMA_UNSUPPORTED` | unknown schema discriminator |
| `E_SCHEMA_INVALID` | malformed/incomplete schema |
| `E_SCHEMA_UNKNOWN_FIELD` | unknown field or ambiguous version combination |
| `E_DUPLICATE_OBJECT` | duplicate object id |
| `E_DUPLICATE_NODE` | duplicate node id |
| `E_CAPACITY_NEGATIVE` | negative byte/KiB capacity |
| `E_CAPACITY_OVERFLOW` | normalization overflow |
| `E_BATCH_LIMIT_ZERO` | zero/negative batch limit |
| `E_MIGRATION_TARGET_CAPACITY` | missing/deleted/insufficient target capacity |
| `E_MIGRATION_SOURCE` | source layer mismatch |
| `E_MIGRATION_BACKPRESSURE` | migration bytes/objects/buffer pressure |
| `E_THREAD_BUDGET` | worker budget outside `[1,maxWorkers]` |
| `E_BUFFER_BUDGET` | invalid per-layer buffer |
| `E_GATE_TARGET_NOT_VISIBLE` | target access before commit |
| `E_GATE_WRONG_LAYER` | request for another committed layer |
| `E_CHECKPOINT_TRUNCATED` | truncated/corrupt checkpoint |
| `E_CHECKPOINT_VERSION_MISMATCH` | schema/fingerprint mismatch |
| `E_CHECKPOINT_PROJECT_MISMATCH` | different project identity |
| `E_OBJECT_NOT_FOUND` | event/edit references missing object |
| `E_NODE_NOT_FOUND` | event/edit references missing node |

All domain errors are `*BizError` values with sorted, locatable context.

## Public API Sketch

```go
fixture, _ := thermalcache.GenerateFixture(42)
project, _ := thermalcache.LoadProject(fixture.Documents[thermalcache.SchemaV3])

engine, _ := thermalcache.NewEngine(project)
result, _ := engine.Run()

_ = engine.Checkpoint("cache.sqlite")
recovered, _ := thermalcache.NewEngineFromCheckpoint(project, "cache.sqlite")

sim, _ := thermalcache.NewSimulator(project)
_, _ = sim.Run()
incremental, _ := sim.Apply(thermalcache.Edit{
    Kind: thermalcache.EditObjectAccess,
    ObjectID: "o03",
    AtMs: 200,
    Counts: []int64{2, 0, 0, 0, 0, 3, 0},
})
```

## Verification

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go run ./cmd/thermalcache -seed 42 -out artifacts
```

Recorded result on 2026-11-26:

- 17 top-level tests passed, including 9 injected anomaly subtests and 4
  incremental-edit subtests.
- v1/v2/v3 fingerprints all
  `23170f32ca4500abb3304d34074dc80e7ee9438671b5be5944e51627c160c42f`.
- Each schema produced 7 ticks and 8 batches with identical normalized results.
- Fixed-seed normalized result digest:
  `39293d8436900a8fc2cfda02ffb4d8d1a10620f709a9d3665ca47ab7f9951495`.
- 42 seed timeline: `[0 100 200 300 400 500 600]`.
- Migration batches stayed within limits; hot+cold actual occupancy charged
  every object once.
- Object `o05` read from cold and accepted one pending write at tick 100, then
  switched to hot at tick 200 with source/target version 1.
- Object `o10` was force-migrated at tick 500 behind the gate and committed at
  tick 600.
- Incremental object/capacity/append-event edits all matched full recompute,
  reusing 2, 3, and 4 prefix ticks respectively.
- Partial checkpoint recovery and interrupt rollback preserved source content
  with no double counting.

Detailed output is in `artifacts/verification.txt` and
`artifacts/demo_output.json`.
