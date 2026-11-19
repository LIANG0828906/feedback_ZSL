# Dual-dialect local SQL executor

This repository contains a single-host, columnar executor for two legacy SQL
pipelines:

- MySQL 8.0 daily aggregate SQL over `fact_sales`, `dim_store`, and
  `dim_product`.
- PostgreSQL 15 streaming deduplication SQL over `stream_events`.

It reads a metadata-declared local CSV/Parquet directory, normalizes dialect
semantics, chooses a statistics-based join order, pushes filters and partial
aggregates down, hash-repartitions every join input, executes in columnar
batches with DuckDB, and checkpoints completed partitions for fault recovery.

The implementation is Python 3.11 compatible and is intended for Windows
10/11 or Windows Server 2022. Paths are handled with `pathlib`; DuckDB spill and
temporary files use forward-slash SQL paths so the same code runs on Windows and
POSIX hosts.

## Quick start

### Windows PowerShell

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt

.\.venv\Scripts\python scripts\generate_data.py --output data
.\.venv\Scripts\python scripts\generate_baselines.py --data-dir data --output-dir baseline

.\.venv\Scripts\python run_job.py `
  --query samples\mysql_daily.sql `
  --data-dir data `
  --config samples\config.mysql.json `
  --output runs\mysql\result.csv `
  --report runs\mysql\report.json

.\.venv\Scripts\python run_job.py `
  --query samples\postgres_streaming.sql `
  --data-dir data `
  --config samples\config.postgres.json `
  --output runs\postgres\result.csv `
  --report runs\postgres\report.json
