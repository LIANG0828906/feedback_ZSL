# Architecture and Invariants

## Snapshot Ownership

`PointCloud` is immutable. NumPy arrays are copied to contiguous native
layouts, marked read-only, and are never mutated after construction.
`PointCloudSnapshot` combines that cloud with a version number, content hash,
and parent version.

`AsyncEditController` owns the display snapshot:

```text
renderer -> controller.display_snapshot() -> immutable snapshot reference
worker   -> new arrays -> operator record -> repository.commit()
         -> single published-reference replacement
```

The worker never writes into the currently published arrays. New data is not
visible until the object and manifest commit has succeeded. Therefore a
renderer call before the replacement must observe the complete old snapshot, and
a call after it must observe the complete new snapshot. There is no window in
which some points or packets are from each version.

## Operator Purity

Operators accept a snapshot, a selection, normalized parameters, and optional
observation callbacks. They do not mutate inputs or persistent state.
Cancellation raises before returning a result. Repeated execution with the same
snapshot bytes and parameters produces identical output bytes.

Selection is a sorted unique index tuple plus:

- source content hash,
- source point count,
- selected bounds.

The hash binding prevents applying a selection made on version A to version B.
Changing or discarding a selection has no effect on the committed cloud.

## Commit Protocol

Object publication uses a staging file and a hard link in the same directory:

```text
objects/.v00000002.pcwb.<random>.tmp   write + fsync
objects/v00000002.pcwb                atomic no-replace rename
manifest.json                         atomic replace, fsync directory
```

The manifest is the commit point. An object without a manifest entry is an
orphan after a crash and is removed by recovery. A manifest can only reference
objects that were fsynced and linked before the manifest replacement.

`recover()` also validates the current manifest entry's SHA-256 against the
object bytes. The application does not silently splice a newer object into an
older manifest.

## Background Editing

Only one edit worker is active at a time. Progress consists of a phase and
fraction. Once elapsed time crosses the configured threshold, the workbench
shows phase, percentage, and elapsed milliseconds in the title and periodically
logs them. The render/input loop never joins the worker.

Cancellation is cooperative:

- the event is checked before expensive stages and periodically inside loops;
- a cancelled operation returns no operator result;
- no commit is attempted;
- the published snapshot and repository current version stay unchanged;
- local temporary arrays become unreachable and are reclaimed.

## Cache Semantics

Cache keys include source version, source content hash, operator, and canonical
parameters. A hit also verifies:

- source version is not newer than the requesting snapshot,
- source hash matches,
- payload SHA-256 matches the recorded output hash,
- parsed cloud hash matches the same output hash.

Rollback invalidates cache entries produced by later versions. Content-hash
validation prevents stale or corrupted payloads from being republished.

## Renderer Isolation

The renderer obtains one snapshot reference per frame. It uploads a new VBO only
when the snapshot content hash changes. The in-flight GL upload is a rendering
buffer only; it is not the committed source of truth. If the process dies, the
next startup reads the manifest and committed object.

The offscreen path uses the same OpenGL renderer with a hidden GLFW window. The
software fallback still executes the same display-loop contract: one
`display_snapshot()` read per frame, frame timings, and input-loop liveness.

## Determinism Boundary

Deterministic artifacts:

- generated PCWB samples,
- committed PCWB objects,
- manifests,
- operation trace,
- summary.

Nondeterministic observations:

- measured frame duration,
- input latency,
- worker duration and progress timing,
- host/OpenGL identification.

Nondeterministic values are written only to the runtime metrics sidecar. This
keeps reproducibility checks meaningful without falsifying measurements.
