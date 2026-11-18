# Operations and error handling

## Exit / error codes

| Code | CLI exit | Meaning |
| --- | ---: | --- |
| `E_INPUT` | 2 | manifest missing/invalid, schema mismatch, malformed Parquet/JSONL, bad batch/shard |
| `E_STATE` | 3 | state lock held, SQLite corrupt, schema version unsupported |
| `E_MEMORY` | 4 | configured memory limit or Python allocator exceeded |
| `E_INTERNAL` | 5 | unexpected engine failure |
| `E_INTERRUPTED` | 130 | operator interrupt |

Input errors always identify a shard id or batch id in the message and
`last_error.json`.  A bad input unit is never skipped: the current transaction
rolls back and the job stops.

## State lock and corruption

The engine holds an OS-level exclusive lock on `<state>.lock` and uses SQLite
`BEGIN IMMEDIATE` for every unit.  A second writer exits `E_STATE` without
touching committed state.

At startup SQLite `quick_check` and the schema version are verified.  If the
database is corrupt:

1. the DB, WAL and SHM files are renamed to `*.corrupt.<timestamp>`;
2. `<state>.poisoned` records the reason;
3. the run exits `E_STATE`;
4. later runs refuse to read the polluted state until an operator explicitly
passes `--reset-state`.

`--reset-state` is the only destructive operation.  It is performed only after
the exclusive lock has been acquired.

## Recovery

Re-run the same command with the same `--state` path.  Inputs whose ids are in
`consumed_inputs` are skipped; open key-windows and the watermark are loaded
from SQLite.  `report.json.recovery` contains:

* cumulative recovery count;
* one checkpoint per resumed run;
* the previous commit sequence and input id;
* watermark, open key-windows and committed minute count at the checkpoint.

`--terminate-after-batch N` performs a hard `os._exit(137)` immediately after
batch `N` has committed, to exercise external termination.  It writes
`termination_injected.json` first.  A run killed at any point inside a batch
rolls back that batch.

## Memory, spill and slots

* `--slots` is the number of logical processing slots (default 6).
* `--workers` is capped at 2 OS processes; logical slot 6/6 is still modeled.
* `--memory-limit-mb` defaults to 900 and is capped at 1024.
* `--spill-mb` controls when aggregate window rows spill to
  `--temp-dir` as JSONL; spill files and the run temp directory are removed at
  normal job end.
* On `E_MEMORY` the current transaction rolls back.  `report.partial.json` and
  `windows.partial.csv` contain all previously committed windows.

The rating of the generated input is 100,000 Parquet payment rows plus 40,000
JSONL refund rows in 128 batches.  On the 2-CPU reference container the full
test run completed in about 10 seconds with a process-tree peak near 400 MB,
well below 6 minutes and 1 GiB.

## Skew

The logical assigner keeps all events of a `(payment_id, minute)` together.
Keys above `--skew-threshold` (default 0.40) are expanded to one unit per
minute and distributed across slots.  The report contains both:

* `skew.slot_loads`: deterministic global plan over all canonical key-windows;
* `skew.actual_slot_loads`: cumulative events actually aggregated per slot.

Window results do not depend on slot count or worker count.