```

### Acceptance run

```powershell
.\.venv\Scripts\python scripts\verify_all.py --include-failure-test
```

The verifier regenerates the deterministic samples, creates independent
baselines, runs both dialects, injects `join_01:p0001` batch 1, checks result
SHA-256 equality, checks partition-count invariance, checks bounded/streaming
parity, checks report determinism, and optionally checks retry exhaustion.

## Layout

| Path | Purpose |
| --- | --- |
| `engine/sql.py` | Tokenizer and AST parser for the bounded SQL subset |
| `engine/dialects.py` | MySQL/PostgreSQL semantic normalization |
| `engine/catalog.py` | Metadata, CSV/Parquet scanning, statistics |
| `engine/planner.py` | Filter/partial-aggregate pushdown and DP join ordering |
| `engine/runtime.py` | Partition checkpoints, fault injection, retries, quotas |
| `engine/execution.py` | Hash repartition, vectorized joins, aggregation, output |
| `engine/streaming.py` | Event-time watermark and `DISTINCT ON` deduplication |
| `engine/job.py` | One-shot orchestration and stable JSON report |
| `scripts/generate_data.py` | Fixed-seed 30k fact / 20k dimension samples |
| `scripts/generate_baselines.py` | Independent dialect baseline generation |
| `scripts/verify_all.py` | End-to-end acceptance checks |
| `samples/` | Two sample queries and executor/fault configs |

## Input metadata

`data/schema.json` is the source of truth. Each table declares its files,
format, columns, key, row count, and cardinality/null statistics:

```json
{
  "tables": {
    "fact_sales": {
      "files": ["fact_sales.parquet"],
      "format": "parquet",
      "columns": {
        "sale_id": "BIGINT",
        "event_ts": "TIMESTAMP",
        "store_id": "BIGINT",
        "amount": "BIGINT"
      },
      "primary_key": ["sale_id"],
      "row_count": 30000,
      "stats": {
        "store_id": {"distinct_count": 60, "null_count": 0}
      }
    }
  }
}
```

CSV and Parquet files may be mixed across tables. Empty nullable string fields
in CSV are normalized to SQL `NULL`. If statistics are omitted, the catalog
computes distinct/null counts from the input using Arrow.

## Dialect semantics

| Boundary | MySQL mode | PostgreSQL mode |
| --- | --- | --- |
| Identifier quote | Backticks | Double quotes |
| `ORDER BY ... ASC` default | `NULLS FIRST` | `NULLS LAST` |
| `ORDER BY ... DESC` default | `NULLS LAST` | `NULLS FIRST` |
| Integer division | `a DIV b`, trunc toward zero | `a / b` for integer operands, trunc toward zero |
| Modulo | `MOD`, sign follows dividend | `%`, sign follows dividend |
| Date truncation | `DATE(ts)` | `date_trunc('day', ts)` |

Explicit `NULLS FIRST/LAST` always overrides the dialect default. The report
contains the effective switches under `dialect_semantics`.

The parser intentionally supports a bounded SQL subset: `SELECT`/`DISTINCT
ON`, inner joins, conjunctions/comparisons, `IS NULL`, `BETWEEN`, arithmetic,
`CAST`, `GROUP BY`, `HAVING`, `ORDER BY`, and `LIMIT`. Unsupported constructs
fail with `E_SQL_UNSUPPORTED` instead of being silently mistranslated.

## Planning and execution

1. Single-table filters are pushed to scans.
2. Aggregates over the largest compatible fact relation are rewritten into
   partial states (`sum`, `count`, `min`, `max`; `AVG` becomes
   `sum`/`count`).
3. A dynamic program uses filtered row counts and join-key cardinalities to
   select a connected join order. The plan tree reports estimated and actual
   rows.
4. Every join side is hash-partitioned by the join key into the configured
   number of execution partitions. Partitions are processed in Arrow/DuckDB
   columnar batches; no per-row join objects are materialized.
5. The final aggregate is hash-partitioned by its group keys. Each partition
   file is sorted with `ORDER BY ALL`, then a global dialect-aware sort produces
   the deterministic result.

`--partitions`, `--batch-rows`, `--max-workers`, and the related JSON fields
control execution. The same input produces the same result for any partition
count.

## Fault injection and recovery

Set a partition id and zero-based input batch in JSON:

```json
{
  "fault_injection": {
    "partition_id": "join_01:p0001",
    "batch": 1
  },
  "max_retries": 2
}
```

or use environment variables:

```text
EXECUTOR_FAULT_PARTITION=join_01:p0001
EXECUTOR_FAULT_BATCH=1
EXECUTOR_MAX_RETRIES=2
```

The injected fault fires once per job checkpoint. Completed partition files and
their manifest entries are retained; only the failed partition is resubmitted.
Use `--resume` after a process/job failure to continue from the checkpoint.
Checkpoints are invalidated automatically when the query, schema, engine state
format, or resource shape changes.

If retries are exhausted, the job:

- exits with stable code `E_PARTITION_RETRY_EXHAUSTED` and process exit code 20;
- deletes any target result file;
- writes `failure_manifest.json` with the completed partition list;
- never reports partial rows as a successful result.

## Streaming semantics

The PostgreSQL query uses `DISTINCT ON (event_key)` with event time and ingest
sequence ordering. The streaming path:

- tracks a watermark from the maximum observed event time minus allowed
  lateness;
- keeps the earliest event time per key;
- finalizes candidates below the watermark;
- drops and counts later records for finalized keys without rewriting output.

`stream_mode=incremental` processes configured batches. `stream_mode=bounded`
uses the same earliest-event state machine without watermark expiry. With the
provided sample, both modes produce byte-identical results.

## Resource control

Default limits are intentionally below the task envelope:

- 2 concurrent execution units (hard cap for the two-logical-CPU envelope);
- 768 MiB estimated process-memory budget;
- 192 MiB DuckDB memory per worker;
- 1.5 GiB temporary-data limit.

The scheduler uses a fixed-size `ThreadPoolExecutor`, each DuckDB connection is
limited to one thread and its own spill directory, and every read batch is
checked against a shared memory governor. Exceeding a limit raises a stable
business error (`E_MEMORY_LIMIT`, `E_TEMP_LIMIT`) before the operating system
can kill the process. Aggregate state crossing the configured threshold is
written under the local spill directory; successful jobs remove the temporary
workspace after recording `spill_bytes`. Six-way parallelism is provided by
running six independent logical instances, each capped at two CPU units and its
own memory/temp budget.

## Report

Reports are JSON with recursively sorted keys. Repeated successful runs over
the same input differ only in `duration_ms`. Important fields include:

- `dialect_semantics`: active NULL-order, division/modulo, date and quoting
  switches;
- `plan`: plan tree with estimated/actual operator rows;
- `operators`: sorted per-partition input/output row counts;
- `repartition`: repartition operation and partition counts;
- `faults`: injection config, retried partitions, completed partition list;
- `stream`: mode, late-record and late-same-key counts, final watermark;
- `resources`: bounded peak-memory estimate, active concurrency, spill bytes;
- `result`: path, row count, format, and SHA-256.
