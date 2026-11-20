from __future__ import annotations

import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .atlas import AtlasConfig
from .errors import ErrorCode, GlyphForgeError
from .raster import RasterOptions
from .scheduler import BudgetConfig
from .util import read_json


def _positive_float(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{name} must be a number")
    number = float(value)
    if not math.isfinite(number) or number <= 0:
        raise ValueError(f"{name} must be finite and positive")
    return number


def _finite_float(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{name} must be a number")
    number = float(value)
    if not math.isfinite(number):
        raise ValueError(f"{name} must be finite")
    return number


@dataclass(frozen=True)
class WorkbenchConfig:
    pixel_size: float = 24.0
    subpixel_x: float = 0.0
    subpixel_y: float = 0.0
    supersample: int = 4
    quant_step: float = 1.0 / 255.0
    fill_rule: str = "nonzero"
    tile_size: int = 8
    curve_tolerance: float = 0.05
    coverage_tolerance: int = 0
    budget: BudgetConfig = field(default_factory=BudgetConfig)
    atlas: AtlasConfig = field(default_factory=AtlasConfig)

    @classmethod
    def from_dict(cls, raw: Any) -> WorkbenchConfig:
        if not isinstance(raw, dict):
            raise ValueError("configuration must be a JSON object")
        offset = raw.get("subpixel_offset", [0.0, 0.0])
        if not isinstance(offset, list) or len(offset) != 2:
            raise ValueError("subpixel_offset must contain x and y")
        budget_raw = raw.get("budget", {})
        atlas_raw = raw.get("atlas", {})
        if not isinstance(budget_raw, dict) or not isinstance(atlas_raw, dict):
            raise ValueError("budget and atlas settings must be objects")
        degrade = budget_raw.get(
            "degrade_sequence", ["shrink_batch", "reject"]
        )
        if not isinstance(degrade, list) or not all(
            isinstance(item, str) for item in degrade
        ):
            raise ValueError("degrade_sequence must be an array of strings")
        config = cls(
            pixel_size=_positive_float(raw.get("pixel_size", 24.0), "pixel_size"),
            subpixel_x=_finite_float(offset[0], "subpixel_offset[0]"),
            subpixel_y=_finite_float(offset[1], "subpixel_offset[1]"),
            supersample=int(raw.get("supersample", 4)),
            quant_step=float(raw.get("quant_step", 1.0 / 255.0)),
            fill_rule=str(raw.get("fill_rule", "nonzero")),
            tile_size=int(raw.get("tile_size", 8)),
            curve_tolerance=_positive_float(
                raw.get("curve_tolerance", 0.05), "curve_tolerance"
            ),
            coverage_tolerance=int(raw.get("coverage_tolerance", 0)),
            budget=BudgetConfig(
                parallelism=int(budget_raw.get("parallelism", 4)),
                global_memory_bytes=int(
                    budget_raw.get("global_memory_bytes", 64 * 1024 * 1024)
                ),
                max_task_samples=int(
                    budget_raw.get("max_task_samples", 4_000_000)
                ),
                max_task_memory_bytes=int(
                    budget_raw.get("max_task_memory_bytes", 4 * 1024 * 1024)
                ),
                sampling_batch_size=int(
                    budget_raw.get("sampling_batch_size", 2048)
                ),
                min_sampling_batch_size=int(
                    budget_raw.get("min_sampling_batch_size", 256)
                ),
                degrade_sequence=tuple(degrade),
            ),
            atlas=AtlasConfig(
                width=int(atlas_raw.get("width", 512)),
                height=int(atlas_raw.get("height", 256)),
                padding=int(atlas_raw.get("padding", 2)),
            ),
        )
        config.validate()
        return config

    @classmethod
    def from_file(cls, path: str | Path) -> WorkbenchConfig:
        return cls.from_dict(read_json(path))

    def validate(self) -> None:
        if self.supersample < 1 or self.supersample > 16:
            raise ValueError("supersample must be between 1 and 16")
        if self.tile_size <= 0:
            raise ValueError("tile_size must be positive")
        if self.coverage_tolerance < 0 or self.coverage_tolerance > 255:
            raise ValueError("coverage_tolerance must be between 0 and 255")
        if self.fill_rule not in ("nonzero", "evenodd"):
            raise ValueError("fill_rule must be nonzero or evenodd")
        if not math.isfinite(self.quant_step) or self.quant_step <= 0:
            raise GlyphForgeError(
                ErrorCode.E_QUANT_INVALID,
                "quantization step must be finite and greater than zero",
                {"quant_step": self.quant_step},
            )
        if self.quant_step > 1.0:
            raise GlyphForgeError(
                ErrorCode.E_QUANT_INVALID,
                "quantization step must not exceed one coverage unit",
                {"quant_step": self.quant_step},
            )
        if not math.isfinite(self.subpixel_x) or not math.isfinite(self.subpixel_y):
            raise ValueError("subpixel offsets must be finite")
        if self.pixel_size <= 0:
            raise ValueError("pixel_size must be positive")
        if self.curve_tolerance <= 0:
            raise ValueError("curve_tolerance must be positive")
        self.budget.validate()
        self.atlas.validate()

    def raster_options(self, units_per_em: float) -> RasterOptions:
        options = RasterOptions(
            pixel_size=self.pixel_size,
            units_per_em=units_per_em,
            subpixel_x=self.subpixel_x,
            subpixel_y=self.subpixel_y,
            supersample=self.supersample,
            quant_step=self.quant_step,
            fill_rule=self.fill_rule,
        )
        options.validate()
        return options

    def to_dict(self) -> dict[str, Any]:
        return {
            "pixel_size": self.pixel_size,
            "subpixel_offset": [self.subpixel_x, self.subpixel_y],
            "supersample": self.supersample,
            "quant_step": self.quant_step,
            "fill_rule": self.fill_rule,
            "tile_size": self.tile_size,
            "curve_tolerance": self.curve_tolerance,
            "coverage_tolerance": self.coverage_tolerance,
            "budget": {
                "parallelism": self.budget.parallelism,
                "global_memory_bytes": self.budget.global_memory_bytes,
                "max_task_samples": self.budget.max_task_samples,
                "max_task_memory_bytes": self.budget.max_task_memory_bytes,
                "sampling_batch_size": self.budget.sampling_batch_size,
                "min_sampling_batch_size": self.budget.min_sampling_batch_size,
                "degrade_sequence": list(self.budget.degrade_sequence),
            },
            "atlas": {
                "width": self.atlas.width,
                "height": self.atlas.height,
                "padding": self.atlas.padding,
            },
        }
