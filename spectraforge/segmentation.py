"""Stable pixel-block and spectral-band segmentation."""

from __future__ import annotations

from typing import Any

from .errors import WorkbenchError
from .models import SegmentSpec
from .util import hash_object


def make_segments(
    width: int,
    height: int,
    band_count: int,
    *,
    split_mode: str = "tiles",
    tile_width: int = 32,
    tile_height: int = 32,
) -> list[SegmentSpec]:
    if split_mode == "tiles":
        if tile_width <= 0 or tile_height <= 0:
            raise WorkbenchError(
                "E_PARA",
                "Tile dimensions must be positive",
                {"field": "tile_width/tile_height", "tile_width": tile_width, "tile_height": tile_height},
            )
        segments: list[SegmentSpec] = []
        index = 0
        for y0 in range(0, height, tile_height):
            for x0 in range(0, width, tile_width):
                x1 = min(width, x0 + tile_width)
                y1 = min(height, y0 + tile_height)
                segments.append(
                    SegmentSpec(
                        index=index,
                        mode="tiles",
                        x0=x0,
                        y0=y0,
                        x1=x1,
                        y1=y1,
                        label=f"tile {index:03d} [{x0},{y0}]-[{x1},{y1})",
                    )
                )
                index += 1
        return segments
    if split_mode == "bands":
        return [
            SegmentSpec(
                index=index,
                mode="bands",
                x0=0,
                y0=0,
                x1=width,
                y1=height,
                band_start=index,
                band_end=index + 1,
                label=f"band {index:02d}",
            )
            for index in range(band_count)
        ]
    raise WorkbenchError(
        "E_PARA",
        "Unknown segmentation mode",
        {"field": "split_mode", "value": split_mode, "allowed": ["tiles", "bands"]},
    )


def segmentation_spec(
    width: int,
    height: int,
    band_count: int,
    *,
    split_mode: str,
    tile_width: int,
    tile_height: int,
    interpolation: str,
) -> dict[str, Any]:
    return {
        "split_mode": split_mode,
        "tile_width": int(tile_width),
        "tile_height": int(tile_height),
        "width": int(width),
        "height": int(height),
        "band_count": int(band_count),
        "interpolation": str(interpolation),
    }


def segmentation_key(spec: dict[str, Any]) -> str:
    return hash_object(spec).split(":", 1)[1][:16]


def segment_for_pixel(segments: list[SegmentSpec], x: int, y: int) -> int:
    for segment in segments:
        if segment.mode == "tiles":
            if segment.x0 <= x < segment.x1 and segment.y0 <= y < segment.y1:
                return segment.index
    return 0


def estimate_segment_bytes(spec: SegmentSpec, band_count: int) -> int:
    pixels = max(1, spec.width * spec.height)
    # aligned band slice + absolute/chroma/linear RGB + masks + temporary fusion.
    return int(pixels * (band_count * 8 + 3 * 8 * 4 + 4 * 4 + 256))
