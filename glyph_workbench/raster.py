from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .errors import WorkbenchError
from .flatten import FlatContour
from .geometry import glyph_bbox
from .models import Glyph
from .params import RASTER_ALGORITHM, RenderParams

MAX_BITMAP_SIDE = 16384
MAX_BITMAP_PIXELS = 64 * 1024 * 1024


@dataclass(frozen=True)
class RasterSpec:
    width: int
    height: int
    grid: int
    pixels_per_unit: float
    bbox: tuple[float, float, float, float]

    @property
    def scaled_origin_x(self) -> float:
        return self.bbox[0] * self.pixels_per_unit

    @property
    def scaled_top_y(self) -> float:
        return self.bbox[3] * self.pixels_per_unit

    @property
    def fine_width(self) -> int:
        return self.width * self.grid

    @property
    def fine_height(self) -> int:
        return self.height * self.grid

    @property
    def component_bit_bytes(self) -> int:
        return math.ceil((self.fine_width * self.fine_height) / 8)

    @property
    def component_working_bytes(self) -> int:
        return self.component_bit_bytes + self.width * self.height * 2

    def check_buffer(self, config: RenderParams, contour_index: int | None = None) -> None:
        if self.component_working_bytes > config.max_buffer_bytes:
            raise WorkbenchError(
                "E_PARA",
                "per-thread bitmap buffer exceeds max_buffer_bytes",
                contour_index=contour_index,
                required_bytes=self.component_working_bytes,
                max_buffer_bytes=config.max_buffer_bytes,
            )

    def as_dict(self) -> dict:
        return {
            "width": self.width,
            "height": self.height,
            "grid": self.grid,
            "pixels_per_unit": self.pixels_per_unit,
            "bbox": list(self.bbox),
            "component_bit_bytes": self.component_bit_bytes,
        }


@dataclass
class RasterComponent:
    contour_index: int
    packed_bits: np.ndarray
    coverage_counts: np.ndarray
    flat_count: int
    content_hash: str

    @property
    def buffer_bytes(self) -> int:
        return int(self.packed_bits.nbytes + self.coverage_counts.nbytes)

    def unpack_bits(self, spec: RasterSpec) -> np.ndarray:
        return np.unpackbits(self.packed_bits)[: spec.fine_width * spec.fine_height]

    def to_dict(self) -> dict:
        return {
            "contour_index": self.contour_index,
            "flat_count": self.flat_count,
            "buffer_bytes": self.buffer_bytes,
            "content_hash": self.content_hash,
        }


@dataclass
class RasterResult:
    glyph_id: str
    params: RenderParams
    spec: RasterSpec
    components: list[RasterComponent]
    coverage_counts: np.ndarray
    coverage: np.ndarray
    coverage_u8: np.ndarray
    area: float
    commit_sequence: list[int]

    @property
    def width(self) -> int:
        return self.spec.width

    @property
    def height(self) -> int:
        return self.spec.height

    def component_by_index(self, contour_index: int) -> RasterComponent:
        for component in self.components:
            if component.contour_index == contour_index:
                return component
        raise KeyError(contour_index)

    def summary(self) -> dict:
        return {
            "glyph_id": self.glyph_id,
            "width": self.width,
            "height": self.height,
            "grid": self.spec.grid,
            "pixels_per_unit": self.spec.pixels_per_unit,
            "bbox": list(self.spec.bbox),
            "area": self.area,
            "commit_sequence": self.commit_sequence,
            "coverage_min": int(self.coverage_counts.min()),
            "coverage_max": int(self.coverage_counts.max()),
        }


def make_raster_spec(glyph: Glyph, params: RenderParams) -> RasterSpec:
    params.validate()
    bbox = glyph_bbox(glyph)
    scaled_width = (bbox[2] - bbox[0]) * params.pixels_per_unit
    scaled_height = (bbox[3] - bbox[1]) * params.pixels_per_unit
    if scaled_width <= 0.0 or scaled_height <= 0.0:
        raise WorkbenchError(
            "E_PARA",
            "glyph bounding box has no rasterizable area",
            glyph_id=glyph.glyph_id,
        )
    width = max(1, math.ceil(scaled_width - 1e-12))
    height = max(1, math.ceil(scaled_height - 1e-12))
    if width > MAX_BITMAP_SIDE or height > MAX_BITMAP_SIDE:
        raise WorkbenchError(
            "E_PARA",
            f"bitmap side exceeds {MAX_BITMAP_SIDE}",
            glyph_id=glyph.glyph_id,
            width=width,
            height=height,
        )
    if width * height > MAX_BITMAP_PIXELS:
        raise WorkbenchError(
            "E_PARA",
            f"bitmap pixel count exceeds {MAX_BITMAP_PIXELS}",
            glyph_id=glyph.glyph_id,
            width=width,
            height=height,
        )
    return RasterSpec(
        width=width,
        height=height,
        grid=params.grid,
        pixels_per_unit=params.pixels_per_unit,
        bbox=bbox,
    )


