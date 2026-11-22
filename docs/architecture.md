# Architecture

## Pipeline

```text
scene JSON
  -> validation (schema/range/ref)
  -> per-member A* (fixed-point cost + admissible heuristic)
  -> formation-tree smoothing (line of sight + slot tolerance + spacing)
  -> waypoint extraction (grid + world readings)
  -> per-tick replay (step + turn-rate + formation deviation)
  -> path/simulation caches + dependency graph
  -> atomic checkpoint
```

The pure-Python core has no Qt dependency. The PySide6 layer is a diagnostic client over
the same engine used by CLI and tests.

## Fixed-point contract

- `COORD_SCALE = 1000`: one grid cell is 1000 world units.
- `ANGLE_SCALE = 1000`: one degree is 1000 millidegrees.
- Cell costs are integers in `[-1, 1_000_000]`.
- A* cost and f-score are checked before addition against `2^63-1`.
- Positions are integer world coordinates; every interpolation is integer arithmetic.
- A* reverse-distance verification uses the exact same movement graph as the forward
  search. An overestimate reports the member id, first violating cell, `h`, and exact
  remaining cost.

## Formation and dependency model

Slots define each member offset relative to its parent's slot. The relation is a tree:

- root/leader has no formation offset dependency;
- each child samples its own and its parent's path at the same normalized progress;
- expected child position is `parent_position + slot[child] - slot[parent]`;
- slot deviation and parent spacing are recorded for every tick.

Invalidating member `m` for a path edit computes `closure({m})`, i.e. `m` plus all
descendants in the formation tree. Smoothing and simulation are recomputed in tree
pre-order. Ancestors and unrelated branches retain their path and simulation intervals.

Path dependencies contain:

- exact cells on the A* path;
- terrain classes traversed;
- active blockade ids traversed;
- all cell coordinates examined by the search (including blocked frontier neighbors);
- source map content hash.

Edit policy:

- cost increase / blockade activation: invalidate members whose actual path (raw or
  smoothed) touches the changed cells;
- cost decrease / cell opening / blockade removal: additionally invalidate members whose
  own A* examined the changed cell, because a newly cheaper or newly passable route can
  change the optimum;
- blockade effect ids are retained across activation cycles so reopening restores the
  previously affected candidate set.

This remains bounded by the affected member's search scope and never expands to a whole
map or every formation member without a dependency path.

## Simulation intervals

Each member trace is split into fixed tick intervals. `dependency_graph_payload()` emits
explicit edges:

```text
parent member interval -> child member interval (formation parent/child coupling)
```

On incremental replay, only intervals of dirty closure members are replaced. Reused
intervals keep their persisted digest; checkpoint load recomputes each interval digest
and rejects the whole snapshot on mismatch.

## Atomicity and concurrency

- `BoundedExecutor` caps workers and in-flight submissions at
  `max_workers * per_worker_buffer`.
- Results are consumed in stable member-id order.
- A unit exception is returned as an outcome and marks only that unit failed.
- Cache replacement occurs under one re-entrant engine lock after all candidate objects
  are computed.
- Checkpoint writes take a process thread lock and an OS file lock, write a same-directory
  temporary file, `fsync`, then `os.replace`.
- Snapshot member order is always sorted and stored explicitly as `commit_order`.

## Snapshot validity

A snapshot is usable only when all of these match exactly:

- checkpoint format and scene schema;
- current map content hash;
- full scene content hash;
- formation id/hash;
- search config hash;
- simulation config hash;
- fixed-point version and algorithm version;
- complete member set and stable commit order.

Any mismatch means none of the old caches are consumed; `load_or_rebuild()` performs the
full pipeline.
