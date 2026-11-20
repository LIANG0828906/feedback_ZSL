"""Configuration model for deterministic raster and atlas runs."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_CONFIG_INVALID, fail
from .font import FILL_RULES
from .raster import SamplingSpec


@dataclass(frozen=True)
class BudgetConfig:
    max_task_sampling_points: int | None = None
    max_task_memory_bytes: int | None = None
    max_total_sampling_points: int | None = None
    max_total_memory_bytes: int | None = None

    def __post_init__(self) -> None:
        for name in (
            "max_task_sampling_points",
            "max_task_memory_bytes",
            "max_total_sampling_points",
            "max_total_memory_bytes",
        ):
            value = getattr(self, name)
            if value is not None and (isinstance(value, bool) or not isinstance(value, int) or value < 0):
                fail(E_CONFIG_INVALID, f"{name} must be a non-negative integer or null", field=name)

    def as_dict(self) -> dict[str, int | None]:
        return {
            "max_task_sampling_points": self.max_task_sampling_points,
            "max_task_memory_bytes": self.max_task_memory_bytes,
            "max_total_sampling_points": self.max_total_sampling_points,
            "max_total_memory_bytes": self.max_total_memory_bytes,
        }


@dataclass(frozen=True)
class TargetConfig:
    name: str
    pixel_size: float
    subpixel_x: float = 0.0
    subpixel_y: float = 0.0

    def __post_init__(self) -> None:
        if not self.name:
            fail(E_CONFIG_INVALID, "Target name cannot be empty")
        if self.name in {".", ".."} or "/" in self.name or "\\" in self.name:
            fail(E_CONFIG_INVALID, "Target name must not contain path separators", target=self.name)
        if not math.isfinite(self.pixel_size) or self.pixel_size <= 0:
            fail(E_CONFIG_INVALID, "Target pixel_size must be positive", target=self.name)
        if not math.isfinite(self.subpixel_x) or not math.isfinite(self.subpixel_y):
            fail(E_CONFIG_INVALID, "Target subpixel offsets must be finite", target=self.name)

    def as_dict(self) -> dict[str, object]:
        return {
            "name": self.name,
            "pixel_size": self.pixel_size,
            "subpixel_x": self.subpixel_x,
            "subpixel_y": self.subpixel_y,
        }


@dataclass(frozen=True)
class AtlasConfig:
    width: int
    height: int
    padding: int = 1

    def __post_init__(self) -> None:
        for name, value, minimum in (
            ("width", self.width, 1),
            ("height", self.height, 1),
            ("padding", self.padding, 0),
        ):
            if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
                fail(E_CONFIG_INVALID, f"Atlas {name} must be an integer >= {minimum}")

    def as_dict(self) -> dict[str, int]:
        return {"width": self.width, "height": self.height, "padding": self.padding}


@dataclass(frozen=True)
class WorkConfig:
    font_path: Path
    glyphs: tuple[str, ...] | None
    targets: tuple[TargetConfig, ...]
    tile_width: int
    tile_height: int
    parallelism: int
    sampling: SamplingSpec
    fill_rule: str
    budget: BudgetConfig
    atlas: AtlasConfig
    coverage_tolerance: float
    output_dir: Path
    snapshot_dir: Path
    temp_dir: Path
    config_path: Path

    def __post_init__(self) -> None:
        if self.tile_width < 1 or self.tile_height < 1:
            fail(E_CONFIG_INVALID, "Tile dimensions must be positive")
        if self.parallelism < 1:
            fail(E_CONFIG_INVALID, "parallelism must be at least one")
        if self.fill_rule not in FILL_RULES:
            fail(E_CONFIG_INVALID, "fill_rule must be nonzero or evenodd", fill_rule=self.fill_rule)
        if not math.isfinite(self.coverage_tolerance) or self.coverage_tolerance < 0:
            fail(E_CONFIG_INVALID, "coverage_tolerance must be a non-negative finite number")
        if not self.targets:
            fail(E_CONFIG_INVALID, "At least one target is required")
        names = [target.name for target in self.targets]
        if len(names) != len(set(names)):
            fail(E_CONFIG_INVALID, "Target names must be unique")

    def target(self, name: str) -> TargetConfig:
        for target in self.targets:
            if target.name == name:
                return target
        fail(E_CONFIG_INVALID, "Unknown target", target=name)
        raise AssertionError("unreachable")

    def as_dict(self) -> dict[str, object]:
        return {
            "font": str(self.font_path),
            "glyphs": list(self.glyphs) if self.glyphs is not None else None,
            "targets": [target.as_dict() for target in self.targets],
            "tile": {"width": self.tile_width, "height": self.tile_height},
            "parallelism": self.parallelism,
            "samples": {"x": self.sampling.samples_x, "y": self.sampling.samples_y},
            "quantization_step": self.sampling.quantization_step,
            "fill_rule": self.fill_rule,
            "budget": self.budget.as_dict(),
            "atlas": self.atlas.as_dict(),
            "coverage_tolerance": self.coverage_tolerance,
            "output_dir": str(self.output_dir),
            "snapshot_dir": str(self.snapshot_dir),
            "temp_dir": str(self.temp_dir),
        }


def load_config(path: str | Path) -> WorkConfig:
    config_path = Path(path).resolve()
    try:
        document = json.loads(config_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CONFIG_INVALID, "Cannot read configuration", path=str(config_path), error=str(exc))
    return parse_config(document, config_path)


def parse_config(document: dict[str, Any], config_path: str | Path) -> WorkConfig:
    if not isinstance(document, dict):
        fail(E_CONFIG_INVALID, "Configuration root must be an object")
    config_path = Path(config_path).resolve()
    base = config_path.parent
    font_value = document.get("font")
    if not isinstance(font_value, str) or not font_value:
        fail(E_CONFIG_INVALID, "Configuration requires a local font path")
    font_path = _resolve_path(base, font_value)

    raw_glyphs = document.get("glyphs")
    glyphs: tuple[str, ...] | None
    if raw_glyphs is None or raw_glyphs == "all":
        glyphs = None
    elif isinstance(raw_glyphs, list) and all(isinstance(item, str) and item for item in raw_glyphs):
        glyphs = tuple(raw_glyphs)
    else:
        fail(E_CONFIG_INVALID, "glyphs must be 'all' or an array of glyph names")

    raw_targets = document.get("targets")
    if raw_targets is None:
        raw_targets = [{"name": "default", "pixel_size": 16}]
    if not isinstance(raw_targets, list) or not raw_targets:
        fail(E_CONFIG_INVALID, "targets must be a non-empty array")
    targets = tuple(_parse_target(raw_target, index) for index, raw_target in enumerate(raw_targets))

    tile = document.get("tile", document.get("tile_size", {}))
    if isinstance(tile, list) and len(tile) == 2:
        tile = {"width": tile[0], "height": tile[1]}
    if not isinstance(tile, dict):
        fail(E_CONFIG_INVALID, "tile must be an object or [width, height]")
    tile_width = _positive_int(tile.get("width", 16), "tile.width")
    tile_height = _positive_int(tile.get("height", 16), "tile.height")
    parallelism = _positive_int(document.get("parallelism", 1), "parallelism")

    samples = document.get("samples", {})
    if not isinstance(samples, dict):
        fail(E_CONFIG_INVALID, "samples must be an object")
    sample_x = _positive_int(samples.get("x", samples.get("samples_x", 4)), "samples.x")
    sample_y = _positive_int(samples.get("y", samples.get("samples_y", 4)), "samples.y")
    quantization_step = _positive_int(document.get("quantization_step", 1), "quantization_step")
    if quantization_step > 255:
        fail(E_CONFIG_INVALID, "quantization_step must be <= 255")

    fill_rule = str(document.get("fill_rule", "nonzero"))
    if fill_rule not in FILL_RULES:
        fail(E_CONFIG_INVALID, "fill_rule must be nonzero or evenodd", fill_rule=fill_rule)

    raw_budget = document.get("budget", {})
    if not isinstance(raw_budget, dict):
        fail(E_CONFIG_INVALID, "budget must be an object")
    budget = BudgetConfig(
        max_task_sampling_points=_optional_int(
            raw_budget.get("max_task_sampling_points", raw_budget.get("max_sampling_points")),
            "budget.max_task_sampling_points",
        ),
        max_task_memory_bytes=_optional_int(
            raw_budget.get("max_task_memory_bytes", raw_budget.get("max_memory_bytes")),
            "budget.max_task_memory_bytes",
        ),
        max_total_sampling_points=_optional_int(
            raw_budget.get("max_total_sampling_points"),
            "budget.max_total_sampling_points",
        ),
        max_total_memory_bytes=_optional_int(
            raw_budget.get("max_total_memory_bytes"),
            "budget.max_total_memory_bytes",
        ),
    )

    raw_atlas = document.get("atlas", {})
    if not isinstance(raw_atlas, dict):
        fail(E_CONFIG_INVALID, "atlas must be an object")
    atlas = AtlasConfig(
        width=_positive_int(raw_atlas.get("width", 256), "atlas.width"),
        height=_positive_int(raw_atlas.get("height", 256), "atlas.height"),
        padding=_optional_int(raw_atlas.get("padding", 1), "atlas.padding") or 0,
    )

    tolerance_value = document.get("coverage_tolerance", 0)
    if isinstance(tolerance_value, bool) or not isinstance(tolerance_value, (int, float)):
        fail(E_CONFIG_INVALID, "coverage_tolerance must be numeric")
    coverage_tolerance = float(tolerance_value)
    if not math.isfinite(coverage_tolerance) or coverage_tolerance < 0:
        fail(E_CONFIG_INVALID, "coverage_tolerance must be non-negative")

    output_dir = _resolve_path(base, str(document.get("output_dir", "out")))
    snapshot_dir = _resolve_path(base, str(document.get("snapshot_dir", "snapshots")))
    temp_dir = _resolve_path(base, str(document.get("temp_dir", ".tmp")))
    return WorkConfig(
        font_path=font_path,
        glyphs=glyphs,
        targets=targets,
        tile_width=tile_width,
        tile_height=tile_height,
        parallelism=parallelism,
        sampling=SamplingSpec(sample_x, sample_y, quantization_step),
        fill_rule=fill_rule,
        budget=budget,
        atlas=atlas,
        coverage_tolerance=coverage_tolerance,
        output_dir=output_dir,
        snapshot_dir=snapshot_dir,
        temp_dir=temp_dir,
        config_path=config_path,
    )


def _parse_target(raw_target: Any, index: int) -> TargetConfig:
    if isinstance(raw_target, (int, float)) and not isinstance(raw_target, bool):
        return TargetConfig(name=f"target-{index}", pixel_size=float(raw_target))
    if not isinstance(raw_target, dict):
        fail(E_CONFIG_INVALID, "Target must be a number or object", target_index=index)
    name = str(raw_target.get("name", raw_target.get("id", f"target-{index}")))
    size = raw_target.get("pixel_size", raw_target.get("size"))
    if not isinstance(size, (int, float)) or isinstance(size, bool):
        fail(E_CONFIG_INVALID, "Target pixel_size is required", target=name)
    return TargetConfig(
        name=name,
        pixel_size=float(size),
        subpixel_x=float(raw_target.get("subpixel_x", 0.0)),
        subpixel_y=float(raw_target.get("subpixel_y", 0.0)),
    )


def _resolve_path(base: Path, value: str) -> Path:
    path = Path(value)
    return path if path.is_absolute() else (base / path).resolve()


def _positive_int(value: Any, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        fail(E_CONFIG_INVALID, f"{field} must be a positive integer")
    return value


def _optional_int(value: Any, field: str) -> int | None:
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        fail(E_CONFIG_INVALID, f"{field} must be a non-negative integer or null")
    return value

