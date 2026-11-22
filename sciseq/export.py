"""Preview deviation validation and atomic result export."""

from __future__ import annotations

import json
import os
import uuid
from pathlib import Path
from typing import Any

import numpy as np

from .config import PipelineConfig
from .engine import SequenceProcessor, downsample_image
from .errors import SciSeqError
from .models import SequenceResult


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, sort_keys=True, allow_nan=False)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _atomic_npy(path: Path, array: np.ndarray) -> None:
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
    with temporary.open("wb") as handle:
        np.save(handle, array, allow_pickle=False)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def compare_preview_to_high_precision(
    preview: SequenceResult,
    high_precision: SequenceResult,
    *,
    mean_abs_limit: float,
) -> dict[str, Any]:
    if preview.frame_count != high_precision.frame_count:
        raise SciSeqError(
            "E_DIVERGE",
            "preview and high-precision frame counts differ",
            field="frame_count",
        )
    if preview.downsample_factor <= 1:
        difference = np.abs(preview.denoised.astype(np.float64) - high_precision.denoised.astype(np.float64))
        selected = np.ones(preview.frame_count, dtype=bool)
    else:
        factor = preview.downsample_factor
        reference_down = np.stack(
            [downsample_image(frame, factor) for frame in high_precision.denoised]
        )
        difference = np.abs(preview.denoised.astype(np.float64) - reference_down.astype(np.float64))
        selected = preview.denoised_mask
    frame_metrics: list[dict[str, Any]] = []
    selected_values: list[np.ndarray] = []
    for index in range(preview.frame_count):
        frame_difference = difference[index]
        frame_metrics.append(
            {
                "frame": index,
                "selected_for_denoise": bool(selected[index]),
                "mean_abs_diff": float(frame_difference.mean()),
                "max_abs_diff": float(frame_difference.max()),
            }
        )
        if selected[index]:
            selected_values.append(frame_difference.ravel())
    selected_difference = (
        np.concatenate(selected_values)
        if selected_values
        else np.zeros(0, dtype=np.float64)
    )
    mean_abs = float(selected_difference.mean()) if selected_difference.size else 0.0
    report = {
        "compared_frames": int(selected.sum()),
        "mean_abs_diff": mean_abs,
        "max_abs_diff": float(selected_difference.max()) if selected_difference.size else 0.0,
        "mismatch_pixels": int(np.count_nonzero(selected_difference > 0.0)),
        "compared_pixels": int(selected_difference.size),
        "mean_abs_limit": float(mean_abs_limit),
        "within_limit": mean_abs <= mean_abs_limit,
        "frames": frame_metrics,
    }
    if not report["within_limit"]:
        raise SciSeqError(
            "E_DIVERGE",
            "preview deviates beyond the declared export limit",
            field="preview_denoised",
            details={
                "mean_abs_diff": mean_abs,
                "mean_abs_limit": mean_abs_limit,
                "max_abs_diff": report["max_abs_diff"],
            },
        )
    return report


def export_result(
    result: SequenceResult,
    output_dir: Path | str,
    *,
    config: PipelineConfig | None = None,
    preview_comparison: dict[str, Any] | None = None,
) -> dict[str, Any]:
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    _atomic_npy(root / "denoised.npy", result.denoised)
    _atomic_npy(root / "offsets.npy", result.offsets)
    feature_payload = [
        [feature.to_dict() for feature in frame_features] for frame_features in result.features
    ]
    _atomic_json(root / "features.json", {"frames": feature_payload})
    _atomic_json(root / "stats.json", result.stats)
    summary = result.summarize()
    summary["export_high_precision_recomputed"] = True
    summary["preview_comparison"] = preview_comparison
    if config is not None:
        summary["config"] = config.decision_payload()
    _atomic_json(root / "summary.json", summary)
    return summary


def export_with_recomputation(
    processor: SequenceProcessor,
    preview: SequenceResult,
    output_dir: Path | str,
    *,
    progress_callback: Any = None,
) -> dict[str, Any]:
    high_precision = processor.run_full(
        parallel=True,
        progress_callback=progress_callback,
    )
    comparison = compare_preview_to_high_precision(
        preview,
        high_precision,
        mean_abs_limit=processor.config.budget.preview_mean_abs_limit,
    )
    return export_result(
        high_precision,
        output_dir,
        config=processor.config,
        preview_comparison=comparison,
    )

