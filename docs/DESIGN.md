# Design Notes

## Determinism

The generator uses a local SplitMix64 stream seeded by `--seed`; it never reads
the clock or network. Geometry is serialized as millimetres. All solver state,
path costs, decay times and energies use Q16.16 integers. The integer square
root is Newton iteration over `uint64`, and ties are resolved lexicographically
by room and portal sequence. Generator truth uses a separate simple-path
enumeration and acoustic formula implementation rather than calling the
production Dijkstra solver.

The same sample seed therefore yields byte-identical canonical JSON, commit
order, room chains and result numbers on independent runs.

## Room Ownership

A listener belongs to the room whose rectangle contains it. A room result owns
all `(source, listener)` pairs for listeners physically inside that room. This
partition makes a committed room self-contained while still allowing paths to
cross other rooms.

An active portal contributes an undirected edge between the two rooms recorded
on its wall. The shortest path search is not plain room Dijkstra: each state
also stores the entry portal, because the distance to the next portal depends on
the current point inside the room.

## Invalidation

The dependency graph is rebuilt from committed path results:

- every visited room depends on the listener result;
- every traversed portal depends on the listener result;
- every listener maps to its owning room result.

Parameter edits probe listener rooms that previously used the edited room.
Portal and boundary edits probe the affected connected component. A candidate
room is marked invalid only when its canonical result changed. Unaffected rooms
keep their frozen state and are written to the room cache under the new edit
version.

## Snapshot Invariants

For a resumable run:

1. Run metadata binds the whole level hash, parameter version, algorithm version
   and precision.
2. Every snapshot binds a contiguous `seq`, room ID, input hash and payload
   content hash.
3. `SaveRoom` derives `seq` from the current maximum inside the same mutex and
   transaction that inserts the row. The engine only asks for commits in sorted
   room order.
4. Recovery validates all rows before using any row. A single bad hash, gap,
   duplicate room or version mismatch deletes the complete run snapshot and
   starts from an empty run.
5. A cancel or simulated hard stop can leave only committed transactions. The
   next run recomputes all rooms after the last contiguous snapshot.

`room_cache` is separate from resumable run snapshots. It is also versioned and
content-hashed, and is used only for already-computed room results whose input
projection still matches.

## Concurrency

- `MaxParallel` bounds room workers.
- The job channel is deliberately bounded; producer blocking is counted in
  `queue_blocked`.
- `GraphCacheBudget` is a semaphore around graph-cache ownership. A worker that
  cannot acquire immediately records `graph_cache_budget_waits` and blocks.
- `PathBufferLimit` caps rooms in one search path and returns `E_RESOURCE` if
  exceeded.
- All SQLite writes share one connection and one mutex, and each room commit is a
  transaction.

The output map is assembled independent of worker completion order, so changing
parallelism cannot change any compared field.

## Three-Path Equality

`verify.ThreeWay` executes:

1. Full recompute of the level after all serialized edits.
2. Live preview from the base level, applying the edits one by one and reusing
   unchanged room results.
3. A checkpoint run intentionally interrupted after two commits, followed by a
   fresh SQLite open and resume.

The comparison checks commit order, progress, aggregate listener stats, room
parameters, incident portal snapshots, neighbor/portal sets, every source pair,
room chain, portal chain, fixed path, fixed decay and fixed energy. It returns
the first difference as `E_DIFF` with room/listener/source/field metadata.

## HTTP Workbench

The server embeds HTML, CSS and JavaScript, so the UI does not access remote
assets. All mutations take the server mutex and update the same in-memory
session. The UI uses a Canvas for the plan and charts; room edge drags become
the same `move_room_edge` operation as the numeric controls.
