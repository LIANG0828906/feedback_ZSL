from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from typing import Any

from minute_backfill.engine import BackfillEngine, EngineConfig


ROOT = Path(__file__).resolve().parents[1]


def run_engine(
    tmp_path: Path,
    data_dir: Path,
    name: str,
    *,
    workers: int = 0,
    slots: int = 6,
    lateness: int = 0,
    spill_mb: float = 128.0,
    memory_limit_mb: float = 900.0,
    terminate_after_batch: int | None = None,
    reset: bool = True,
) -> tuple[dict[str, Any], Path, Path]:
    state = tmp_path / f"{name}.db"
    out = tmp_path / f"{name}-out"
    engine = BackfillEngine(
        EngineConfig(
            data_dir=data_dir,
            state_db=state,
            out_dir=out,
            slots=slots,
            workers=workers,
            allowed_lateness_seconds=lateness,
            spill_mb=spill_mb,
            memory_limit_mb=memory_limit_mb,
            terminate_after_batch=terminate_after_batch,
            reset_state=reset,
        )
    )
    report = engine.run()
    return report, state, out


def run_cli(*args: str, cwd: Path = ROOT) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    env["PYTHONPATH"] = str(ROOT) + (
        os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else ""
    )
    return subprocess.run(
        [sys.executable, "-m", "minute_backfill.cli", *args],
        cwd=str(cwd),
        env=env,
        text=True,
        capture_output=True,
        timeout=300,
    )


def business_signature(report: dict[str, Any]) -> dict[str, Any]:
    return {
        "windows": report["windows"],
        "dedup": report["dedup"],
        "late": report["late"],
        "join_diagnostics": report["join_diagnostics"],
        "skew": report["skew"],
        "processing": {
            key: report["processing"][key]
            for key in (
                "consumed_inputs",
                "consumed_shards",
                "consumed_batches",
                "last_commit_seq",
                "last_consumed_batch_index",
                "watermark",
                "watermark_us",
            )
        },
    }


def normalized_windows(report: dict[str, Any]) -> list[dict[str, Any]]:
    result = []
    for window in report["windows"]:
        result.append(
            {
                "window_start": window["window_start"],
                "pay_count": window["pay_count"],
                "pay_amount_minor": window["pay_amount_minor"],
                "refund_count": window["refund_count"],
                "refund_amount_minor": window["refund_amount_minor"],
                "net_minor": window["net_minor"],
                "close_reason": window["close_reason"],
            }
        )
    return result
