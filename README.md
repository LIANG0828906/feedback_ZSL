# Minute-level feature backfill engine

An offline, single-host, resumable engine for replaying Parquet payment history
together with minute-batched JSONL refunds.  It produces per-minute payment,
refund and net aggregates with one canonical deduplication rule on both input
routes, event-time watermark closure, late-event accounting, logical-slot
parallel aggregation, hot-key window splitting, SQLite checkpoints and a
deterministic audit report.

**Quick start (Windows 10/11, Python 3.11)**

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt

python -m minute_backfill.cli generate `
  --out data\demo --seed 42 --payment-rows 100000 --refund-rows 40000 --batches 128

python -m minute_backfill.cli run `
  --data data\demo --state state\demo.db --out out\demo `
  --slots 6 --workers 2 --allowed-lateness-seconds 60
```

The report is `out\demo\report.json`; minute aggregates are also in
`windows.csv` and per-payment join detail in `key_windows.csv`.

**What is implemented**

- Strict Parquet schema/row validation and strict JSONL parsing against
  `manifest.json`; malformed input exits `E_INPUT` with the shard/batch id.
- One dedup rule for both routes: side + payment id + event minute, earliest
  event time wins, first arrival breaks equal-time ties.
- Same-minute, same-payment-key logical Join; refund must be at or after
  payment.
- Watermark = `max(consumed batch declared_event_time) - allowed_lateness`,
  monotonic across gaps and recovery; closed windows never change.
- SQLite transaction per shard/batch containing canonical events, window
  aggregate, minute output, commit flag, watermark, late/dedup counters and
  consumed marker.
- Hard external termination injection and resume from the last committed
  checkpoint with identical windows, late counts and final results.
- Six logical slots with at most two worker processes.  Hot keys above 40% are
  split by minute while each payment/refund key-window stays together.
- Local spill of aggregate state, temp cleanup, observed process-tree memory
  enforcement and `E_MEMORY` preservation of committed windows.
- Deterministic report with `business_digest`, Prometheus metrics and fixed-seed
  generation of 5% duplicate, out-of-order and 30–900s delayed refund events.

**Documentation**

- `docs/semantics.md` – exact dedup, Join, watermark and late semantics.
- `docs/operations.md` – Windows operations, error codes, recovery, corruption
  quarantine, memory/spill tuning.
- `docs/report.md` – report and output contract.
- `examples/manifest.json` – field checklist example.

**Verification**

```powershell
python -m pytest -q
```

The suite includes an independent in-memory oracle, full 100k+40k+128-batch
scale test, 0 vs 300s lateness comparison, controlled duplicate tie/replacement
tests, crash/resume equivalence, repeated-commit idempotence, hot-key split
comparison across one vs six slots, `E_INPUT`/`E_STATE`/`E_MEMORY`, spill
cleanup and report determinism.

The generator fixes row and batch counts.  On-disk Parquet bytes are
encoding-dependent; the six-column Snappy sample used by the acceptance run is
smaller than the 120 MB reference size, while the row count, batch count,
duplicate/delay ratios and Join workload are reproduced.

Reference run in this workspace: 12 tests pass; full-scale generation + engine
run completes in about 10 seconds, report peak process-tree RSS about 400 MB.

For a durable acceptance artifact:

```powershell
python scripts\verify_end_to_end.py
```

This generates the full dataset, runs the complete job, hard-terminates a
second job after a committed batch, resumes it, compares
`business_digest`/windows/dedup/late fields, and writes
`artifacts\verification.json`.
