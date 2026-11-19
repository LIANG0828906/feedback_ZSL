from __future__ import annotations

import json
import sqlite3
from pathlib import Path
from typing import Any

from iovflow.accum import add_event, merge_accs, new_acc, project_result
from iovflow.config import RunConfig
from iovflow.expr import ExprEvaluator
from iovflow.input import load_input
from iovflow.orchestrator import run_job
from iovflow.sql_parser import parse_sql
from iovflow.util import fingerprint, parse_time


def make_config(
    root: Path,
    job_id: str,
    mode: str = "stream",
    allowed_lateness: int = 300,
    **overrides: Any,
) -> RunConfig:
    values = dict(
        job_id=job_id,
        sql_path=str(root / "window_5m.sql"),
        input_dir=str(root / "data"),
        mode=mode,
        state_db=str(root / f"state-{job_id}.db"),
        results_db=str(root / f"results-{job_id}.db"),
        temp_dir=str(root / "tmp"),
        report_dir=str(root / "reports"),
        parallelism=2,
        max_shards=4,
        allowed_lateness=allowed_lateness,
        no_progress_timeout=10,
    )
    values.update(overrides)
    return RunConfig(**values)


def independent_results(
    root: Path,
    sql_path: Path,
    allowed_lateness: int = 300,
    dedupe_keys: tuple[str, ...] = (
        "vehicle_id",
        "trip_id",
        "event_time",
        "event_type",
    ),
    data_dir: Path | None = None,
) -> tuple[dict[tuple[str, int], dict[str, Any]], int]:
    plan = parse_sql(Path(sql_path).read_text(encoding="utf-8"))
    dataset = load_input(data_dir or (root / "data"), "vehicle_id")
    filter_eval = ExprEvaluator(plan.filter_ast, plan.sql)
    window_eval = ExprEvaluator(plan.window.expr_ast, plan.sql)
    seen: set[str] = set()
    states: dict[tuple[str, int], dict[str, Any]] = {}
    completed: dict[tuple[str, int], dict[str, Any]] = {}
    max_declared: int | None = None
    late = 0
    for batch in dataset.batches:
        max_declared = (
            batch.declared_epoch
            if max_declared is None
            else max(max_declared, batch.declared_epoch)
        )
        watermark = max_declared - allowed_lateness
        with open(batch.path, "r", encoding="utf-8") as fh:
            for line in fh:
                if not line.strip():
                    continue
                row = json.loads(line)
                fp = fingerprint(row, dedupe_keys)
                if fp in seen:
                    continue
                seen.add(fp)
                if filter_eval.eval(row) is not True:
                    continue
                wstart = int(parse_time(window_eval.eval(row)))
                wend = wstart + plan.window.size_seconds
                if wend <= watermark:
                    late += 1
                    continue
                key = (str(row["vehicle_id"]), wstart)
                acc = states.setdefault(key, new_acc())
                add_event(acc, plan.aggregates, row)
        for key in list(states):
            wstart = key[1]
            if wstart + plan.window.size_seconds <= watermark and key not in completed:
                merged = merge_accs([states[key]], plan.aggregates)
                completed[key] = project_result(
                    merged, plan.aggregates, key[0], wstart, wstart + plan.window.size_seconds
                )
    for key, acc in states.items():
        if key not in completed:
            merged = merge_accs([acc], plan.aggregates)
            completed[key] = project_result(
                merged, plan.aggregates, key[0], key[1], key[1] + plan.window.size_seconds
            )
    return completed, late


def load_results(db_path: Path, job_id: str) -> dict[tuple[str, int], dict[str, Any]]:
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute(
            "SELECT vehicle_id, window_start_epoch, result_json FROM window_results "
            "WHERE job_id=? AND is_final=1",
            (job_id,),
        ).fetchall()
    finally:
        conn.close()
    return {
        (str(r["vehicle_id"]), int(r["window_start_epoch"])): json.loads(r["result_json"])
        for r in rows
    }


def assert_results_equal(
    case,
    expected: dict[tuple[str, int], dict[str, Any]],
    actual: dict[tuple[str, int], dict[str, Any]],
) -> None:
    case.assertEqual(set(expected), set(actual))
    for key in expected:
        for field, value in expected[key].items():
            got = actual[key].get(field)
            if isinstance(value, float):
                case.assertAlmostEqual(got, value, places=6, msg=f"{key} {field}")
            else:
                case.assertEqual(got, value, f"{key} {field}")

