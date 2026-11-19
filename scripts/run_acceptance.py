#!/usr/bin/env python3
"""End-to-end acceptance runner for batch, stream, fault and resume paths."""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from iovflow.generator import GeneratorConfig, generate
from tests.helpers import independent_results, load_results


def run_cli(args: list[str], check: bool = True) -> tuple[int, dict, str]:
    proc = subprocess.run(
        [sys.executable, "-m", "iovflow", *args],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
    )
    payload = {}
    if proc.stdout.strip():
        try:
            payload = json.loads(proc.stdout)
        except json.JSONDecodeError:
            payload = {"raw_stdout": proc.stdout}
    if check and proc.returncode != 0:
        raise SystemExit(
            f"command failed ({proc.returncode}): {' '.join(args)}\n{proc.stderr[-4000:]}"
        )
    return proc.returncode, payload, proc.stderr


def run_job(
    data: str,
    work: Path,
    job: str,
    mode: str,
    lateness: int,
    extra: list[str] | None = None,
    wait: bool = True,
) -> dict:
    args = [
        "run",
        "--sql",
        "sql/window_5m.sql",
        "--input",
        data,
        "--mode",
        mode,
        "--job-id",
        job,
        "--state-db",
        str(work / "state.db"),
        "--results-db",
        str(work / "results.db"),
        "--temp-dir",
        str(work / "tmp"),
        "--report-dir",
        str(work / "reports"),
        "--allowed-lateness",
        str(lateness),
        "--parallelism",
        "2",
        "--max-shards",
        "4",
        "--no-progress-timeout",
        "30",
    ]
    if extra:
        args.extend(extra)
    _, payload, _ = run_cli(args)
    return payload


def verify(work: Path, a: str, b: str) -> dict:
    _, payload, _ = run_cli(
        [
            "verify",
            "--job-a",
            a,
            "--job-b",
            b,
            "--results-db",
            str(work / "results.db"),
            "--state-db",
            str(work / "state.db"),
        ]
    )
    return payload


def duplicate_rows(work: Path, job: str) -> int:
    conn = sqlite3.connect(work / "results.db")
    try:
        return int(
            conn.execute(
                "SELECT COUNT(*) FROM (SELECT vehicle_id, window_start_epoch, COUNT(*) c "
                "FROM window_results WHERE job_id=? GROUP BY vehicle_id, window_start_epoch "
                "HAVING c>1)",
                (job,),
            ).fetchone()[0]
        )
    finally:
        conn.close()


def reference_check(work: Path, data: Path, job: str, lateness: int) -> dict:
    expected, late = independent_results(
        work,
        ROOT / "sql/window_5m.sql",
        allowed_lateness=lateness,
        data_dir=data,
    )
    actual = load_results(work / "results.db", job)
    if set(expected) != set(actual):
        return {
            "ok": False,
            "reason": "window key sets differ",
            "expected": len(expected),
            "actual": len(actual),
        }
    for key, value in expected.items():
        got = actual[key]
        for field, want in value.items():
            have = got.get(field)
            if isinstance(want, float):
                if abs(float(have) - want) > 1e-5:
                    return {"ok": False, "reason": f"{key} {field}", "expected": want, "actual": have}
            elif have != want:
                return {"ok": False, "reason": f"{key} {field}", "expected": want, "actual": have}
    return {"ok": True, "windows": len(expected), "late_events": late}


