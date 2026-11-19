"""Deterministic job report writer.

All keys are sorted and only elapsed time / timestamps vary between repeated
runs of the same input; every business field is reproducible.
"""

from __future__ import annotations

import time

from .util import ensure_dir, write_json


def write_report(result_dir: str, name: str, report: dict) -> str:
    ensure_dir(result_dir)
    path = f"{result_dir.rstrip('/')}/{name}.json"
    payload = dict(report)
    payload["generated_at_ms"] = int(time.time() * 1000)
    write_json(path, payload)
    return path


def business_fingerprint(report: dict) -> dict:
    """Fields that must be identical across batch / mixed / resumed runs."""

    return {
        "windows": report["windows"],
        "dedup_dropped": report["dedup_dropped"],
        "late_events": report["late_events"],
        "window_count": report["window_count"],
        "split_plan": report["split_plan"],
    }
