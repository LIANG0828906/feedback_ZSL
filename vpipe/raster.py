from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass, field
from typing import Any, Callable, Sequence

from .errors import E_PREVIEW_STALE, PipelineError
from .geometry import PreparedPath, prepare_path
from .model import Bounds, PathData, Point, bounds_for_paths


@dataclass(frozen=True, slots=True)
class RasterSpec:
    width: int
    height: int
    bounds: Bounds
    background_rgba: tuple[int, int, int, int] = (246, 247, 249, 255)
    fill_rgba: tuple[int, int, int, int] = (31, 111, 235, 210)

    def __post_init__(self) -> None:
        if self.width <= 0 or self.height <= 0:
            raise ValueError("raster dimensions must be positive")
        if self.bounds.width <= 0.0 or self.bounds.height <= 0.0:
            raise ValueError("raster bounds must have positive area")

    @property
    def cell_area(self) -> float:
        return (self.bounds.width / self.width) * (self.bounds.height / self.height)

    def pixel_center(self, x: int, y: int) -> tuple[float, float]:
        px = self.bounds.min_x + (x + 0.5) * self.bounds.width / self.width
        py = self.bounds.min_y + (y + 0.5) * self.bounds.height / self.height
        return px, py

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "height": self.height,
            "bounds": self.bounds.to_dict(),
            "background_rgba": list(self.background_rgba),
            "fill_rgba": list(self.fill_rgba),
            "cell_area": self.cell_area,
        }

    @staticmethod
    def auto(
        paths: Sequence[PathData],
        max_side: int = 320,
        margin_ratio: float = 0.04,
    ) -> "RasterSpec":
        bounds = bounds_for_paths(paths)
        margin = max(bounds.width, bounds.height, 1.0) * margin_ratio
        bounds = bounds.expanded(margin)
        if bounds.width <= 1e-9 or bounds.height <= 1e-9:
            bounds = bounds.expanded(1.0)
        aspect = bounds.width / bounds.height
        if aspect >= 1.0:
            width = max_side
            height = max(1, min(max_side, int(round(max_side / aspect))))
        else:
            height = max_side
            width = max(1, min(max_side, int(round(max_side * aspect))))
        return RasterSpec(width, height, bounds)


@dataclass(frozen=True)
class RasterFrame:
    width: int
    height: int
    mask: bytes
    rgba: bytes
    spec: RasterSpec
    raster_hash: str
    inside_cells: int
    relevant_result_hash: str | None = None
    sequence: int = 0
    metadata: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "height": self.height,
            "raster_hash": self.raster_hash,
            "result_hash": self.relevant_result_hash,
            "inside_cells": self.inside_cells,
            "sequence": self.sequence,
            "spec": self.spec.to_dict(),
            "metadata": self.metadata,
        }


def _hash_raster(
    width: int,
    height: int,
    mask: bytes,
    spec: RasterSpec,
) -> str:
    digest = hashlib.sha256()
    digest.update(b"vpipe-raster-v1\0")
    digest.update(width.to_bytes(8, "little"))
    digest.update(height.to_bytes(8, "little"))
    digest.update(repr(spec.bounds).encode("utf-8"))
    digest.update(repr(spec.background_rgba).encode("ascii"))
    digest.update(repr(spec.fill_rgba).encode("ascii"))
    digest.update(mask)
    return "sha256:" + digest.hexdigest()


def rasterize_membership(
    spec: RasterSpec,
    membership: Callable[[float, float], bool],
    result_hash: str | None = None,
    metadata: dict[str, Any] | None = None,
) -> RasterFrame:
    mask = bytearray(spec.width * spec.height)
    for y in range(spec.height):
        for x in range(spec.width):
            px, py = spec.pixel_center(x, y)
            inside = membership(px, py)
            index = y * spec.width + x
            if inside:
                mask[index] = 1
    return _frame_from_mask(
        spec,
        bytes(mask),
        result_hash,
        metadata,
    )


def _frame_from_mask(
    spec: RasterSpec,
    mask_bytes: bytes,
    result_hash: str | None,
    metadata: dict[str, Any] | None,
) -> RasterFrame:
    rgba = bytearray(spec.width * spec.height * 4)
    background = spec.background_rgba
    fill = spec.fill_rgba
    inside_count = 0
    for index, inside in enumerate(mask_bytes):
        if inside:
            inside_count += 1
            color = fill
        else:
            color = background
        offset = index * 4
        rgba[offset : offset + 4] = bytes(color)
    return RasterFrame(
        width=spec.width,
        height=spec.height,
        mask=mask_bytes,
        rgba=bytes(rgba),
        spec=spec,
        raster_hash=_hash_raster(spec.width, spec.height, mask_bytes, spec),
        inside_cells=inside_count,
        relevant_result_hash=result_hash,
        metadata=dict(metadata or {}),
    )