def kill_and_resume(work: Path, data: str, job: str, lateness: int) -> dict:
    args = [
        "run",
        "--sql",
        "sql/window_5m.sql",
        "--input",
        data,
        "--mode",
        "stream",
        "--job-id",
        job,
        "--state-db",
        str(work / "state.db"),
        "--results-db",
        str(work / "results.db"),
        "--temp-dir",
        str(work / "tmp"),
        "--report-dir",
        str(work / "reports"),
        "--allowed-lateness",
        str(lateness),
        "--parallelism",
        "2",
        "--max-shards",
        "4",
        "--no-progress-timeout",
        "30",
    ]
    proc = subprocess.Popen(
        [sys.executable, "-m", "iovflow", *args],
        cwd=str(ROOT),
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    killed = False
    committed = 0
    for _ in range(4000):
        time.sleep(0.005)
        if proc.poll() is not None:
            break
        try:
            conn = sqlite3.connect(work / "state.db")
            committed = int(
                conn.execute(
                    "SELECT COUNT(*) FROM job_batches WHERE job_id=? "
                    "AND committed_at IS NOT NULL",
                    (job,),
                ).fetchone()[0]
            )
            conn.close()
        except Exception:
            committed = 0
        if committed >= 8:
            proc.kill()
            killed = True
            break
    proc.wait()
    if proc.stderr is not None:
        proc.stderr.close()
    if not killed:
        raise SystemExit(f"external kill point was not reached (finished after {committed} batches)")
    _, payload, _ = run_cli(args + ["--resume"])
    return {"report": payload, "killed_after_batches": committed}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--quick", action="store_true")
    parser.add_argument("--regen", action="store_true")
    parser.add_argument("--data", default=None)
    parser.add_argument("--work", default=None)
    parser.add_argument("--skip-resume", action="store_true")
    args = parser.parse_args()
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    work = Path(args.work or ROOT / "artifacts" / f"acceptance-{stamp}")
    work.mkdir(parents=True, exist_ok=True)
    data = Path(args.data or (work / "data" if args.quick else ROOT / "data"))
    rows, batches = (8000, 24) if args.quick else (80000, 96)
    if args.regen or not (data / "manifest.json").exists():
        print(f"generating {rows} rows / {batches} batches -> {data}", file=sys.stderr)
        generate(
            GeneratorConfig(
                out_dir=str(data),
                rows=rows,
                batches=batches,
                vehicles=120 if args.quick else 1200,
                trips_per_vehicle=4,
                hot_vehicle_share=0.12,
            )
        )

    summary: dict = {
        "generated_at": stamp,
        "data": str(data),
        "work": str(work),
        "quick": args.quick,
        "checks": {},
    }
    batch = run_job(str(data), work, "acc-batch", "batch", 300)
    stream = run_job(str(data), work, "acc-stream", "stream", 300)
    summary["batch"] = {
        k: batch.get(k) for k in ("status", "results", "resources", "performance")
    }
    summary["stream"] = {
        k: stream.get(k) for k in ("status", "results", "resources", "performance")
    }
    v = verify(work, "acc-batch", "acc-stream")
    summary["checks"]["batch_stream_equal"] = v["ok"]
    summary["checks"]["no_duplicate_windows"] = (
        duplicate_rows(work, "acc-batch") == 0 and duplicate_rows(work, "acc-stream") == 0
    )
    summary["checks"]["batch_under_180s"] = batch["duration_seconds"] <= 180
    summary["checks"]["stream_p95_under_2s"] = (
        stream["performance"]["batch_p95_seconds"] <= 2.0
    )
    summary["checks"]["peak_memory_under_1GiB"] = (
        batch["resources"]["peak_memory_mb"] <= 1024
        and stream["resources"]["peak_memory_mb"] <= 1024
    )
    summary["checks"]["max_shard_ratio_under_40pct"] = all(
        r["max_shard_ratio"] <= 0.40 for r in (batch["routing"], stream["routing"])
    )
    reference = reference_check(work, data, "acc-batch", 300)
    summary["reference"] = reference
    summary["checks"]["independent_reference_equal"] = reference["ok"]
    summary["checks"]["reference_late_count"] = (
        reference.get("late_events") == batch["results"]["late_events"]
    )

    # Real stage-worker termination, followed by same-stage takeover.
    fault_batch = max(2, batches // 2)
    fault = run_job(
        str(data),
        work,
        "acc-fault",
        "stream",
        300,
        extra=[
            "--inject-kill",
            f"stage=window_aggregate,batch={fault_batch},shard=0",
            "--inject-delay-ms",
            "0",
        ],
    )
    vf = verify(work, "acc-stream", "acc-fault")
    summary["fault"] = {
        "status": fault.get("status"),
        "takeover_count": fault["fault_recovery"]["takeover_count"],
        "events": fault["fault_recovery"]["events"],
    }
    summary["checks"]["fault_recovery_equal"] = vf["ok"]
    summary["checks"]["takeover_observed"] = fault["fault_recovery"]["takeover_count"] >= 1

    # Lateness 0 and a large tolerance must move closure/late counts in the
    # declared direction while batch and stream remain equal for each setting.
    l0 = run_job(str(data), work, "acc-late0", "batch", 0)
    l0s = run_job(str(data), work, "acc-late0-stream", "stream", 0)
    l900 = run_job(str(data), work, "acc-late900", "stream", 900)
    v0 = verify(work, "acc-late0", "acc-late0-stream")
    summary["lateness"] = {
        "late0_events": l0["results"]["late_events"],
        "late900_events": l900["results"]["late_events"],
        "late0_watermark": l0["watermark"]["final"],
        "late900_watermark": l900["watermark"]["final"],
    }
    summary["checks"]["late0_has_late_events"] = l0["results"]["late_events"] > 0
    summary["checks"]["late900_has_no_late_events"] = l900["results"]["late_events"] == 0
    summary["checks"]["late_semantics_batch_stream_equal"] = v0["ok"]

    if not args.skip_resume:
        resumed = kill_and_resume(work, str(data), "acc-resume", 300)
        vr = verify(work, "acc-stream", "acc-resume")
        summary["resume"] = resumed
        summary["checks"]["external_kill_resume_equal"] = vr["ok"]
        summary["checks"]["resume_reached_commit_point"] = (
            resumed["report"]["checkpoint"]["resumed_from_batch"] >= 0
        )

    summary["ok"] = all(summary["checks"].values())
    out = work / "acceptance_report.json"
    out.write_text(json.dumps(summary, indent=2, sort_keys=True, default=str), encoding="utf-8")
    print(json.dumps(summary, indent=2, sort_keys=True, default=str))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

