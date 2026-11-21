from __future__ import annotations

import hashlib
import math
from typing import Any

from loudness_diag.jsonutil import sha256_json


def make_block_row(index: int, *, energy: float | None = None) -> dict[str, Any]:
    value = energy if energy is not None else 0.01 * (index + 1)
    start = index * 9600
    end = start + 9600
    return {
        "block_index": index,
        "block_id": f"{index:08d}",
        "window_start": start,
        "window_end": end,
        "sample_count": 9600,
        "complete": True,
        "sync_marker": f"sync:test:{index:08d}:{start}-{end}",
        "content_hash": hashlib.sha256(f"content-{index}".encode()).hexdigest(),
        "ungated_energy": value,
        "short_term_loudness": -0.691 + 10.0 * math.log10(value),
        "true_peak": 0.25,
        "evaluation_hash": hashlib.sha256(f"eval-{index}".encode()).hexdigest(),
    }


def make_report(version: int) -> dict[str, Any]:
    report = {
        "schema_version": 1,
        "asset_id": "versioned",
        "version_label": version,
        "blocks": [make_block_row(index) for index in range(3)],
    }
    report["report_hash"] = sha256_json(report)
    return report