def _scanline_mask(spec: RasterSpec, prepared: PreparedPath) -> bytes:
    width = spec.width
    height = spec.height
    mask = bytearray(width * height)
    scale_x = spec.bounds.width / width
    scale_y = spec.bounds.height / height
    evenodd = prepared.path.fill_rule == "evenodd"
    for row in range(height):
        py = spec.bounds.min_y + (row + 0.5) * scale_y
        events: list[tuple[float, int]] = []
        for edge in prepared.edges:
            a = edge.a
            b = edge.b
            if a.y <= py < b.y:
                x = a.x + (py - a.y) * (b.x - a.x) / (b.y - a.y)
                events.append((x, 1))
            elif b.y <= py < a.y:
                x = a.x + (py - a.y) * (b.x - a.x) / (b.y - a.y)
                events.append((x, -1))
        if not events:
            continue
        events.sort()
        winding = 0
        inside = False
        previous_x = spec.bounds.min_x
        event_index = 0
        while event_index < len(events):
            x = events[event_index][0]
            if inside and x > previous_x:
                start_pixel = max(
                    0,
                    math.ceil((previous_x - spec.bounds.min_x) / scale_x - 0.5),
                )
                end_pixel = min(
                    width,
                    math.ceil((x - spec.bounds.min_x) / scale_x - 0.5),
                )
                if end_pixel > start_pixel:
                    offset = row * width
                    mask[offset + start_pixel : offset + end_pixel] = b"\x01" * (
                        end_pixel - start_pixel
                    )
            delta = 0
            while event_index < len(events) and abs(events[event_index][0] - x) <= 1e-12:
                delta += events[event_index][1]
                event_index += 1
            winding += delta
            inside = (winding % 2 == 1) if evenodd else (winding != 0)
            previous_x = x
    return bytes(mask)


def rasterize_path(
    path: PathData,
    spec: RasterSpec,
    tolerance: float,
    result_hash: str | None = None,
) -> RasterFrame:
    prepared = prepare_path(path, tolerance)
    return _frame_from_mask(
        spec,
        _scanline_mask(spec, prepared),
        result_hash,
        {"source": "path"},
    )


def rasterize_operation_truth(
    path_a: PathData,
    path_b: PathData,
    operation: str,
    spec: RasterSpec,
    tolerance: float,
) -> RasterFrame:
    from .boolean import _operation_value

    prepared_a = prepare_path(path_a, tolerance)
    prepared_b = prepare_path(path_b, tolerance)
    mask_a = _scanline_mask(spec, prepared_a)
    mask_b = _scanline_mask(spec, prepared_b)
    combined = bytes(
        _operation_value(operation, bool(a), bool(b))
        for a, b in zip(mask_a, mask_b)
    )
    return _frame_from_mask(
        spec,
        combined,
        None,
        {"source": "operation_truth", "operation": operation},
    )


class PreviewService:
    """Only exposes immutable committed frames and rejects stale bindings."""

    def __init__(self) -> None:
        self._committed_hashes: set[str] = set()
        self._latest_by_result: dict[str, RasterFrame] = {}
        self._latest_result_hash: str | None = None
        self._sequence = 0

    def mark_committed(self, result_hash: str) -> None:
        self._committed_hashes.add(result_hash)

    def publish(self, result_hash: str, frame: RasterFrame) -> RasterFrame:
        if result_hash not in self._committed_hashes:
            raise PipelineError(
                E_PREVIEW_STALE,
                "refusing to publish a preview for an uncommitted result",
                result_hash=result_hash,
            )
        self._sequence += 1
        published = RasterFrame(
            width=frame.width,
            height=frame.height,
            mask=frame.mask,
            rgba=frame.rgba,
            spec=frame.spec,
            raster_hash=frame.raster_hash,
            inside_cells=frame.inside_cells,
            relevant_result_hash=result_hash,
            sequence=self._sequence,
            metadata=dict(frame.metadata),
        )
        self._latest_by_result[result_hash] = published
        self._latest_result_hash = result_hash
        return published

    def read(self, expected_result_hash: str) -> RasterFrame:
        if (
            expected_result_hash not in self._committed_hashes
            or expected_result_hash != self._latest_result_hash
            or expected_result_hash not in self._latest_by_result
        ):
            raise PipelineError(
                E_PREVIEW_STALE,
                "preview binding does not match the committed result list",
                expected_result_hash=expected_result_hash,
                latest_result_hash=self._latest_result_hash,
            )
        return self._latest_by_result[expected_result_hash]

    def latest_result_hash(self) -> str | None:
        return self._latest_result_hash

