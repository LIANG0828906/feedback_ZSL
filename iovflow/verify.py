from __future__ import annotations

import json
import math
import sqlite3
from pathlib import Path
from typing import Any

from .errors import EngineError
from .storage import open_results, open_state


def _load_final(results_db: str, job_id: str) -> dict[tuple[str, int], dict[str, Any]]:
    conn = open_results(results_db)
    try:
        rows = conn.execute(
            "SELECT * FROM window_results WHERE job_id=? AND is_final=1",
            (job_id,),
        ).fetchall()
    finally:
        conn.close()
    out: dict[tuple[str, int], dict[str, Any]] = {}
    for row in rows:
        key = (str(row["vehicle_id"]), int(row["window_start_epoch"]))
        if key in out:
            raise EngineError(
                "IOV_STATE_CORRUPT",
                f"duplicate final window row for {key} in job {job_id}",
            )
        out[key] = json.loads(row["result_json"])
    return out


def _duplicate_final_versions(results_db: str, job_id: str) -> int:
    conn = open_results(results_db)
    try:
        return int(
            conn.execute(
                "SELECT COUNT(*) FROM ("
                " SELECT vehicle_id, window_start_epoch, COUNT(*) c"
                " FROM result_versions WHERE job_id=? AND is_final=1"
                " GROUP BY vehicle_id, window_start_epoch HAVING c>1)",
                (job_id,),
            ).fetchone()[0]
        )
    finally:
        conn.close()


def verify_jobs(
    job_a: str,
    job_b: str,
    results_db: str,
    state_db: str | None = None,
    tolerance: float = 1e-6,
) -> dict[str, Any]:
    a = _load_final(results_db, job_a)
    b = _load_final(results_db, job_b)
    keys_a, keys_b = set(a), set(b)
    missing = sorted(keys_b - keys_a)
    extra = sorted(keys_a - keys_b)
    mismatches: list[dict[str, Any]] = []
    for key in sorted(keys_a & keys_b):
        diffs = _diff(a[key], b[key], tolerance)
        if diffs:
            mismatches.append({"vehicle_id": key[0], "window_start_epoch": key[1], "fields": diffs})
    out: dict[str, Any] = {
        "ok": not (missing or extra or mismatches),
        "job_a": job_a,
        "job_b": job_b,
        "windows_a": len(a),
        "windows_b": len(b),
        "missing_in_a": len(missing),
        "extra_in_a": len(extra),
        "mismatches": len(mismatches),
        "sample_missing": missing[:10],
        "sample_extra": extra[:10],
        "sample_mismatches": mismatches[:10],
        "duplicate_final_versions_a": _duplicate_final_versions(results_db, job_a),
        "duplicate_final_versions_b": _duplicate_final_versions(results_db, job_b),
    }
    if out["duplicate_final_versions_a"] or out["duplicate_final_versions_b"]:
        out["ok"] = False
    if state_db:
        sa = open_state(state_db)
        try:
            wa = sa.execute(
                "SELECT batch_id, watermark FROM watermark_history WHERE job_id=? ORDER BY batch_id",
                (job_a,),
            ).fetchall()
            wb = sa.execute(
                "SELECT batch_id, watermark FROM watermark_history WHERE job_id=? ORDER BY batch_id",
                (job_b,),
            ).fetchall()
            out["watermarks_equal"] = [(int(r["batch_id"]), int(r["watermark"])) for r in wa] == [
                (int(r["batch_id"]), int(r["watermark"])) for r in wb
            ]
            out["job_a_status"] = sa.execute(
                "SELECT status FROM jobs WHERE job_id=?", (job_a,)
            ).fetchone()["status"]
            out["job_b_status"] = sa.execute(
                "SELECT status FROM jobs WHERE job_id=?", (job_b,)
            ).fetchone()["status"]
            out["takeovers_a"] = int(
                sa.execute(
                    "SELECT COALESCE(SUM(takeover_count),0) FROM stage_shards WHERE job_id=?",
                    (job_a,),
                ).fetchone()[0]
            )
            out["takeovers_b"] = int(
                sa.execute(
                    "SELECT COALESCE(SUM(takeover_count),0) FROM stage_shards WHERE job_id=?",
                    (job_b,),
                ).fetchone()[0]
            )
            late_a = {
                (str(r["vehicle_id"]), int(r["window_start"])): int(r["late_count"])
                for r in sa.execute(
                    "SELECT vehicle_id, window_start, late_count FROM late_windows WHERE job_id=?",
                    (job_a,),
                ).fetchall()
            }
            late_b = {
                (str(r["vehicle_id"]), int(r["window_start"])): int(r["late_count"])
                for r in sa.execute(
                    "SELECT vehicle_id, window_start, late_count FROM late_windows WHERE job_id=?",
                    (job_b,),
                ).fetchall()
            }
            out["late_counts_equal"] = late_a == late_b
            if late_a != late_b:
                out["ok"] = False
        finally:
            sa.close()
    return out


def _diff(a: Any, b: Any, tolerance: float, path: str = "") -> list[dict[str, Any]]:
    if isinstance(a, dict) and isinstance(b, dict):
        out: list[dict[str, Any]] = []
        for key in sorted(set(a) | set(b)):
            p = f"{path}.{key}" if path else key
            if key not in a:
                out.append({"path": p, "a": None, "b": b[key]})
            elif key not in b:
                out.append({"path": p, "a": a[key], "b": None})
            else:
                out.extend(_diff(a[key], b[key], tolerance, p))
        return out
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        if not math.isclose(float(a), float(b), rel_tol=tolerance, abs_tol=tolerance):
            return [{"path": path, "a": a, "b": b}]
        return []
    if a != b:
        return [{"path": path, "a": a, "b": b}]
    return []


def query_vehicle(
    job_id: str,
    vehicle_id: str,
    results_db: str,
    final_only: bool = True,
) -> dict[str, Any]:
    conn = open_results(results_db)
    try:
        sql = "SELECT * FROM window_results WHERE job_id=? AND vehicle_id=?"
        params: list[Any] = [job_id, vehicle_id]
        if final_only:
            sql += " AND is_final=1"
        sql += " ORDER BY window_start_epoch"
        rows = conn.execute(sql, params).fetchall()
        late = conn.execute(
            "SELECT * FROM late_windows WHERE job_id=? AND vehicle_id=? "
            "ORDER BY window_start",
            (job_id, vehicle_id),
        ).fetchall()
    finally:
        conn.close()
    return {
        "job_id": job_id,
        "vehicle_id": vehicle_id,
        "windows": [json.loads(r["result_json"]) | {"is_final": bool(r["is_final"])} for r in rows],
        "late_windows": [dict(r) for r in late],
    }

