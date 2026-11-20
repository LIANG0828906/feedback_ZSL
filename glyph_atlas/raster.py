"""Shared anti-aliased scan converter used by whole and tiled rendering."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass

import numpy as np

from .errors import E_CONFIG_INVALID, fail
from .font import FILL_RULES, ResolvedGlyph
from .geometry import LineContour, Point, bounds


@dataclass(frozen=True)
class SamplingSpec:
    samples_x: int = 4
    samples_y: int = 4
    quantization_step: int = 1

    def __post_init__(self) -> None:
        for name, value in (
            ("samples_x", self.samples_x),
            ("samples_y", self.samples_y),
            ("quantization_step", self.quantization_step),
        ):
            if isinstance(value, bool) or not isinstance(value, int):
                fail(E_CONFIG_INVALID, f"{name} must be an integer", field=name)
        if self.samples_x < 1 or self.samples_y < 1:
            fail(E_CONFIG_INVALID, "Subpixel sample counts must be at least one")
        if self.quantization_step < 1 or self.quantization_step > 255:
            fail(E_CONFIG_INVALID, "quantization_step must be in [1, 255]")


@dataclass(frozen=True)
class PixelBox:
    x: int
    y: int
    width: int
    height: int

    def __post_init__(self) -> None:
        if self.x < 0 or self.y < 0 or self.width < 0 or self.height < 0:
            fail(E_CONFIG_INVALID, "Pixel box coordinates and dimensions must be non-negative")

    @property
    def area(self) -> int:
        return self.width * self.height


@dataclass(frozen=True)
class GlyphRenderPlan:
    glyph: ResolvedGlyph
    pixel_size: float
    subpixel_x: float
    subpixel_y: float
    scale: float
    contours: tuple[tuple[Point, ...], ...]
    bbox: tuple[float, float, float, float]
    origin_x: int
    origin_top: int
    width: int
    height: int
    advance_px: float
    fill_rule: str

    def tile_boxes(self, tile_width: int, tile_height: int) -> list[tuple[tuple[int, int], PixelBox]]:
        if tile_width < 1 or tile_height < 1:
            fail(E_CONFIG_INVALID, "Tile dimensions must be positive")
        result: list[tuple[tuple[int, int], PixelBox]] = []
        for tile_y in range(math.ceil(self.height / tile_height)):
            for tile_x in range(math.ceil(self.width / tile_width)):
                x = tile_x * tile_width
                y = tile_y * tile_height
                width = min(tile_width, self.width - x)
                height = min(tile_height, self.height - y)
                result.append(((tile_x, tile_y), PixelBox(x, y, width, height)))
        return result

    def signature(self) -> dict[str, object]:
        return {
            "glyph": self.glyph.name,
            "unicode": list(self.glyph.unicode),
            "pixel_size": self.pixel_size,
            "subpixel_x": self.subpixel_x,
            "subpixel_y": self.subpixel_y,
            "scale": self.scale,
            "origin_x": self.origin_x,
            "origin_top": self.origin_top,
            "width": self.width,
            "height": self.height,
            "fill_rule": self.fill_rule,
            "contours": [
                [[point.x, point.y] for point in contour]
                for contour in self.contours
            ],
        }


@dataclass(frozen=True)
class TileRaster:
    coord: tuple[int, int]
    box: PixelBox
    coverage: np.ndarray
    raw_sum: float
    quantized_sum: float
    quantization_error: float
    max_quantization_error: float
    sample_points: int
    memory_bytes: int
    content_hash: str

    def summary(self) -> dict[str, object]:
        return {
            "coord": list(self.coord),
            "box": [self.box.x, self.box.y, self.box.width, self.box.height],
            "sample_points": self.sample_points,
            "memory_bytes": self.memory_bytes,
            "content_hash": self.content_hash,
            "raw_sum": _round_float(self.raw_sum),
            "quantized_sum": _round_float(self.quantized_sum),
            "quantization_error": _round_float(self.quantization_error),
            "max_quantization_error": _round_float(self.max_quantization_error),
        }


def make_render_plan(
    glyph: ResolvedGlyph,
    pixel_size: float,
    units_per_em: float = 1000.0,
    subpixel_x: float = 0.0,
    subpixel_y: float = 0.0,
    fill_rule: str | None = None,
) -> GlyphRenderPlan:
    return make_render_plan_with_units(
        glyph,
        pixel_size=pixel_size,
        units_per_em=units_per_em,
        subpixel_x=subpixel_x,
        subpixel_y=subpixel_y,
        fill_rule=fill_rule,
    )


def make_render_plan_with_units(
    glyph: ResolvedGlyph,
    pixel_size: float,
    units_per_em: float,
    subpixel_x: float = 0.0,
    subpixel_y: float = 0.0,
    fill_rule: str | None = None,
) -> GlyphRenderPlan:
    if not math.isfinite(pixel_size) or pixel_size <= 0:
        fail(E_CONFIG_INVALID, "pixel_size must be a positive finite number")
    if not math.isfinite(subpixel_x) or not math.isfinite(subpixel_y):
        fail(E_CONFIG_INVALID, "Subpixel offsets must be finite")
    rule = fill_rule or glyph.fill_rule
    if rule not in FILL_RULES:
        fail(E_CONFIG_INVALID, "fill_rule must be nonzero or evenodd", fill_rule=rule)

    if not math.isfinite(units_per_em) or units_per_em <= 0:
        fail(E_CONFIG_INVALID, "units_per_em must be a positive finite number")
    scale = pixel_size / units_per_em

    pixel_contours: list[tuple[Point, ...]] = []
    all_points: list[Point] = []
    for contour in glyph.contours:
        transformed = tuple(
            Point(point.x * scale + subpixel_x, point.y * scale + subpixel_y)
            for point in contour.points
        )
        pixel_contours.append(transformed)
        all_points.extend(transformed)

    if all_points:
        min_x, min_y, max_x, max_y = bounds(all_points)
        origin_x = math.floor(min_x + 1e-9)
        origin_top = math.ceil(max_y - 1e-9)
        width = max(1, math.ceil(max_x - 1e-9) - origin_x)
        height = max(1, origin_top - math.floor(min_y + 1e-9))
    else:
        min_x = min_y = 0.0
        max_x = max_y = 0.0
        origin_x = 0
        origin_top = 1
        width = 1
        height = 1
    return GlyphRenderPlan(
        glyph=glyph,
        pixel_size=pixel_size,
        subpixel_x=subpixel_x,
        subpixel_y=subpixel_y,
        scale=scale,
        contours=tuple(pixel_contours),
        bbox=(min_x, min_y, max_x, max_y),
        origin_x=origin_x,
        origin_top=origin_top,
        width=width,
        height=height,
        advance_px=glyph.advance_width * scale,
        fill_rule=rule,
    )


def rasterize_tile(
    plan: GlyphRenderPlan,
    box: PixelBox,
    sampling: SamplingSpec,
    coord: tuple[int, int] = (0, 0),
) -> TileRaster:
    """Rasterize an axis-aligned tile with global, pixel-aligned sample points."""

    if box.x + box.width > plan.width or box.y + box.height > plan.height:
        fail(
            E_CONFIG_INVALID,
            "Tile box lies outside glyph bitmap",
            coord=list(coord),
            box=[box.x, box.y, box.width, box.height],
            bitmap=[plan.width, plan.height],
        )
    sample_points = box.area * sampling.samples_x * sampling.samples_y
    memory_bytes = estimate_tile_memory(box, sampling)
    coverage = _scan_tile(plan, box, sampling)
    raw_bytes = np.clip(coverage, 0.0, 1.0) * 255.0
    step = float(sampling.quantization_step)
    quantized_float = np.floor(raw_bytes / step + 0.5) * step
    quantized_float = np.clip(quantized_float, 0.0, 255.0)
    quantized = quantized_float.astype(np.uint8)
    error = np.abs(quantized_float - raw_bytes)
    content_hash = hashlib.sha256(quantized.tobytes(order="C")).hexdigest()
    return TileRaster(
        coord=coord,
        box=box,
        coverage=quantized,
        raw_sum=float(raw_bytes.sum()),
        quantized_sum=float(quantized_float.sum()),
        quantization_error=float(error.sum()),
        max_quantization_error=float(error.max()) if error.size else 0.0,
        sample_points=sample_points,
        memory_bytes=memory_bytes,
        content_hash=content_hash,
    )


def rasterize_whole(
    plan: GlyphRenderPlan,
    sampling: SamplingSpec,
) -> tuple[np.ndarray, TileRaster]:
    box = PixelBox(0, 0, plan.width, plan.height)
    tile = rasterize_tile(plan, box, sampling, (0, 0))
    return tile.coverage, tile


def estimate_tile_memory(box: PixelBox, sampling: SamplingSpec) -> int:
    # Deliberately conservative but deterministic. It is an estimate reported
    # before/with the task; enforcement happens before a tile is committed.
    pixels = box.area
    samples = pixels * sampling.samples_x * sampling.samples_y
    crossing_bytes = pixels * 5 * 3 * 8
    sample_buffers = samples * 9
    coverage_buffers = pixels * 17
    fixed_overhead = 4096
    return int(crossing_bytes + sample_buffers + coverage_buffers + fixed_overhead)


def coverage_sha256(coverage: np.ndarray) -> str:
    array = np.ascontiguousarray(coverage, dtype=np.uint8)
    return hashlib.sha256(array.tobytes(order="C")).hexdigest()


def compare_coverage(
    tiled: np.ndarray,
    reference: np.ndarray,
    tolerance: float,
) -> tuple[bool, dict[str, object]]:
    if tiled.shape != reference.shape:
        return False, {
            "reason": "shape",
            "tiled_shape": list(tiled.shape),
            "reference_shape": list(reference.shape),
            "coordinate": [0, 0],
        }
    difference = np.abs(tiled.astype(np.int16) - reference.astype(np.int16))
    max_difference = int(difference.max()) if difference.size else 0
    total_difference = int(difference.sum())
    if max_difference <= tolerance:
        return True, {
            "max_difference": max_difference,
            "total_difference": total_difference,
            "tolerance": tolerance,
        }
    y, x = np.unravel_index(int(np.argmax(difference)), difference.shape)
    return False, {
        "reason": "coverage",
        "coordinate": [int(x), int(y)],
        "tiled_value": int(tiled[y, x]),
        "reference_value": int(reference[y, x]),
        "difference": int(difference[y, x]),
        "max_difference": max_difference,
        "total_difference": total_difference,
        "tolerance": tolerance,
    }


def tile_mismatch_coord(details: dict[str, object], plan: GlyphRenderPlan, tile_width: int, tile_height: int) -> tuple[int, int]:
    coordinate = details.get("coordinate", [0, 0])
    if not isinstance(coordinate, (list, tuple)) or len(coordinate) != 2:
        return (0, 0)
    x, y = int(coordinate[0]), int(coordinate[1])
    return (x // tile_width, y // tile_height)


def _scan_tile(plan: GlyphRenderPlan, box: PixelBox, sampling: SamplingSpec) -> np.ndarray:
    if box.width == 0 or box.height == 0:
        return np.zeros((box.height, box.width), dtype=np.float64)
    if not plan.contours:
        return np.zeros((box.height, box.width), dtype=np.float64)

    edges = _edge_arrays(plan.contours)
    if edges is None:
        return np.zeros((box.height, box.width), dtype=np.float64)
    x0, y0, x1, y1, winding = edges

    offset_x = (np.arange(sampling.samples_x, dtype=np.float64) + 0.5) / sampling.samples_x
    offset_y = (np.arange(sampling.samples_y, dtype=np.float64) + 0.5) / sampling.samples_y
    pixel_columns = np.arange(box.x, box.x + box.width, dtype=np.float64)
    x_samples = (pixel_columns[:, None] + offset_x[None, :]).reshape(-1)
    x_samples += plan.origin_x
    sample_count = np.zeros((box.height, box.width), dtype=np.int32)

    for row in range(box.height):
        gy = box.y + row
        for sub_y, y_offset in enumerate(offset_y):
            y_sample = plan.origin_top - (gy + y_offset)
            crossing_mask = ((y0 <= y_sample) & (y_sample < y1)) | ((y1 <= y_sample) & (y_sample < y0))
            if not crossing_mask.any():
                continue
            crossing_x = x0[crossing_mask] + (
                (y_sample - y0[crossing_mask]) * (x1[crossing_mask] - x0[crossing_mask])
                / (y1[crossing_mask] - y0[crossing_mask])
            )
            crossing_winding = winding[crossing_mask]
            order = np.argsort(crossing_x, kind="stable")
            crossing_x = crossing_x[order]
            crossing_winding = crossing_winding[order]
            cumulative_winding = np.cumsum(crossing_winding)
            left_index = np.searchsorted(crossing_x, x_samples, side="right")
            winding_at_sample = np.zeros(x_samples.shape, dtype=np.int32)
            nonempty = left_index > 0
            winding_at_sample[nonempty] = cumulative_winding[left_index[nonempty] - 1]
            if plan.fill_rule == "nonzero":
                inside = winding_at_sample != 0
            else:
                inside = (left_index % 2) == 1
            sample_count[row] += inside.reshape(box.width, sampling.samples_x).sum(axis=1)

    total_samples = sampling.samples_x * sampling.samples_y
    return sample_count.astype(np.float64) / float(total_samples)


def _edge_arrays(contours: tuple[tuple[Point, ...], ...]) -> tuple[np.ndarray, ...] | None:
    x0: list[float] = []
    y0: list[float] = []
    x1: list[float] = []
    y1: list[float] = []
    winding: list[int] = []
    for contour in contours:
        for index, start in enumerate(contour):
            end = contour[(index + 1) % len(contour)]
            if start.y == end.y:
                continue
            x0.append(start.x)
            y0.append(start.y)
            x1.append(end.x)
            y1.append(end.y)
            winding.append(1 if end.y > start.y else -1)
    if not x0:
        return None
    return (
        np.asarray(x0, dtype=np.float64),
        np.asarray(y0, dtype=np.float64),
        np.asarray(x1, dtype=np.float64),
        np.asarray(y1, dtype=np.float64),
        np.asarray(winding, dtype=np.int32),
    )


def _round_float(value: float) -> float:
    return float(round(value, 9))

