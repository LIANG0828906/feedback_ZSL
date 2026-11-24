# Design Notes

## 1. Deterministic unit of work

The simulation has one canonical work unit: one logical tick.

```text
apply intents at tick T (atomic batch)
  -> produce/consume over [T, T+1)
  -> truncate at capacity
  -> record overflow
  -> tick = T + 1
```

Every path (`RunFull`, `RunWindowed`, `RunPreview`) drives this same `Engine.Step` implementation. A checkpoint is a serialized state at a committed tick boundary plus the intent offset. No path owns a separate economy formula.

## 2. Fixed point

`AmountScale = 1000`.

- A rate of `2500` means `2.500` resource units per tick.
- A capacity of `120000` means `120.000` resource units.
- Net rate can be negative but inputs/outputs/capacities/stocks are non-negative.
- Values are capped at `2^52` in loaded samples to keep all validated arithmetic safely inside `int64`.

JSON contains integers only. No float participates in settlement.

## 3. Flow and ordering

Nodes form a forest through `parent_id`. `TopoOrder` returns parents before children with ID tie-breaking.

For each active node:

1. Check worker requirement.
2. For every input resource, build an allocation plan over the node's own stock and then its parent chain.
3. If any resource is short, consume nothing and mark the node stalled.
4. Commit allocations.
5. Add outputs, clip at capacity, and record overflow per node/resource.

This gives deterministic upstream-to-downstream flow within a tick while keeping input failure atomic.

## 4. Upgrade atomicity

An upgrade intent names exactly `current_level + 1`. The engine resolves one `Upgrade` record and replaces:

- level
- outputs
- inputs
- capacity
- workers required
- recipe override flag

as one assignment on a cloned step state. Any validation/runtime error restores the entire step backup, including tick, intent offset, stocks, stalls and overflow counters.

If an upgrade or recipe lowers capacity below current stock, the same atomic switch truncates stock to the new capacity and adds the excess to the node/resource overflow ledger. This keeps `stock <= capacity` true immediately after the switch.

## 5. Preview consistency

Two independent approximate integrations can diverge when capacity clipping or input starvation happens inside a coarse interval. To keep the business requirement "final state per sample exactly equal" without exception, preview is:

- coarse in **publishing**: user-facing curve points at `step` intervals;
- exact in **integration**: internally the same one-tick state machine.

This also means preview can record sample snapshots exactly at business sampling ticks and compare field-by-field with exact settlement. The preview result carries `step` and `cutoff_tick`, so the display is traceable even though its integration unit is canonical.

## 6. Windows and checkpoint state

A checkpoint is not a report. It contains only what is necessary to continue:

- complete node runtime state
- tick and next intent offset
- cumulative overflow counters and last-overflow ticks
- content/parameter/algorithm versions
- state hash for corruption detection

Global totals are derived from node state, so they cannot be partially merged from old and new windows.

SQLite uses:

- one connection (`MaxOpenConns(1)`)
- process-level writer mutex
- one transaction per window
- `journal_mode=WAL`, `synchronous=FULL`
- `(scenario_id, window_index)` primary key

The writer checks the previous window's `end_tick` and `end_intent_offset` before insert. A duplicate/out-of-order writer is rejected with `E_CHECKPOINT`.

On load, all rows are scanned. Any version/hash/prefix mismatch deletes the complete scenario checkpoint set in a transaction. The following run starts from tick 0. A partially mixed prefix is never returned.

## 7. Failure and cancellation

- A per-tick step is backed by a deep state clone and is all-or-nothing.
- A per-window failure restores the window-start state and records `window_failures`.
- Prior committed windows stay valid.
- Cancellation between or inside windows returns `E_CANCEL`; uncommitted current-window state is discarded.
- Forced process termination leaves SQLite to roll back the in-flight transaction. The next load resumes from the last fully committed window only.

## 8. Parallel pipeline

Cross-window economy state is a dependency chain: window `i+1` needs the exact end state of window `i`. The implementation therefore parallelizes the safe immutable portion:

- window index/tick slicing
- intent offset computation
- static intent/target validation
- checkpoint metadata preparation

The state fold and global overflow/total merge remain an ordered serial section. Results flow through a bounded channel sized by `workers * buffer_per_worker`. When full, producers block. This retains deterministic commit order and exact equality with single-threaded settlement while still using the worker pool for parallel window preparation.

## 9. Workbench transaction model

The browser stages edits. A commit request contains one `batch_id` and a list of edits. The server:

1. clones the current working copy;
2. applies all edits;
3. resolves intent order by `(timestamp, seq)`;
4. clears stale truth, recomputes `params_version` and `scene_hash`;
5. validates the whole scenario;
6. runs windowed settlement;
7. swaps the committed pointer only after all prior steps succeed.

Any failure returns `E_TXN` with the original error as `cause`; the working copy and committed scene pointer remain unchanged.

## 10. Truth generation

Sample truth is produced by the same `Engine` over the same canonical ticks. The generator does not implement a second economy formula. This makes truth a regression fixture for core settlement and serialization, not an independent oracle with ambiguous rounding.
