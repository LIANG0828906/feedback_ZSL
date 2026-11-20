from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .hashing import encode_payload, stable_hash
from .pipeline import ProcessResult


def _json_text(payload: Any) -> str:
    return json.dumps(
        encode_payload(payload),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    )


def write_json(path: str | Path, payload: Any) -> str:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    text = _json_text(payload) + "\n"
    destination.write_text(text, encoding="utf-8")
    return stable_hash(payload)


def write_trace_jsonl(result: ProcessResult, path: str | Path) -> str:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    text = "".join(
        json.dumps(entry, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
        + "\n"
        for entry in result.trace
    )
    destination.write_text(text, encoding="utf-8")
    return stable_hash(result.trace)


def write_filter_coefficients(result: ProcessResult, path: str | Path) -> str:
    payload = {
        "version": result.version,
        "content_hash": result.content_hash,
        "sample_rate": result.measurement["sample_rate"],
        "filters": {
            channel: {
                "coefficients": design["coefficients"],
                "group_delay_samples": design["group_delay_samples"],
                "group_delay_seconds": design["group_delay_seconds"],
                "truncation_db_bound": design["truncation_db_bound"],
            }
            for channel, design in result.filters.items()
        },
    }
    return write_json(path, payload)


def write_diagnostic_bundle(
    result: ProcessResult,
    report_path: str | Path,
    trace_path: str | Path,
    coefficients_path: str | Path | None = None,
) -> dict[str, str]:
    report_payload = result.to_dict(include_trace=True)
    report_hash = write_json(report_path, report_payload)
    trace_hash = write_trace_jsonl(result, trace_path)
    coefficient_hash = ""
    if coefficients_path is not None:
        coefficient_hash = write_filter_coefficients(result, coefficients_path)
    return {
        "report_hash": report_hash,
        "trace_hash": trace_hash,
        "coefficients_hash": coefficient_hash,
    }

