# Technical notes

## 1. Canonical model and schema versions

All loaders produce the same `model.Project`:

- Quota and guarantee rates become PPM (`fixed.Scale = 1_000_000`).
  - per-mille `x -> 1000x PPM`
  - per-ten-thousand `x -> 100x PPM`
- Node and burst capacities become bytes. KiB uses exact `* 1024`.
- Event and wait windows become logical ticks using `tickDurationMs`; ms inputs
  must align to the declared tick.
- V1 is snake_case/flat/byte/per-mille quota/per-myriad guarantee/tick events.
- V2 is camelCase/rack-nested/KiB/per-myriad quota/per-mille guarantee/ms events.
- V3 is envelope/flat/byte/mixed scales/tick events with different names.

A missing burst pointer is not converted to a silent ordinary zero. It produces:

- `burstFieldPresent=false`
- `burstEnabled=false`
- `burstDegradation="RULE_BURST_FIELD_ABSENT:NOT_ENABLED"`

An explicit zero is annotated as `RULE_BURST_EXPLICIT_ZERO:NOT_ENABLED`.

## 2. Deterministic ledger settlement

Events at a tick are normalized into semantic phases independent of input order:

1. weight adjustments;
2. releases (burst usage is released first);
3. exact canonical demand settlement.

Every request is split into ordered demand classes:

1. `guarantee`: bytes up to `max(0, GuaranteeBytes - QuotaUsedBytes)`;
2. `quota`: remaining ordinary quota;
3. `burst`: remaining explicit burst cap for burst events.

All guarantee demands sort before quota demands, and quota demands before burst
demands. Within a class the key is priority descending, scheduling weight
descending, guarantee size descending, enqueue tick ascending, then event ID.
This makes the result independent of submitted event order and worker schedule.

Non-guarantee allocation is capped by physical node headroom minus the sum of
eligible tenants' unused guarantee reserves. Therefore ordinary quota and burst
allocation cannot consume capacity currently reserved for another guarantee.
If a later edit increases a guarantee after burst usage exists, the waiting
guarantee records the largest existing burst user as
`encroachingTenantId`, the victim as `victimTenantId`, and
`C-GUARANTEE-RESERVE`.

The engine checks these invariants after every tick:

- `QuotaUsedBytes <= QuotaBytes`;
- `BurstUsedBytes <= BurstCapBytes`;
- `UsedBytes == QuotaUsedBytes + BurstUsedBytes`;
- `UsedBytes <= QuotaBytes + BurstCapBytes`;
- node used/pre-deducted bytes never exceed capacity;
- the sum of tenant usage pinned to a node equals node used bytes;
- a tenant is pinned to at most one node at a time.

Shortages may partially admit a request; the remaining bytes enter the queue.
TTL exhaustion emits `REJECT_WAIT_TTL`; hard quota/node ceilings emit stable
`REJECT_QUOTA_HARD` or `REJECT_NODE_CAPACITY`; queue exhaustion emits
`BP_TENANT_QUEUE_FULL`.

## 3. Gate and reconciliation

`gate.FastGate.Decide` performs O(1) arithmetic against:

- node fast headroom: capacity - settled used - provisional pre-deduction;
- the requesting tenant's own remaining quota/burst balance;
- the absolute request ceiling.

It never consults or reserves another tenant's balance. If the injected clock
or wall clock exceeds the declared budget, the decision is deterministically
downgraded to `QUEUE` with `GATE_LATENCY_DEGRADED_QUEUE` and
`C-LATENCY-BUDGET`.

The engine records both the provisional reserve and exact settlement:

- `deltaBytes = settledBytes - reservedBytes`;
- `matched`: delta zero;
- `over_reserved_released`: fast path reserved more than exact priority
  settlement used;
- `fast_queue_caught_up`: fast path queued but exact canonical priority admitted
  bytes after provisional holds were released.

