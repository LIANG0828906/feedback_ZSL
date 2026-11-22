"""Reproducible fine-versus-downsampled scale experiments."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .budget import choose_mode
from .config import Budget, QualityTolerance, ScaleTolerance, SegmentationParams
from .errors import BusinessError
from .geometry import compare_masks, scale_consistency
from .models import Sample
from .segmentation import segment_image


@dataclass
class DualModeResult:
    sample_id: str
    width: int
    height: int
    factor: int
    fine_seconds: float
    downsampled_seconds: float
    fine_vs_truth: dict[str, Any]
    downsampled_vs_truth: dict[str, Any]
    scale: dict[str, Any]
    scale_error: dict[str, Any] | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_id": self.sample_id,
            "width": self.width,
            "height": self.height,
            "pixel_count": self.width * self.height,
            "factor": self.factor,
            "fine_seconds": round(self.fine_seconds, 4),
            "downsampled_seconds": round(self.downsampled_seconds, 4),
            "fine_vs_truth": self.fine_vs_truth,
            "downsampled_vs_truth": self.downsampled_vs_truth,
            "scale": self.scale,
            "scale_error": self.scale_error,
            "passed": self.scale_error is None,
        }


def run_dual_mode(
    sample: Sample,
    params: SegmentationParams,
    *,
    factor: int = 2,
    scale_tolerance: ScaleTolerance | None = None,
    quality_tolerance: QualityTolerance | None = None,
) -> DualModeResult:
    scale_tolerance = scale_tolerance or ScaleTolerance()
    params.validate(image_shape=sample.image.shape[:2])
    fine = segment_image(sample.image, params, mode="fine", factor=1)
    downsampled = segment_image(
        sample.image, params, mode="downsampled", factor=factor
    )
    fine_report = compare_masks(
        fine.final_labels,
        sample.truth_mask,
        tolerance=quality_tolerance,
        include_contours=False,
    )
    downsampled_report = compare_masks(
        downsampled.final_labels,
        sample.truth_mask,
        tolerance=quality_tolerance,
        include_contours=False,
    )
    scale_error = None
    try:
        scale_report = scale_consistency(
            fine.final_labels,
            downsampled.final_labels,
            factor,
            scale_tolerance,
        )
        scale_payload = scale_report.to_dict()
    except BusinessError as exc:
        if exc.code != "E_SCALE":
            raise
        scale_error = exc.to_dict()
        scale_payload = {
            "passed": False,
            "error": scale_error,
            "factor": factor,
        }
    return DualModeResult(
        sample_id=sample.sample_id,
        width=sample.width,
        height=sample.height,
        factor=factor,
        fine_seconds=fine.elapsed_seconds,
        downsampled_seconds=downsampled.elapsed_seconds,
        fine_vs_truth=fine_report.to_dict(),
        downsampled_vs_truth=downsampled_report.to_dict(),
        scale=scale_payload,
        scale_error=scale_error,
    )


def run_scale_experiment(
    samples: list[Sample],
    params: SegmentationParams,
    budget: Budget,
    *,
    scale_tolerance: ScaleTolerance | None = None,
    quality_tolerance: QualityTolerance | None = None,
) -> dict[str, Any]:
    scale_tolerance = scale_tolerance or ScaleTolerance()
    ordered = sorted(samples, key=lambda sample: (sample.pixel_count, sample.sample_id))
    results: list[DualModeResult] = []
    for sample in ordered:
        decision = choose_mode(
            sample.width, sample.height, sample.region_count, budget
        )
        factor = decision.downsample_factor if decision.downsample_factor > 1 else 2
        sample_params = params.for_sample(
            sample.recommended_params, sample.region_count
        )
        results.append(
            run_dual_mode(
                sample,
                sample_params,
                factor=factor,
                scale_tolerance=scale_tolerance,
                quality_tolerance=quality_tolerance,
            )
        )
    records = [result.to_dict() for result in results if result.scale_error is None]
    metric_series = {
        "max_centroid_error_px": [
            record["scale"]["max_centroid_error_px"] for record in records
        ],
        "max_area_relative_error": [
            record["scale"]["max_area_relative_error"] for record in records
        ],
        "min_bbox_iou": [record["scale"]["min_bbox_iou"] for record in records],
    }
    tolerance = scale_tolerance.to_dict()
    combined_scores = []
    for record in records:
        scale = record["scale"]
        score = (
            scale["max_centroid_error_px"] / tolerance["centroid_px"]
            + scale["max_area_relative_error"] / tolerance["area_relative"]
            + (1.0 - scale["min_bbox_iou"]) / (1.0 - tolerance["bbox_iou_min"])
        )
        combined_scores.append(float(score))
    monotonic_worsening = [
        metric
        for metric, values in metric_series.items()
        if len(values) >= 2
        and (
            all(b > a + 1e-12 for a, b in zip(values, values[1:]))
            if metric != "min_bbox_iou"
            else all(b < a - 1e-12 for a, b in zip(values, values[1:]))
        )
    ]
    combined_monotonic = len(combined_scores) >= 2 and all(
        b > a + 1e-12 for a, b in zip(combined_scores, combined_scores[1:])
    )
    payload = {
        "experiment": "fine_vs_downsampled_scale_consistency",
        "deterministic": True,
        "ordered_by_pixels": True,
        "tolerance": tolerance,
        "budget": {
            "time_seconds": budget.time_seconds,
            "memory_mb": budget.memory_mb,
            "tile_size": budget.tile_size,
            "max_workers": budget.max_workers,
        },
        "samples": [result.to_dict() for result in results],
        "metric_series": metric_series,
        "combined_deviation_score": combined_scores,
        "monotonic_worsening_metrics": monotonic_worsening,
        "combined_deviation_monotonic_worsening": combined_monotonic,
        "not_monotonically_worsening": not combined_monotonic,
        "all_within_tolerance": all(result.scale_error is None for result in results),
    }
    return payload


def write_experiment_report(payload: dict[str, Any], path: str | Path) -> Path:
    path = Path(path).resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
    return path
