"""Exact output comparison and first-divergence localization."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .io_utils import atomic_write_text, read_json, write_json


ARRAY_FIELDS = (
    "synthetic_rgb",
    "fused_radiance",
    "cleaned_detail",
    "enhanced_detail",
    "chroma_xyz",
    "linear_rgb",
    "gamut_mask",
    "boundary_rgb",
    "gamut_excess",
)


def _load_stats(directory: Path) -> tuple[dict[str, Any], tuple[int, int]]:
    stats = read_json(directory / "stats.json", field="stats")
    segment_size = tuple(int(value) for value in stats["segment_size"])
    return stats, segment_size  # type: ignore[return-value]


def _build_segment_mapping(height: int, width: int, segment_size: tuple[int, int]) -> tuple[int, int, int]:
    seg_h, seg_w = segment_size
    cols = (width + seg_w - 1) // seg_w
    rows = (height + seg_h - 1) // seg_h
    return rows, cols, seg_h


def _segment_index(y: int, x: int, *, width: int, segment_size: tuple[int, int]) -> int:
    _rows, cols, seg_h = _build_segment_mapping(2**31, width, segment_size)
    seg_w = segment_size[1]
    return (y // seg_h) * cols + (x // seg_w)


def _first_array_mismatch(
    left: np.ndarray,
    right: np.ndarray,
    *,
    field: str,
    segment_size: tuple[int, int],
) -> dict[str, Any] | None:
    if left.shape != right.shape or left.dtype != right.dtype:
        if left.size == 0 or right.size == 0:
            y = x = channel = 0
        else:
            y = x = channel = 0
        return {
            "field": field,
            "coordinate": [y, x],
            "channel": channel,
            "segment_index": _segment_index(y, x, width=left.shape[1] if left.ndim >= 2 else 1, segment_size=segment_size),
            "left_shape": list(left.shape),
            "right_shape": list(right.shape),
            "left_dtype": str(left.dtype),
            "right_dtype": str(right.dtype),
        }
    if left.ndim == 0:
        if left.item() == right.item():
            return None
        return {"field": field, "coordinate": None, "channel": None, "segment_index": None}
    mismatch = left != right
    if not np.any(mismatch):
        return None
    flat_index = int(np.argmax(mismatch.reshape(-1)))
    if left.ndim == 2:
        height, width = left.shape
        y = flat_index // width
        x = flat_index % width
        channel = None
    elif left.ndim == 3:
        height, width, channels = left.shape
        y = flat_index // (width * channels)
        x = (flat_index // channels) % width
        channel = flat_index % channels
    else:
        y = x = channel = None
    segment_index = None
    if y is not None and x is not None:
        segment_index = _segment_index(y, x, width=left.shape[1], segment_size=segment_size)
    return {
        "field": field,
        "coordinate": [int(y), int(x)] if y is not None and x is not None else None,
        "channel": int(channel) if channel is not None else None,
        "segment_index": segment_index,
        "left_value": left.reshape(-1)[flat_index].item(),
        "right_value": right.reshape(-1)[flat_index].item(),
    }


def compare_outputs(
    left_dir: Path,
    right_dir: Path,
    *,
    report_path: Path | None = None,
    raise_on_diverge: bool = True,
) -> dict[str, Any]:
    left_dir = Path(left_dir)
    right_dir = Path(right_dir)
    left_stats, left_segment_size = _load_stats(left_dir)
    right_stats, right_segment_size = _load_stats(right_dir)
    if left_segment_size != right_segment_size:
        report: dict[str, Any] = {
            "ok": False,
            "code": "E_DIVERGE",
            "message": "Segment sizes differ between outputs",
            "left": str(left_dir),
            "right": str(right_dir),
            "first_mismatch": {
                "field": "stats.segment_size",
                "coordinate": None,
                "segment_index": None,
                "left": list(left_segment_size),
                "right": list(right_segment_size),
            },
            "checked_fields": [],
        }
        _write_comparison(report, report_path, left_dir, right_dir)
        if raise_on_diverge:
            raise WorkbenchError("E_DIVERGE", report["message"], field="segment_size", details=report)
        return report

    first_mismatch: dict[str, Any] | None = None
    checked: list[str] = []
    for field in ARRAY_FIELDS:
        left_path = left_dir / f"{field}.npy"
        right_path = right_dir / f"{field}.npy"
        if not left_path.exists() or not right_path.exists():
            first_mismatch = {
                "field": field,
                "coordinate": [0, 0],
                "segment_index": 0,
                "missing_left": not left_path.exists(),
                "missing_right": not right_path.exists(),
            }
            break
        left = np.load(left_path, allow_pickle=False)
        right = np.load(right_path, allow_pickle=False)
        mismatch = _first_array_mismatch(
            left,
            right,
            field=field,
            segment_size=left_segment_size,
        )
        checked.append(field)
        if mismatch is not None:
            first_mismatch = mismatch
            break

    stats_checks = (
        ("output_bit_depth", left_stats.get("output_bit_depth"), right_stats.get("output_bit_depth")),
        (
            "filtering_quality.residual_std",
            left_stats.get("filtering_quality", {}).get("residual_std"),
            right_stats.get("filtering_quality", {}).get("residual_std"),
        ),
        (
            "filtering_quality.edge_fidelity",
            left_stats.get("filtering_quality", {}).get("edge_fidelity"),
            right_stats.get("filtering_quality", {}).get("edge_fidelity"),
        ),
        (
            "gamut.out_of_gamut_count",
            left_stats.get("gamut", {}).get("out_of_gamut_count"),
            right_stats.get("gamut", {}).get("out_of_gamut_count"),
        ),
        (
            "gamut.max_excess",
            left_stats.get("gamut", {}).get("max_excess"),
            right_stats.get("gamut", {}).get("max_excess"),
        ),
    )
    if first_mismatch is None:
        for field, left_value, right_value in stats_checks:
            checked.append(f"stats.{field}")
            if left_value != right_value:
                first_mismatch = {
                    "field": f"stats.{field}",
                    "coordinate": None,
                    "segment_index": None,
                    "left_value": left_value,
                    "right_value": right_value,
                }
                break

    ok = first_mismatch is None
    report = {
        "ok": ok,
        "code": None if ok else "E_DIVERGE",
        "message": "Outputs are exactly equal" if ok else "Outputs diverge",
        "left": str(left_dir),
        "right": str(right_dir),
        "segment_size": list(left_segment_size),
        "first_mismatch": first_mismatch,
        "checked_fields": checked,
        "stats_fields": [field for field, _left, _right in stats_checks],
    }
    _write_comparison(report, report_path, left_dir, right_dir)
    if not ok and raise_on_diverge:
        raise WorkbenchError("E_DIVERGE", "Outputs diverge", field=first_mismatch["field"], details=report)
    return report


def _write_comparison(
    report: dict[str, Any],
    report_path: Path | None,
    left_dir: Path,
    right_dir: Path,
) -> None:
    if report_path is None:
        report_path = left_dir.parent / f"comparison_{left_dir.name}_vs_{right_dir.name}.json"
    report_path = Path(report_path)
    report["result_file"] = str(report_path.resolve())
    reproduce_path = report_path.with_suffix(".reproduce.sh")
    root = Path.cwd()
    command = (
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        f"cd {json.dumps(str(root.resolve()))}\n"
        ".venv/bin/python -m mshdr doctor\n"
        ".venv/bin/python -m mshdr generate --seed 20261123 --out samples\n"
        ".venv/bin/python -m mshdr run-paths --dataset samples/dataset.json --out outputs/paths --checkpoint-root checkpoints/paths\n"
        f".venv/bin/python -m mshdr compare --left {json.dumps(str(left_dir.resolve()))} --right {json.dumps(str(right_dir.resolve()))} --report {json.dumps(str(report_path.resolve()))}\n"
        f"cat {json.dumps(str(report_path.resolve()))}\n"
    )
    report["reproduce_script"] = str(reproduce_path.resolve())
    write_json(report_path, report)
    atomic_write_text(reproduce_path, command)
    os.chmod(reproduce_path, 0o755)