def _fill_scanline(
    fine: np.ndarray,
    spec: RasterSpec,
    flat: FlatContour,
) -> None:
    points = flat.points
    starts = points
    ends = np.roll(points, -1, axis=0)
    x0 = starts[:, 0]
    y0 = starts[:, 1]
    x1 = ends[:, 0]
    y1 = ends[:, 1]
    delta_y = y1 - y0
    non_horizontal = np.abs(delta_y) > 1e-14
    starts = starts[non_horizontal]
    ends = ends[non_horizontal]
    x0 = x0[non_horizontal]
    y0 = y0[non_horizontal]
    x1 = x1[non_horizontal]
    y1 = y1[non_horizontal]
    if x0.size == 0:
        return
    inverse_delta_y = 1.0 / (y1 - y0)
    low_y = np.minimum(y0, y1)
    high_y = np.maximum(y0, y1)
    origin_x = spec.scaled_origin_x
    top_y = spec.scaled_top_y
    fine_width = spec.fine_width
    for fine_row in range(spec.fine_height):
        sample_y = top_y - (fine_row + 0.5) / spec.grid
        crossing = (low_y <= sample_y) & (sample_y < high_y)
        if not bool(crossing.any()):
            continue
        crossing_x = x0[crossing] + (sample_y - y0[crossing]) * (
            x1[crossing] - x0[crossing]
        ) * inverse_delta_y[crossing]
        crossing_x.sort()
        row_offset = fine_row * fine_width
        for index in range(0, crossing_x.size - 1, 2):
            low_x = (float(crossing_x[index]) - origin_x) * spec.grid
            high_x = (float(crossing_x[index + 1]) - origin_x) * spec.grid
            start = max(0, math.ceil(low_x - 0.5))
            stop = min(fine_width, math.ceil(high_x - 0.5))
            if stop > start:
                fine[row_offset + start : row_offset + stop] = 1


def coverage_counts_from_bits(bits: np.ndarray, spec: RasterSpec) -> np.ndarray:
    fine = bits.reshape(spec.fine_height, spec.fine_width)
    blocks = fine.reshape(spec.height, spec.grid, spec.width, spec.grid)
    return blocks.sum(axis=(1, 3), dtype=np.uint16)


def rasterize_flat_contour(
    flat: FlatContour,
    spec: RasterSpec,
    params: RenderParams,
) -> RasterComponent:
    spec.check_buffer(params, flat.contour_index)
    fine = np.zeros(spec.fine_width * spec.fine_height, dtype=np.uint8)
    _fill_scanline(fine, spec, flat)
    packed = np.packbits(fine)
    counts = coverage_counts_from_bits(fine, spec)
    import hashlib

    digest = hashlib.sha256()
    digest.update(packed.tobytes())
    digest.update(np.ascontiguousarray(flat.points, dtype=np.float64).tobytes())
    digest.update(repr(spec.as_dict()).encode("utf-8"))
    return RasterComponent(
        contour_index=flat.contour_index,
        packed_bits=packed,
        coverage_counts=counts,
        flat_count=flat.polyline_point_count,
        content_hash=digest.hexdigest(),
    )


def combine_components(
    spec: RasterSpec, components: list[RasterComponent]
) -> tuple[np.ndarray, np.ndarray, np.ndarray, float]:
    ordered = sorted(components, key=lambda component: component.contour_index)
    expected = spec.fine_width * spec.fine_height
    combined = np.zeros(expected, dtype=np.uint8)
    for component in ordered:
        bits = component.unpack_bits(spec)
        if bits.size != expected:
            raise WorkbenchError(
                "E_STATE",
                "checkpoint bitmap component has incompatible dimensions",
                contour_index=component.contour_index,
            )
        combined ^= bits
    counts = coverage_counts_from_bits(combined, spec)
    normalization = float(spec.grid * spec.grid)
    coverage = counts.astype(np.float64) / normalization
    coverage_u8 = np.clip(
        np.rint(coverage * 255.0), 0, 255
    ).astype(np.uint8)
    area = float(counts.sum(dtype=np.uint64)) / normalization / (
        spec.pixels_per_unit * spec.pixels_per_unit
    )
    return counts, coverage, coverage_u8, area


def result_from_components(
    glyph_id: str,
    params: RenderParams,
    spec: RasterSpec,
    components: list[RasterComponent],
    commit_sequence: list[int],
) -> RasterResult:
    counts, coverage, coverage_u8, area = combine_components(spec, components)
    return RasterResult(
        glyph_id=glyph_id,
        params=params,
        spec=spec,
        components=sorted(components, key=lambda component: component.contour_index),
        coverage_counts=counts,
        coverage=coverage,
        coverage_u8=coverage_u8,
        area=area,
        commit_sequence=list(commit_sequence),
    )


def rasterize_flat_glyph(
    glyph: Glyph,
    flats: list[FlatContour],
    params: RenderParams,
) -> RasterResult:
    spec = make_raster_spec(glyph, params)
    components = [
        rasterize_flat_contour(flat, spec, params) for flat in flats
    ]
    return result_from_components(
        glyph_id=glyph.glyph_id,
        params=params,
        spec=spec,
        components=components,
        commit_sequence=[flat.contour_index for flat in flats],
    )


def component_image_hash(spec: RasterSpec, components: list[RasterComponent]) -> str:
    import hashlib

    digest = hashlib.sha256()
    for component in sorted(components, key=lambda item: item.contour_index):
        digest.update(component.packed_bits.tobytes())
    digest.update(repr(spec.as_dict()).encode("utf-8"))
    return digest.hexdigest()


def algorithm_identity() -> str:
    return RASTER_ALGORITHM

