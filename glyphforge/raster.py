from __future__ import annotations

import hashlib
import math
import struct
import threading
from collections.abc import Iterable
from dataclasses import dataclass
from typing import Protocol

import numpy as np

from .errors import ErrorCode, GlyphForgeError, RasterCancelled
from .model import Glyph

_TEMP_BYTES_PER_SAMPLE = 64


class CancelEvent(Protocol):
    def is_set(self) -> bool: ...


@dataclass(frozen=True)
class RasterOptions:
    pixel_size: float
    units_per_em: float
    subpixel_x: float = 0.0
    subpixel_y: float = 0.0
    supersample: int = 4
    quant_step: float = 1.0 / 255.0
    fill_rule: str = "nonzero"

    def validate(self) -> None:
        if self.units_per_em <= 0 or not math.isfinite(self.units_per_em):
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "units_per_em must be positive",
                {"units_per_em": self.units_per_em},
            )
        if self.pixel_size <= 0 or not math.isfinite(self.pixel_size):
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "pixel size must be positive",
                {"pixel_size": self.pixel_size},
            )
        if not math.isfinite(self.subpixel_x) or not math.isfinite(self.subpixel_y):
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "subpixel offsets must be finite",
                {"subpixel_x": self.subpixel_x, "subpixel_y": self.subpixel_y},
            )
        if self.supersample < 1 or self.supersample > 16:
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "supersample must be between 1 and 16",
                {"supersample": self.supersample},
            )
        if self.fill_rule not in ("nonzero", "evenodd"):
            raise GlyphForgeError(
                ErrorCode.E_GLYPH_INVALID,
                "fill rule must be nonzero or evenodd",
                {"fill_rule": self.fill_rule},
            )
        if not math.isfinite(self.quant_step) or self.quant_step <= 0.0:
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


@dataclass(frozen=True)
class EdgeBatch:
    x0: np.ndarray
    y0: np.ndarray
    x1: np.ndarray
    y1: np.ndarray
    direction: np.ndarray


@dataclass
class GlyphGeometry:
    glyph_name: str
    contours: tuple[np.ndarray, ...]
    continuous_bbox: tuple[float, float, float, float]
    x0: int
    y0: int
    width: int
    height: int
    edges: EdgeBatch
    samples_per_pixel: int

    @property
    def rect(self) -> tuple[int, int, int, int]:
        return self.x0, self.y0, self.width, self.height

    def tile_rects(self, tile_size: int) -> list[tuple[int, int, int, int, int, int]]:
        if tile_size <= 0:
            raise ValueError("tile size must be positive")
        tiles: list[tuple[int, int, int, int, int, int]] = []
        for row, top in enumerate(range(0, self.height, tile_size)):
            for col, left in enumerate(range(0, self.width, tile_size)):
                width = min(tile_size, self.width - left)
                height = min(tile_size, self.height - top)
                tiles.append(
                    (
                        row,
                        col,
                        self.x0 + left,
                        self.y0 + top,
                        width,
                        height,
                    )
                )
        return tiles


