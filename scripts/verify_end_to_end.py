#!/usr/bin/env python3
"""One-command end-to-end acceptance run.

Generates a fixed-seed dataset, runs a full job, hard-terminates a second job
after a committed batch, resumes it, and compares the deterministic business
digest of both reports.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]


def _cli(*args: str) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    env["PYTHONPATH"] = str(ROOT) + (
        os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else ""
    )
    return subprocess.run(
        [sys.executable, "-m", "minute_backfill.cli", *args],
        cwd=str(ROOT),
        env=env,
        text=True,
        capture_output=True,
    )


def _must(result: subprocess.CompletedProcess[str]) -> None:
    if result.returncode != 0:
        raise SystemExit(
            f"command failed with {result.returncode}\nstdout:\n{result.stdout}\n"
            f"stderr:\n{result.stderr}"
        )


def _load(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", type=Path, default=ROOT / ".work" / "verify")
    parser.add_argument("--json", type=Path, default=ROOT / "artifacts" / "verification.json")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--payment-rows", type=int, default=100_000)
    parser.add_argument("--refund-rows", type=int, default=40_000)
    parser.add_argument("--batches", type=int, default=128)
    parser.add_argument("--minutes", type=int, default=128)
    parser.add_argument("--lateness", type=int, default=60)
    parser.add_argument("--kill-after", type=int, default=64)
    args = parser.parse_args()

    out = args.out.resolve()
    shutil.rmtree(out, ignore_errors=True)
    data = out / "data"
    full_out = out / "full"
    killed_out = out / "killed"

    generated = _cli(
        "generate",
        "--out", str(data),
        "--seed", str(args.seed),
        "--payment-rows", str(args.payment_rows),
        "--refund-rows", str(args.refund_rows),
        "--batches", str(args.batches),
        "--minutes", str(args.minutes),
        "--force",
    )
    _must(generated)

    full = _cli(
        "run",
        "--data", str(data),
        "--state", str(out / "full.db"),
        "--out", str(full_out),
        "--slots", "6",
        "--workers", "2",
        "--allowed-lateness-seconds", str(args.lateness),
        "--reset-state",
    )
    _must(full)

    killed = _cli(
        "run",
        "--data", str(data),
        "--state", str(out / "killed.db"),
        "--out", str(killed_out),
        "--slots", "6",
        "--workers", "2",
        "--allowed-lateness-seconds", str(args.lateness),
        "--terminate-after-batch", str(args.kill_after),
        "--reset-state",
    )
    if killed.returncode != 137:
        raise SystemExit(f"termination injection expected 137, got {killed.returncode}")

    resumed = _cli(
        "run",
        "--data", str(data),
        "--state", str(out / "killed.db"),
        "--out", str(killed_out),
        "--slots", "6",
        "--workers", "2",
        "--allowed-lateness-seconds", str(args.lateness),
    )
    _must(resumed)

    full_report = _load(full_out / "report.json")
    resumed_report = _load(killed_out / "report.json")
    checks = {
        "business_digest_equal": (
            full_report["business_digest"] == resumed_report["business_digest"]
        ),
        "windows_equal": full_report["windows"] == resumed_report["windows"],
        "dedup_equal": full_report["dedup"] == resumed_report["dedup"],
        "late_equal": full_report["late"] == resumed_report["late"],
        "duration_under_360s": full_report["timing"]["duration_seconds"] < 360,
        "peak_memory_under_1gib": (
            full_report["resources"]["peak_memory_bytes"] < 1024**3
        ),
        "actual_slot_share_under_40pct": (
            full_report["skew"]["actual_max_share"] <= 0.40
        ),
    }
    passed = all(checks.values())
    summary = {
        "passed": passed,
        "checks": checks,
        "dataset": {
            "seed": args.seed,
            "payment_rows": args.payment_rows,
            "refund_rows": args.refund_rows,
            "refund_batches": args.batches,
            "generation": _load(data / "_generation_report.json"),
        },
        "full_run": {
            "business_digest": full_report["business_digest"],
            "windows": len(full_report["windows"]),
            "dedup_dropped": full_report["dedup"]["dropped"],
            "late_events": full_report["late"]["total"],
            "duration_seconds": full_report["timing"]["duration_seconds"],
            "peak_memory_bytes": full_report["resources"]["peak_memory_bytes"],
            "spill_bytes": full_report["resources"]["spill_bytes"],
            "skew_max_share": full_report["skew"]["max_share"],
            "actual_skew_max_share": full_report["skew"]["actual_max_share"],
        },
        "resume": {
            "recovery_count": resumed_report["recovery"]["count"],
            "checkpoint": resumed_report["recovery"]["checkpoints"][0],
        },
    }
    args.json.parent.mkdir(parents=True, exist_ok=True)
    args.json.write_text(
        json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(summary, ensure_ascii=True, indent=2, sort_keys=True))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
