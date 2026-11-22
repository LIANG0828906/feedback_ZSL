"""Fixed tile geometry, resource governance and per-tile NPR rendering."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import ResourceError, TileRenderError
from .model import Camera, RenderParams, Scene
from .npr import compose_style, outline_from_levels, posterize
from .raster import shade_scene
from .strokes import StrokePlan, rasterize_strokes_tile


@dataclass(frozen=True)
class Tile:
    tile_id: int
    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0


@dataclass
class BlockResult:
    tile_id: int
    x0: int
    y0: int
    x1: int
    y1: int
    base_rgb: np.ndarray
    poster_rgb: np.ndarray
    levels: np.ndarray
    outline: np.ndarray
    brush_rgba: np.ndarray
    styled_rgb: np.ndarray
    coverage: np.ndarray
    stroke_ids: tuple[int, ...]
    stroke_point_count: int
    content_hash: str
    version: int


class FaultInjector:
    """Local-only deterministic fault injection for failed-tile recovery tests."""

    def __init__(self, tile_ids: set[int] | None = None, reason: str = "injected fault") -> None:
        self.tile_ids = set(tile_ids or ())
        self.reason = reason
        self._lock = threading.Lock()

    def should_fail(self, tile_id: int) -> bool:
        with self._lock:
            return tile_id in self.tile_ids

    def clear(self, tile_id: int | None = None) -> None:
        with self._lock:
            if tile_id is None:
                self.tile_ids.clear()
            else:
                self.tile_ids.discard(tile_id)


class ResourceGovernor:
    """Global brush-cache budget plus per-tile pixel-buffer validation.

    The cache semaphore blocks when full; it never silently drops a stroke or a
    tile.  Tiles are rendered in budgets no larger than the configured capacity.
    """

    def __init__(self, params: RenderParams) -> None:
        self.params = params
        self._condition = threading.Condition()
        self._available = int(params.stroke_cache_budget)
        self._leased = 0

    def validate_tile(self, tile: Tile) -> None:
        padded_pixels = (tile.width + 2) * (tile.height + 2)
        if padded_pixels > self.params.thread_pixel_budget:
            raise ResourceError(
                "thread_pixel_budget",
                f"padded tile {tile.tile_id} needs {padded_pixels} pixels, "
                f"budget is {self.params.thread_pixel_budget}",
            )
        if tile.width <= 0 or tile.height <= 0:
            raise ResourceError("tile", "tile dimensions must be positive")

    def _acquire_stroke_slots(self, count: int) -> None:
        if count <= 0:
            return
        if count > self.params.stroke_cache_budget:
            raise ResourceError(
                "stroke_cache_budget",
                f"chunk of {count} strokes exceeds budget {self.params.stroke_cache_budget}",
            )
        with self._condition:
            while self._available < count:
                self._condition.wait()
            self._available -= count
            self._leased += count

    def _release_stroke_slots(self, count: int) -> None:
        if count > 0:
            with self._condition:
                if self._leased < count:
                    raise RuntimeError("stroke cache lease released more slots than acquired")
                self._leased -= count
                self._available += count
                if self._available > self.params.stroke_cache_budget:
                    raise RuntimeError("stroke cache budget counter overflow")
                self._condition.notify_all()


def make_tiles(width: int, height: int, tile_width: int, tile_height: int) -> tuple[Tile, ...]:
    tiles: list[Tile] = []
    tile_id = 0
    for y0 in range(0, height, tile_height):
        for x0 in range(0, width, tile_width):
            tiles.append(
                Tile(
                    tile_id=tile_id,
                    x0=x0,
                    y0=y0,
                    x1=min(width, x0 + tile_width),
                    y1=min(height, y0 + tile_height),
                )
            )
            tile_id += 1
    return tuple(tiles)


def tile_for_pixel(
    x: int,
    y: int,
    width: int,
    height: int,
    tile_width: int,
    tile_height: int,
) -> int:
    cols = (width + tile_width - 1) // tile_width
    return (y // tile_height) * cols + (x // tile_width)


def render_tile(
    scene: Scene,
    camera: Camera,
    params: RenderParams,
    plan: StrokePlan,
    tile: Tile,
    *,
    content_hash: str,
    version: int,
    quality: str = "exact",
    sample_factor: int = 1,
    governor: ResourceGovernor | None = None,
    fault_injector: FaultInjector | None = None,
) -> BlockResult:
    if fault_injector is not None and fault_injector.should_fail(tile.tile_id):
        raise TileRenderError(tile.tile_id, fault_injector.reason, injected=True)
    governor = governor or ResourceGovernor(params)
    governor.validate_tile(tile)

    px0 = tile.x0 - 1
    py0 = tile.y0 - 1
    padded_width = tile.width + 2
    padded_height = tile.height + 2
    shade = shade_scene(
        scene,
        camera,
        params.exact_width,
        params.exact_height,
        x0=px0,
        y0=py0,
        region_width=padded_width,
        region_height=padded_height,
        quality=quality,  # type: ignore[arg-type]
    )
    padded_levels, padded_poster = posterize(shade.rgb, params.levels)
    padded_outline = outline_from_levels(padded_levels)
    outline = padded_outline[1:-1, 1:-1].copy()
    # Do not create an artificial outline against pixels outside the image.
    if tile.y0 == 0:
        outline[0, :] = False
    if tile.y1 == params.exact_height:
        outline[-1, :] = False
    if tile.x0 == 0:
        outline[:, 0] = False
    if tile.x1 == params.exact_width:
        outline[:, -1] = False
    base_rgb = (shade.rgb[1:-1, 1:-1] * 255.0 + 0.5).astype(np.uint8)
    levels = padded_levels[1:-1, 1:-1].copy()
    poster_rgb = padded_poster[1:-1, 1:-1].copy()
    brush_rgba, coverage, stroke_ids, point_count = rasterize_strokes_tile(
        plan,
        params.exact_width,
        params.exact_height,
        tile.x0,
        tile.y0,
        tile.x1,
        tile.y1,
        sample_factor=sample_factor,
        cache_budget=params.stroke_cache_budget,
        cache_lease=lambda count: _make_lease(governor, count),
    )
    styled_rgb = compose_style(poster_rgb, brush_rgba, outline)
    return BlockResult(
        tile_id=tile.tile_id,
        x0=tile.x0,
        y0=tile.y0,
        x1=tile.x1,
        y1=tile.y1,
        base_rgb=base_rgb,
        poster_rgb=poster_rgb,
        levels=levels,
        outline=outline,
        brush_rgba=brush_rgba,
        styled_rgb=styled_rgb,
        coverage=coverage,
        stroke_ids=stroke_ids,
        stroke_point_count=point_count,
        content_hash=content_hash,
        version=version,
    )


def _make_lease(governor: ResourceGovernor, count: int) -> Any:
    from .strokes import CacheLease

    return CacheLease(governor, count)


def block_digest_fields(block: BlockResult) -> dict[str, Any]:
    return {
        "tile_id": block.tile_id,
        "x0": block.x0,
        "y0": block.y0,
        "x1": block.x1,
        "y1": block.y1,
        "stroke_ids": list(block.stroke_ids),
        "stroke_point_count": block.stroke_point_count,
        "content_hash": block.content_hash,
        "version": block.version,
    }