@dataclass(frozen=True)
class CoverageSummary:
    samples: int
    pixels: int
    raw_coverage_sum: float
    quantized_coverage_sum: float
    quantization_abs_error: float
    quantization_squared_error: float
    max_abs_error: float
    quant_step: float

    @classmethod
    def from_arrays(
        cls,
        raw: np.ndarray,
        quantized_bytes: np.ndarray,
        quant_step: float,
    ) -> CoverageSummary:
        before = raw.astype(np.float64, copy=False)
        after = quantized_bytes.astype(np.float64) / 255.0
        difference = after - before
        return cls(
            samples=0,
            pixels=int(raw.size),
            raw_coverage_sum=float(before.sum()),
            quantized_coverage_sum=float(after.sum()),
            quantization_abs_error=float(np.abs(difference).sum()),
            quantization_squared_error=float(np.square(difference).sum()),
            max_abs_error=float(np.abs(difference).max()) if difference.size else 0.0,
            quant_step=float(quant_step),
        )

    def with_samples(self, samples: int) -> CoverageSummary:
        return CoverageSummary(
            samples=samples,
            pixels=self.pixels,
            raw_coverage_sum=self.raw_coverage_sum,
            quantized_coverage_sum=self.quantized_coverage_sum,
            quantization_abs_error=self.quantization_abs_error,
            quantization_squared_error=self.quantization_squared_error,
            max_abs_error=self.max_abs_error,
            quant_step=self.quant_step,
        )

    def __add__(self, other: CoverageSummary) -> CoverageSummary:
        return CoverageSummary(
            samples=self.samples + other.samples,
            pixels=self.pixels + other.pixels,
            raw_coverage_sum=self.raw_coverage_sum + other.raw_coverage_sum,
            quantized_coverage_sum=(
                self.quantized_coverage_sum + other.quantized_coverage_sum
            ),
            quantization_abs_error=(
                self.quantization_abs_error + other.quantization_abs_error
            ),
            quantization_squared_error=(
                self.quantization_squared_error + other.quantization_squared_error
            ),
            max_abs_error=max(self.max_abs_error, other.max_abs_error),
            quant_step=self.quant_step,
        )

    def to_dict(self) -> dict:
        return {
            "samples": self.samples,
            "pixels": self.pixels,
            "raw_coverage_sum": self.raw_coverage_sum,
            "quantized_coverage_sum": self.quantized_coverage_sum,
            "quantization_abs_error": self.quantization_abs_error,
            "quantization_rms_error": (
                math.sqrt(self.quantization_squared_error / self.pixels)
                if self.pixels
                else 0.0
            ),
            "max_abs_error": self.max_abs_error,
            "quant_step": self.quant_step,
        }


@dataclass(frozen=True)
class TileCoverage:
    glyph_name: str
    row: int
    col: int
    x: int
    y: int
    width: int
    height: int
    coverage: np.ndarray
    summary: CoverageSummary
    batch_size: int
    content_hash: str


def coverage_content_hash(coverage: np.ndarray) -> str:
    height, width = coverage.shape
    digest = hashlib.sha256()
    digest.update(struct.pack("<II", width, height))
    digest.update(np.ascontiguousarray(coverage, dtype=np.uint8).tobytes())
    return digest.hexdigest()


def _build_edges(contours: Iterable[np.ndarray]) -> EdgeBatch:
    starts: list[np.ndarray] = []
    ends: list[np.ndarray] = []
    for points in contours:
        if len(points) < 2:
            continue
        starts.append(points[:-1])
        ends.append(points[1:])
    if not starts:
        empty = np.empty(0, dtype=np.float64)
        return EdgeBatch(empty, empty, empty, empty, np.empty(0, dtype=np.int8))
    p0 = np.concatenate(starts, axis=0)
    p1 = np.concatenate(ends, axis=0)
    direction = np.where(p1[:, 1] > p0[:, 1], 1, -1).astype(np.int8)
    return EdgeBatch(p0[:, 0], p0[:, 1], p1[:, 0], p1[:, 1], direction)


def prepare_geometry(
    glyph: Glyph,
    options: RasterOptions,
) -> GlyphGeometry:
    options.validate()
    scale = options.pixel_size / options.units_per_em
    contours = tuple(
        np.column_stack(
            (
                contour.points[:, 0] * scale + options.subpixel_x,
                -contour.points[:, 1] * scale + options.subpixel_y,
            )
        )
        for contour in glyph.contours
    )
    if contours:
        combined = np.concatenate(contours, axis=0)
        min_x, min_y = combined.min(axis=0)
        max_x, max_y = combined.max(axis=0)
        x0 = int(math.floor(float(min_x)))
        y0 = int(math.floor(float(min_y)))
        x1 = int(math.ceil(float(max_x)))
        y1 = int(math.ceil(float(max_y)))
        width = max(0, x1 - x0)
        height = max(0, y1 - y0)
    else:
        min_x = min_y = max_x = max_y = 0.0
        x0 = y0 = 0
        width = height = 0
    return GlyphGeometry(
        glyph_name=glyph.name,
        contours=contours,
        continuous_bbox=(float(min_x), float(min_y), float(max_x), float(max_y)),
        x0=x0,
        y0=y0,
        width=width,
        height=height,
        edges=_build_edges(contours),
        samples_per_pixel=options.supersample * options.supersample,
    )


