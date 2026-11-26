# Design

## Packages

| Package | Responsibility |
| --- | --- |
| `registry` | Domain model, three-version loader, lease/probe engine, catalog, incremental edits. |
| `samplegen` | Fixed-seed project and truth generation for v1/v2/v3. |
| `checkpoint` | Snapshot/step records and SQLite checkpoint compatibility decisions. |
| `internal/sqlitefile` | Minimal pure-Go SQLite 3 writer/reader for rowid tables. |
| `anomaly` | Repeatable anomaly injection suite. |
| `cmd/registryctl` | Local diagnostic CLI. |

## Deterministic engine

Logical time advances one tick at a time. A tick executes in this order:

1. Settle leases whose expiry is at or before the current logical time.
2. Apply events in declaration order.
3. Complete probe timeouts.
4. Dispatch deferred probes, then newly due probes, under all budgets.
5. Capture instance states and all service catalogs.

Durations are normalized to integer milliseconds. Lease remaining is
`max(0, expires_at - now)`. A heartbeat at the exact expiry is rejected because
lease settlement runs first.

Probes are never reissued while one is in flight. A due probe advances the
instance's next due time even when suppressed. Budget checks use:

- `C_GLOBAL_CONCURRENCY`
- `C_NODE_CONCURRENCY`
- `C_RACK_CONCURRENCY`
- `C_INSTANCE_BUFFER`

Deferred probe order is the monotonically increasing queue sequence. A probe
whose delay exceeds `max_delay_ms` is dropped with `E_PROBE_MAX_DELAY`.

The scheduler is deterministic by construction. There is no OS-thread
scheduling input to probe sets, state transitions or catalog order. Worker
count and submission batches therefore cannot reorder domain results.

## Incremental edits

Configuration edits after tick zero are represented as `ConfigOverride`
records with an effective tick. A full recompute of the edited project and an
incremental recompute therefore share the exact same pre-edit history.

`IncrementalRecompute`:

1. validates that the edit does not touch committed history;
2. clones and validates the edited project;
3. restores the cached snapshot immediately before the first affected tick;
4. if an appended event starts after the cached horizon, extends the old
   project to the new boundary first;
5. recomputes only the suffix;
6. reuses the cached prefix;
7. attaches `Invalidation` source, reason, closure, reused/extended count and
   recomputed count to every recomputed step.

Tests compare the incremental suffix with a full edited-project recomputation.

## Checkpoint file

The checkpoint is a real SQLite 3 file with these rowid tables:

- `instance_state`
- `deferred_queue`
- `inflight_probe`
- `committed_steps`
- `checkpoint_meta` (written last)

Each instance and queue item is a separate row. `checkpoint_meta` has
`complete = 1` and a SHA-256 checksum over all row payloads and version/global
fields.

Commit protocol:

1. encode the complete database in memory;
2. decode it once to validate the B-tree;
3. write a temp file in the destination directory;
4. `fsync` the temp file;
5. atomically rename over the checkpoint path;
6. `fsync` the directory.

A kill before rename leaves the previous checkpoint unchanged. A kill after
rename leaves the new complete checkpoint. Recovery verifies metadata,
checksum, commit count, next tick, last step and version identity before
accepting the file.

Version mismatch is not treated as corruption: the recovery decision is
`full_recompute` with `E_CHECKPOINT_VERSION_MISMATCH`. Truncation and checksum
failure are errors `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT`.

## SQLite subset

The writer emits standard SQLite format-3 pages with:

- 4096-byte pages;
- utf-8 text encoding;
- table leaf/interior B-trees;
- local payload and overflow-page chains;
- record serial types for integers, text and blobs.

It intentionally does not implement updates, indexes, WAL, pointer maps or
free pages. The checkpoint layer only needs immutable rowid tables. Generated
files pass SQLite's independent `PRAGMA integrity_check`.

## Failure handling

All domain failures are `registry.Error` values with a stable `E_xxx` code and
context (`service_id`, `instance_id`, tick/time, path, constraint or checkpoint
path). Loader validation works against a clone; anomaly tests hash the source
before and after injection and reload the baseline catalog.

The CLI maps business failures to exit code 2 and prints a JSON envelope.
Graceful SIGINT/SIGTERM commits the step already in progress through the
normal checkpoint path, then exits 130/143. SIGKILL cannot produce a report;
the next process only accepts the last atomically committed checkpoint.
