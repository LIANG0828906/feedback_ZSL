"""Small serializable value objects used across the application."""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import numpy as np


@dataclass
class Sample:
    sample_id: str
    scene: str
    directory: Path
    manifest: dict[str, Any]
    image: np.ndarray
    truth_mask: np.ndarray

    @property
    def width(self) -> int:
        return int(self.image.shape[1])

    @property
    def height(self) -> int:
        return int(self.image.shape[0])

    @property
    def channels(self) -> int:
        return int(self.image.shape[2])

    @property
    def pixel_count(self) -> int:
        return self.width * self.height

    @property
    def region_count(self) -> int:
        return int(self.manifest["region_count"])

    @property
    def recommended_params(self) -> dict[str, Any]:
        return dict(self.manifest.get("recommended_params", {}))


@dataclass
class RegionStat:
    region_id: int
    label: int
    area: int
    centroid_x: float
    centroid_y: float
    bbox_x: int
    bbox_y: int
    bbox_w: int
    bbox_h: int
    contour: list[list[int]] = field(default_factory=list)

    def to_dict(self, include_contour: bool = True) -> dict[str, Any]:
        data = asdict(self)
        if not include_contour:
            data.pop("contour", None)
        return data


@dataclass
class RegionComparison:
    truth_region_id: int
    predicted_region_id: int
    centroid_error_px: float
    area_relative_error: float
    bbox_iou: float
    mask_iou: float
    truth_area: int
    predicted_area: int

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class ComparisonReport:
    region_count_truth: int
    region_count_predicted: int
    matched_count: int
    mean_centroid_error_px: float
    mean_area_relative_error: float
    mean_bbox_iou: float
    mean_mask_iou: float
    regions: list[RegionComparison]
    tolerance: dict[str, float] | None = None
    passed: bool | None = None

    def to_dict(self) -> dict[str, Any]:
        payload = asdict(self)
        payload["regions"] = [item.to_dict() for item in self.regions]
        return payload


@dataclass
class ModeDecision:
    mode: str
    downsample_factor: int
    block_size: int
    pixel_count: int
    region_count: int
    estimated_fine_seconds: float
    estimated_fine_mb: float
    estimated_seconds: float
    estimated_mb: float
    reason: str
    degradation_reason: str | None
    impact_scope: list[str]
    considered: list[dict[str, Any]]
    budget: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class ScaleViolation:
    region_id: int
    centroid_error_px: float
    area_relative_error: float
    bbox_iou: float

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class ScaleReport:
    factor: int
    tolerance: dict[str, float]
    regions: list[dict[str, Any]]
    violations: list[ScaleViolation]
    passed: bool
    max_centroid_error_px: float
    max_area_relative_error: float
    min_bbox_iou: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "factor": self.factor,
            "tolerance": self.tolerance,
            "regions": self.regions,
            "violations": [item.to_dict() for item in self.violations],
            "passed": self.passed,
            "max_centroid_error_px": self.max_centroid_error_px,
            "max_area_relative_error": self.max_area_relative_error,
            "min_bbox_iou": self.min_bbox_iou,
        }
