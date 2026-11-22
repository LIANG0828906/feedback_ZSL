"""Configuration, versioning and validation for the segmentation chain."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass, replace
from typing import Any

from .errors import BusinessError

ALGORITHM_VERSION = "seg-1.0.0"
SAMPLE_FORMAT_VERSION = 1
CHECKPOINT_FORMAT_VERSION = 1


@dataclass(frozen=True)
class SegmentationParams:
    """All parameters that influence masks or post-processing."""

    color_tolerance: float = 30.0
    gradient_threshold: float = 80.0
    confidence_threshold: float = 0.02
    morph_size: int = 3
    min_area: int = 12
    auto_seeds: bool = True
    seeds: tuple[tuple[int, int], ...] = ()
    expected_regions: int | None = None

    def validate(self, image_shape: tuple[int, int] | None = None) -> None:
        if not math.isfinite(self.color_tolerance) or not (
            0.1 <= self.color_tolerance <= 441.673
        ):
            raise BusinessError(
                "E_PARA",
                "颜色容差必须位于 0.1 到 441.673 之间",
                field="color_tolerance",
                details={"actual": self.color_tolerance},
            )
        if not math.isfinite(self.gradient_threshold) or not (
            0.0 <= self.gradient_threshold <= 1024.0
        ):
            raise BusinessError(
                "E_PARA",
                "梯度阈值必须位于 0 到 1024 之间",
                field="gradient_threshold",
                details={"actual": self.gradient_threshold},
            )
        if not math.isfinite(self.confidence_threshold) or not (
            0.0 <= self.confidence_threshold <= 1.0
        ):
            raise BusinessError(
                "E_PARA",
                "置信度阈值必须位于 0 到 1 之间",
                field="confidence_threshold",
                details={"actual": self.confidence_threshold},
            )
        if self.morph_size < 1 or self.morph_size > 31 or self.morph_size % 2 == 0:
            raise BusinessError(
                "E_PARA",
                "结构元素尺寸必须是 1 到 31 之间的奇数",
                field="morph_size",
                details={"actual": self.morph_size},
            )
        if self.min_area < 0:
            raise BusinessError(
                "E_PARA",
                "最小区域面积不能为负数",
                field="min_area",
                details={"actual": self.min_area},
            )
        if self.expected_regions is not None and self.expected_regions < 1:
            raise BusinessError(
                "E_PARA",
                "预期区域数量必须大于 0",
                field="expected_regions",
                details={"actual": self.expected_regions},
            )
        if not self.auto_seeds and not self.seeds:
            raise BusinessError(
                "E_PARA",
                "关闭自动种子时必须至少提供一个种子点",
                field="seeds",
            )
        if self.seeds and image_shape is not None:
            height, width = image_shape[:2]
            for index, seed in enumerate(self.seeds):
                if (
                    len(seed) != 2
                    or int(seed[0]) < 0
                    or int(seed[1]) < 0
                    or int(seed[0]) >= width
                    or int(seed[1]) >= height
                ):
                    raise BusinessError(
                        "E_PARA",
                        "种子点坐标越界",
                        field=f"seeds[{index}]",
                        details={
                            "actual": list(seed),
                            "width": width,
                            "height": height,
                        },
                    )

    def canonical(self) -> dict[str, Any]:
        data = asdict(self)
        data["seeds"] = [list(item) for item in self.seeds]
        return data

    def version(self) -> str:
        raw = json.dumps(
            {"algorithm": ALGORITHM_VERSION, "params": self.canonical()},
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()[:20]

    def for_sample(
        self, recommended: dict[str, Any], region_count: int
    ) -> "SegmentationParams":
        allowed = {
            "color_tolerance",
            "gradient_threshold",
            "confidence_threshold",
            "morph_size",
            "min_area",
        }
        values = {key: value for key, value in recommended.items() if key in allowed}
        values.setdefault("color_tolerance", self.color_tolerance)
        values.setdefault("gradient_threshold", self.gradient_threshold)
        values.setdefault("confidence_threshold", self.confidence_threshold)
        values.setdefault("morph_size", self.morph_size)
        values.setdefault("min_area", self.min_area)
        return replace(
            self,
            expected_regions=region_count,
            auto_seeds=True,
            seeds=(),
            **values,
        )


@dataclass(frozen=True)
class Budget:
    """Runtime budget used by the deterministic scale strategy."""

    time_seconds: float = 20.0
    memory_mb: float = 512.0
    max_workers: int = 4
    tile_size: int = 256
    block_buffer_mb: float = 4.0

    def validate(self) -> None:
        if not math.isfinite(self.time_seconds) or self.time_seconds <= 0:
            raise BusinessError(
                "E_PARA",
                "时间预算必须大于 0",
                field="budget.time_seconds",
                details={"actual": self.time_seconds},
            )
        if not math.isfinite(self.memory_mb) or self.memory_mb <= 0:
            raise BusinessError(
                "E_PARA",
                "内存预算必须大于 0",
                field="budget.memory_mb",
                details={"actual": self.memory_mb},
            )
        if not 1 <= self.max_workers <= 32:
            raise BusinessError(
                "E_PARA",
                "最大并行度必须位于 1 到 32",
                field="budget.max_workers",
                details={"actual": self.max_workers},
            )
        if not 32 <= self.tile_size <= 2048:
            raise BusinessError(
                "E_PARA",
                "分块尺寸必须位于 32 到 2048",
                field="budget.tile_size",
                details={"actual": self.tile_size},
            )
        if not math.isfinite(self.block_buffer_mb) or not (
            0.1 <= self.block_buffer_mb <= 256.0
        ):
            raise BusinessError(
                "E_PARA",
                "单块缓冲上限必须位于 0.1 到 256 MB",
                field="budget.block_buffer_mb",
                details={"actual": self.block_buffer_mb},
            )

    def version(self) -> str:
        raw = json.dumps(asdict(self), sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(raw).hexdigest()[:20]


@dataclass(frozen=True)
class ScaleTolerance:
    """Declared tolerances for fine-versus-downsampled consistency."""

    centroid_px: float = 2.0
    area_relative: float = 0.12
    bbox_iou_min: float = 0.80

    def validate(self) -> None:
        if self.centroid_px <= 0:
            raise BusinessError(
                "E_PARA",
                "重心容差必须大于 0",
                field="scale_tolerance.centroid_px",
            )
        if not 0 < self.area_relative < 1:
            raise BusinessError(
                "E_PARA",
                "面积相对误差容差必须位于 (0, 1)",
                field="scale_tolerance.area_relative",
            )
        if not 0 < self.bbox_iou_min <= 1:
            raise BusinessError(
                "E_PARA",
                "外接框 IoU 容差必须位于 (0, 1]",
                field="scale_tolerance.bbox_iou_min",
            )

    def to_dict(self) -> dict[str, float]:
        return asdict(self)


@dataclass(frozen=True)
class QualityTolerance:
    """Optional validation envelope against pixel-level truth."""

    centroid_px: float = 8.0
    area_relative: float = 0.25
    bbox_iou_min: float = 0.65
    mask_iou_min: float = 0.55


def canonical_hash(payload: Any) -> str:
    raw = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()