def _raise_if_cancelled(cancel_event: CancelEvent | None) -> None:
    if cancel_event is not None and cancel_event.is_set():
        raise RasterCancelled()


def _coverage_region(
    geometry: GlyphGeometry,
    x0: int,
    y0: int,
    width: int,
    height: int,
    options: RasterOptions,
    batch_size: int,
    cancel_event: CancelEvent | None,
) -> tuple[np.ndarray, int]:
    if width <= 0 or height <= 0:
        return np.zeros((max(0, height), max(0, width)), dtype=np.float64), 0
    supersample = options.supersample
    samples_per_pixel = supersample * supersample
    sample_count = width * height * samples_per_pixel
    coverage = np.zeros((height, width), dtype=np.float64)
    edges = geometry.edges
    if edges.x0.size == 0:
        return coverage, sample_count

    local_pixels = width * height
    sx_offsets = (
        (np.arange(samples_per_pixel, dtype=np.float64) % supersample + 0.5)
        / supersample
    )
    sy_offsets = (
        (np.arange(samples_per_pixel, dtype=np.float64) // supersample + 0.5)
        / supersample
    )
    pixel_offsets_x = np.arange(width, dtype=np.float64)
    pixel_offsets_y = np.arange(height, dtype=np.float64)

    for sample_start in range(0, sample_count, batch_size):
        _raise_if_cancelled(cancel_event)
        sample_stop = min(sample_count, sample_start + batch_size)
        sample_indices = np.arange(sample_start, sample_stop, dtype=np.int64)
        pixel_indices = sample_indices // samples_per_pixel
        sample_in_pixel = sample_indices % samples_per_pixel
        pixel_x = pixel_indices % width
        pixel_y = pixel_indices // width
        sample_x = x0 + pixel_offsets_x[pixel_x] + sx_offsets[sample_in_pixel]
        sample_y = y0 + pixel_offsets_y[pixel_y] + sy_offsets[sample_in_pixel]

        py = sample_y[:, None]
        px = sample_x[:, None]
        upward = (edges.y0[None, :] <= py) & (edges.y1[None, :] > py)
        downward = (edges.y1[None, :] <= py) & (edges.y0[None, :] > py)
        active = upward | downward
        delta_y = edges.y1 - edges.y0
        safe_delta = np.where(delta_y == 0.0, 1.0, delta_y)
        crossing_x = edges.x0 + (py - edges.y0) * (edges.x1 - edges.x0) / safe_delta
        crossing = active & (px < crossing_x)
        if options.fill_rule == "evenodd":
            sample_hits = np.bincount(
                np.nonzero(crossing)[0], minlength=sample_stop - sample_start
            )
            filled = (sample_hits % 2) == 1
        else:
            crossing_samples, crossing_edges = np.nonzero(crossing)
            winding = np.bincount(
                crossing_samples,
                weights=edges.direction[crossing_edges],
                minlength=sample_stop - sample_start,
            )
            filled = winding != 0
        if filled.any():
            coverage += (
                np.bincount(
                    pixel_indices[filled],
                    minlength=local_pixels,
                ).reshape(height, width)
                / float(samples_per_pixel)
            )
    return coverage, sample_count


def _quantize(raw: np.ndarray, quant_step: float) -> np.ndarray:
    if not math.isfinite(quant_step) or quant_step <= 0.0 or quant_step > 1.0:
        raise GlyphForgeError(
            ErrorCode.E_QUANT_INVALID,
            "quantization step must be finite and in (0, 1]",
            {"quant_step": quant_step},
        )
    levels = np.rint(np.clip(raw, 0.0, 1.0) / quant_step)
    quantized = np.clip(levels * quant_step, 0.0, 1.0)
    return np.rint(quantized * 255.0).astype(np.uint8)


def raster_region(
    geometry: GlyphGeometry,
    *,
    row: int,
    col: int,
    x0: int,
    y0: int,
    width: int,
    height: int,
    options: RasterOptions,
    batch_size: int,
    cancel_event: CancelEvent | None = None,
) -> TileCoverage:
    if batch_size <= 0:
        raise ValueError("batch size must be positive")
    options.validate()
    raw, sample_count = _coverage_region(
        geometry,
        x0,
        y0,
        width,
        height,
        options,
        batch_size,
        cancel_event,
    )
    coverage = _quantize(raw, options.quant_step)
    summary = CoverageSummary.from_arrays(raw, coverage, options.quant_step)
    summary = summary.with_samples(sample_count)
    return TileCoverage(
        glyph_name=geometry.glyph_name,
        row=row,
        col=col,
        x=x0,
        y=y0,
        width=width,
        height=height,
        coverage=coverage,
        summary=summary,
        batch_size=batch_size,
        content_hash=coverage_content_hash(coverage),
    )


def compose_tiles(
    tiles: Iterable[TileCoverage],
    *,
    geometry: GlyphGeometry,
    strict: bool = True,
    quant_step: float | None = None,
) -> tuple[np.ndarray, CoverageSummary]:
    coverage = np.zeros((geometry.height, geometry.width), dtype=np.uint8)
    occupied = (
        np.zeros((geometry.height, geometry.width), dtype=np.uint8)
        if strict
        else None
    )
    summary: CoverageSummary | None = None
    ordered = sorted(tiles, key=lambda tile: (tile.row, tile.col))
    for tile in ordered:
        local_y = tile.y - geometry.y0
        local_x = tile.x - geometry.x0
        if tile.width <= 0 or tile.height <= 0:
            continue
        if (
            local_x < 0
            or local_y < 0
            or local_x + tile.width > geometry.width
            or local_y + tile.height > geometry.height
        ):
            raise ValueError("tile lies outside glyph coverage bounds")
        if occupied is not None:
            region = occupied[
                local_y : local_y + tile.height,
                local_x : local_x + tile.width,
            ]
            if np.any(region):
                raise ValueError("overlapping tile coverage")
            region.fill(1)
        coverage[
            local_y : local_y + tile.height,
            local_x : local_x + tile.width,
        ] = tile.coverage
        summary = tile.summary if summary is None else summary + tile.summary
    if (
        occupied is not None
        and geometry.width
        and geometry.height
        and not np.all(occupied)
    ):
        raise ValueError("incomplete tile coverage")
    empty = CoverageSummary(
        samples=0,
        pixels=0,
        raw_coverage_sum=0.0,
        quantized_coverage_sum=0.0,
        quantization_abs_error=0.0,
        quantization_squared_error=0.0,
        max_abs_error=0.0,
        quant_step=(
            float(quant_step)
            if quant_step is not None
            else options_quant_step(ordered)
        ),
    )
    return coverage, summary or empty


def options_quant_step(tiles: Iterable[TileCoverage]) -> float:
    for tile in tiles:
        return tile.summary.quant_step
    return 1.0 / 255.0


def temporary_scanline_bytes(batch_size: int) -> int:
    return max(1, batch_size) * _TEMP_BYTES_PER_SAMPLE


def sample_count_for_rect(width: int, height: int, supersample: int) -> int:
    return max(0, width) * max(0, height) * supersample * supersample


class EventCheckpoint:
    """Small helper for callers that need to expose cancellation checkpoints."""

    def __init__(self, event: threading.Event):
        self.event = event

    def __call__(self) -> None:
        if self.event.is_set():
            raise RasterCancelled()
