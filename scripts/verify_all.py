"""Reproduce the acceptance checks for both dialects and fault recovery."""

from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PYTHON = sys.executable


def run(args: list[str], expect: int = 0) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        [PYTHON, *args],
        cwd=ROOT,
        text=True,
        capture_output=True,
    )
    if result.returncode != expect:
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        raise SystemExit(
            f"command failed ({result.returncode}, expected {expect}): "
            + " ".join(args)
        )
    return result


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def report(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--skip-data-generation", action="store_true")
    parser.add_argument("--include-failure-test", action="store_true")
    args = parser.parse_args()
    runs = ROOT / "runs" / "verify"

    if not args.skip_data_generation or not (ROOT / "data" / "schema.json").exists():
        run(["scripts/generate_data.py", "--output", "data"])
    run(["scripts/generate_baselines.py", "--data-dir", "data", "--output-dir", "baseline"])

    run(
        [
            "run_job.py",
            "--query",
            "samples/mysql_daily.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.mysql.json",
            "--output",
            str(runs / "mysql.csv"),
            "--report",
            str(runs / "mysql.json"),
            "--run-id",
            "verify-mysql",
        ]
    )
    mysql_sha = sha256(runs / "mysql.csv")
    assert mysql_sha == sha256(ROOT / "baseline" / "mysql_daily.csv")
    mysql_report = report(runs / "mysql.json")
    assert mysql_report["status"] == "success"
    assert mysql_report["planning"]["partial_aggregate_pushed_down"] is True
    assert (
        mysql_report["planning"]["planned_max_intermediate_rows"]
        <= mysql_report["planning"]["baseline_max_intermediate_rows"]
    )
    assert (
        mysql_report["planning"]["actual_max_intermediate_rows"]
        <= mysql_report["planning"]["baseline_max_intermediate_rows"]
    )
    assert mysql_report["resources"]["spill_bytes"] > 0

    run(
        [
            "run_job.py",
            "--query",
            "samples/mysql_daily.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.fault.json",
            "--output",
            str(runs / "fault.csv"),
            "--report",
            str(runs / "fault.json"),
            "--run-id",
            "verify-fault",
        ]
    )
    fault_report = report(runs / "fault.json")
    assert sha256(runs / "fault.csv") == mysql_sha
    retried = {
        item["partition_id"] for item in fault_report["faults"]["retried_partitions"]
    }
    assert "join_01:p0001" in retried

    for partition_count in (1, 7):
        run(
            [
                "run_job.py",
                "--query",
                "samples/mysql_daily.sql",
                "--data-dir",
                "data",
                "--config",
                "samples/config.mysql.json",
                "--partitions",
                str(partition_count),
                "--batch-rows",
                "37",
                "--output",
                str(runs / f"mysql-p{partition_count}.csv"),
                "--report",
                str(runs / f"mysql-p{partition_count}.json"),
                "--run-id",
                f"verify-mysql-p{partition_count}",
            ]
        )
        assert sha256(runs / f"mysql-p{partition_count}.csv") == mysql_sha

    run(
        [
            "run_job.py",
            "--query",
            "samples/postgres_streaming.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.postgres.json",
            "--output",
            str(runs / "postgres.csv"),
            "--report",
            str(runs / "postgres.json"),
            "--run-id",
            "verify-postgres",
        ]
    )
    run(
        [
            "run_job.py",
            "--query",
            "samples/postgres_streaming.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.postgres.json",
            "--stream-mode",
            "bounded",
            "--output",
            str(runs / "postgres-bounded.csv"),
            "--report",
            str(runs / "postgres-bounded.json"),
            "--run-id",
            "verify-postgres-bounded",
        ]
    )
    pg_sha = sha256(runs / "postgres.csv")
    assert pg_sha == sha256(ROOT / "baseline" / "postgres_streaming.csv")
    assert pg_sha == sha256(runs / "postgres-bounded.csv")
    pg_report = report(runs / "postgres.json")
    assert pg_report["stream"]["late_records"] > 0
    assert pg_report["stream"]["late_same_key_records"] > 0
    run(
        [
            "run_job.py",
            "--query",
            "samples/postgres_streaming.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.stream_fault.json",
            "--output",
            str(runs / "postgres-fault.csv"),
            "--report",
            str(runs / "postgres-fault.json"),
            "--run-id",
            "verify-postgres-fault",
        ]
    )
    pg_fault = report(runs / "postgres-fault.json")
    assert sha256(runs / "postgres-fault.csv") == pg_sha
    assert any(
        item["partition_id"] == "stream:p0000"
        for item in pg_fault["faults"]["retried_partitions"]
    )

    # Report determinism: the duration is the only permitted changing field.
    run(
        [
            "run_job.py",
            "--query",
            "samples/mysql_daily.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.mysql.json",
            "--output",
            str(runs / "determinism.csv"),
            "--report",
            str(runs / "determinism-1.json"),
            "--run-id",
            "verify-determinism",
        ]
    )
    run(
        [
            "run_job.py",
            "--query",
            "samples/mysql_daily.sql",
            "--data-dir",
            "data",
            "--config",
            "samples/config.mysql.json",
            "--output",
            str(runs / "determinism.csv"),
            "--report",
            str(runs / "determinism-2.json"),
            "--run-id",
            "verify-determinism",
        ]
    )
    first = report(runs / "determinism-1.json")
    second = report(runs / "determinism-2.json")
    first.pop("duration_ms", None)
    second.pop("duration_ms", None)
    assert first == second

    if args.include_failure_test:
        run(
            [
                "run_job.py",
                "--query",
                "samples/mysql_daily.sql",
                "--data-dir",
                "data",
                "--config",
                "samples/config.fault.json",
                "--max-retries",
                "0",
                "--output",
                str(runs / "exhausted.csv"),
                "--report",
                str(runs / "exhausted.json"),
                "--run-id",
                "verify-exhausted",
            ],
            expect=20,
        )
        exhausted = report(runs / "exhausted.json")
        assert exhausted["status"] == "failed"
        assert exhausted["error_code"] == "E_PARTITION_RETRY_EXHAUSTED"
        assert not (runs / "exhausted.csv").exists()
        assert exhausted["faults"]["completed_partitions"]

    print(
        json.dumps(
            {
                "status": "ok",
                "mysql_sha256": mysql_sha,
                "postgres_sha256": pg_sha,
                "fault_partition_retried": "join_01:p0001",
                "late_records": pg_report["stream"]["late_records"],
            },
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()