All provisional holds are released before exact settlement, and the report's
`Reconciliation.Closed` requires node pre-deducted balances to return to zero.
`gate-reconcile` is a fixed sample where one low-priority request is
over-reserved and a high-priority request catches up in the same tick.

## 4. Fixed point and error bounds

All rate math uses int64 PPM values. `fixed.RateBytes` uses an integer product
only to detect whether the declared int64 result width overflows; no floating
point is used. Overflow returns `Overflow=true`, `Saturated=true`, and the
declared saturation value. Critical cases are in
`generate.CriticalDiagnostics` and are written to
`samples/seed-42/critical-conversions.json`. One injected drift case proves the
verifier reports a locatable tenant/tick/field tolerance failure.

Generated capacities are MiB/KiB aligned and rate-aligned, so normal truth
differences are zero. The declared bound remains one byte for floor rounding.

## 5. Incremental invalidation

`incremental.Manager` caches every tick report and committed state. An edit to a
node, tenant quota/guarantee, or weight event:

1. finds direct dependency ticks from report dependency keys;
2. starts at the edit's effective tick (or event tick);
3. clones the committed state at `start-1`;
4. recomputes forward;
5. compares each carried state hash with the corresponding base state;
6. stops when hashes converge and reuses later cached reports.

The proof contains direct ticks, recomputed/propagated ticks, reused ticks,
per-tick sources, and a `ClosureBoundary`. Persistent declaration edits can
legitimately leave the closure open through the horizon because every later
snapshot contains the changed capacity/quota field; early ticks remain reused.
A same-value weight touch test demonstrates finite convergence and reuse after
the boundary. Every incremental result is compared with a fresh full run.

## 6. Concurrency and backpressure

`runtime.Pool` has fixed workers and a bounded queue. Submission is
non-blocking under exhaustion and returns the stable code
`BP_WORKER_BUDGET_EXHAUSTED`; it never silently drops or admits work. Metrics
record count, total, last, and maximum actual wall duration per tenant. Timing
values are diagnostics only; correctness comparisons use canonical reports and
state hashes.

`runtime.RunGateBatch` stores results by input index, so completion order does
not affect output order. Acceptance runs the same gate inputs with 1, 2, 4, and
8 workers and compares canonical records. Full ledger runs are also executed
concurrently and under deterministic event shuffles.

## 7. Checkpoint and forced termination

The standard library-only `checkpoint.Store` implements the explicitly allowed
local equivalent of SQLite:

1. write `ticks/<tick>.entities.json` temp file;
2. fsync file;
3. atomically rename entity file;
4. fsync directory;
5. invoke crash hook (used by the SIGKILL demo after entity durability);
6. write `committed.json.tmp`, fsync, atomically rename the global pointer;
7. fsync directory.

`committed.json` contains format version, project fingerprint, committed tick,
entity path, and entity SHA-256. Recovery reads only the global pointer, then
the matching entity file. An entity file from tick 4 without an updated pointer
to tick 4 is ignored; recovery resumes from tick 3.

A format or project fingerprint mismatch returns a typed
`ErrVersionMismatch`. The CLI then resets the old state and performs a full
recompute rather than partially reusing an incompatible ledger.

`SIGINT`/`SIGTERM` are handled at tick boundaries and write
`exit-report.json`; `SIGKILL` cannot be handled and therefore makes no report
promise. The crash subprocess test proves the restart path independently.

### Possible SQLite mapping

The same commit order maps directly to:

```sql
BEGIN IMMEDIATE;
-- upsert tenant_state rows
-- upsert node_state rows
-- replace queue_item rows
-- replace reserve rows
UPDATE commit_pointer SET committed_tick = ?;
COMMIT;
```

A pure-Go `modernc.org/sqlite` implementation could replace `checkpoint.Store`
without changing `engine.State` or recovery semantics; the file store was chosen
to keep offline verification dependency-free. A ready-to-map table and
transaction order is in `checkpoint/sqlite_schema.sql`.
