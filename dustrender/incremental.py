"""Dependency-driven incremental rerendering and atomic publication."""

from __future__ import annotations

import math
import threading
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .chunks import Chunk
from .config import RenderConfig
from .renderer import ChunkResult
from .scene import Scene
from .scheduler import RenderRun, RenderScheduler, content_hash


@dataclass
class PublishedFrame:
    version: int
    scene: Scene
    config: RenderConfig
    image: np.ndarray
    transmittance: np.ndarray
    results: dict[str, ChunkResult]
    statuses: dict[str, str]

    def clone(self) -> "PublishedFrame":
        return PublishedFrame(
            version=self.version,
            scene=self.scene,
            config=self.config,
            image=self.image.copy(),
            transmittance=self.transmittance.copy(),
            results={key: value for key, value in self.results.items()},
            statuses=dict(self.statuses),
        )


class AtomicPreviewBuffer:
    """Publishes full frames with a single lock-protected reference swap."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._current: PublishedFrame | None = None

    def publish(self, frame: PublishedFrame) -> None:
        with self._lock:
            self._current = frame.clone()

    def snapshot(self) -> PublishedFrame | None:
        with self._lock:
            return self._current.clone() if self._current is not None else None

    @property
    def version(self) -> int:
        with self._lock:
            return self._current.version if self._current is not None else 0


@dataclass
class IncrementalReport:
    run: RenderRun
    affected: list[str]
    reused: list[str]
    reasons: list[str]
    changed_density_blocks: list[dict[str, Any]] = field(default_factory=list)
    conservative_full_frame: bool = False
    published_atomic: bool = True

    def to_dict(self) -> dict[str, Any]:
        return {
            "affected_chunks": self.affected,
            "affected_indices": [int(item[1:]) for item in self.affected],
            "affected_count": len(self.affected),
            "reused_chunks": self.reused,
            "reused_count": len(self.reused),
            "reused_hashes": {
                item: content_hash(self.run.results[item])
                for item in self.reused
                if item in self.run.results
            },
            "reasons": self.reasons,
            "changed_density_blocks": self.changed_density_blocks,
            "conservative_full_frame": self.conservative_full_frame,
            "published_atomic": self.published_atomic,
            "version_scene_sha256": self.run.scene.scene_sha256,
        }


def world_point_to_pixel(
    scene: Scene, point: np.ndarray
) -> tuple[float, float] | None:
    homogeneous = np.append(point, 1.0)
    camera_point = scene.camera.world_to_camera @ homogeneous
    depth = -float(camera_point[2])
    if depth <= scene.camera.near:
        return None
    x = scene.camera.intrinsics[0, 0] * float(camera_point[0]) / depth + scene.camera.intrinsics[0, 2]
    y = scene.camera.intrinsics[1, 1] * float(camera_point[1]) / depth + scene.camera.intrinsics[1, 2]
    return x, y


def project_world_box(
    scene: Scene, minimum: np.ndarray, maximum: np.ndarray, margin: int = 3
) -> tuple[int, int, int, int] | None:
    corners = np.asarray(
        [
            [x, y, z]
            for x in (minimum[0], maximum[0])
            for y in (minimum[1], maximum[1])
            for z in (minimum[2], maximum[2])
        ],
        dtype=np.float64,
    )
    points = [world_point_to_pixel(scene, corner) for corner in corners]
    if any(point is None for point in points):
        # A box crossing the camera plane can influence any pixel; be conservative.
        return 0, 0, scene.width, scene.height
    xs = [float(point[0]) for point in points if point is not None]
    ys = [float(point[1]) for point in points if point is not None]
    x0 = max(0, int(math.floor(min(xs))) - margin)
    x1 = min(scene.width, int(math.ceil(max(xs))) + margin + 1)
    y0 = max(0, int(math.floor(min(ys))) - margin)
    y1 = min(scene.height, int(math.ceil(max(ys))) + margin + 1)
    if x1 <= x0 or y1 <= y0:
        return None
    return x0, y0, x1, y1


def chunks_for_box(
    scene: Scene, chunks: list[Chunk], minimum: np.ndarray, maximum: np.ndarray
) -> set[str]:
    projected = project_world_box(scene, minimum, maximum)
    if projected is None:
        return set()
    x0, y0, x1, y1 = projected
    result = set()
    for chunk in chunks:
        if not (chunk.x1 <= x0 or chunk.x0 >= x1 or chunk.y1 <= y0 or chunk.y0 >= y1):
            result.add(chunk.id)
    return result


def changed_density_blocks(old: Scene, new: Scene) -> list[dict[str, Any]]:
    changes: list[dict[str, Any]] = []
    for field_name in sorted(set(old.density_fields) | set(new.density_fields)):
        old_field = old.density_fields.get(field_name)
        new_field = new.density_fields.get(field_name)
        if old_field is None or new_field is None:
            continue
        new_blocks = {block.index: block for block in new_field.blocks}
        for block in old_field.blocks:
            other = new_blocks.get(block.index)
            if other is not None and other.hash != block.hash:
                changes.append(
                    {
                        "field": field_name,
                        "block_index": list(block.index),
                        "old_hash": block.hash,
                        "new_hash": other.hash,
                    }
                )
    return changes


def analyze_dependencies(
    old: Scene,
    new: Scene,
    chunks: list[Chunk],
    *,
    shadow_distance: float = 8.0,
) -> tuple[set[str], list[str], list[dict[str, Any]], bool]:
    reasons: list[str] = []
    affected: set[str] = set()
    conservative = False
    if (
        old.width != new.width
        or old.height != new.height
        or not np.array_equal(old.camera.intrinsics, new.camera.intrinsics)
        or not np.array_equal(old.camera.world_to_camera, new.camera.world_to_camera)
        or old.camera.near != new.camera.near
        or old.camera.far != new.camera.far
    ):
        reasons.append("camera")
        affected = {chunk.id for chunk in chunks}
        conservative = True
    if not np.array_equal(old.light.direction, new.light.direction) or not np.array_equal(
        old.light.color, new.light.color
    ) or old.light.intensity != new.light.intensity:
        reasons.append("light")
        affected = {chunk.id for chunk in chunks}
        conservative = True
    old_media = old.metadata.get("media")
    new_media = new.metadata.get("media")
    if old_media != new_media:
        reasons.append("medium")
        old_ids = {item.get("id") for item in old_media or [] if isinstance(item, dict)}
        new_ids = {item.get("id") for item in new_media or [] if isinstance(item, dict)}
        if old_ids != new_ids:
            affected = {chunk.id for chunk in chunks}
            conservative = True
        else:
            for index, medium in enumerate(new.media):
                old_raw = next(
                    (item for item in old_media or [] if item.get("id") == medium.id), None
                )
                new_raw = next(
                    (item for item in new_media or [] if item.get("id") == medium.id), None
                )
                if old_raw == new_raw:
                    continue
                minimum, maximum = medium.shape.bounds()
                affected |= chunks_for_box(new, chunks, minimum, maximum)
    block_changes = changed_density_blocks(old, new)
    if block_changes:
        reasons.append("density_block")
        for change in block_changes:
            field = new.density_fields[change["field"]]
            bounds = field.physical_bounds_for_blocks([tuple(change["block_index"])])
            if bounds is None:
                affected = {chunk.id for chunk in chunks}
                conservative = True
                continue
            minimum, maximum = bounds
            affected |= chunks_for_box(new, chunks, minimum, maximum)
            # A sample point can be shadowed by the changed block when its light ray
            # intersects the block. Sweep the block opposite the light for the full
            # shadow distance, then conservatively consider only media using the field.
            low_corner = minimum - new.light.direction * shadow_distance
            high_corner = maximum - new.light.direction * shadow_distance
            swept_min = np.minimum(minimum, low_corner)
            swept_max = np.maximum(maximum, high_corner)
            for medium in new.media:
                if medium.density_field != change["field"]:
                    continue
                medium_min, medium_max = medium.shape.bounds()
                overlap_min = np.maximum(swept_min, medium_min)
                overlap_max = np.minimum(swept_max, medium_max)
                if np.all(overlap_max > overlap_min):
                    affected |= chunks_for_box(new, chunks, overlap_min, overlap_max)
    if not reasons and old.scene_sha256 != new.scene_sha256:
        reasons.append("unproven_change")
        affected = {chunk.id for chunk in chunks}
        conservative = True
    if not reasons:
        reasons.append("no_change")
    return affected, reasons, block_changes, conservative


class IncrementalRenderer:
    def __init__(self, preview: AtomicPreviewBuffer | None = None) -> None:
        self.preview = preview or AtomicPreviewBuffer()
        self.scheduler = RenderScheduler()

    def render_initial(self, scene: Scene, config: RenderConfig, *, persist=None) -> RenderRun:
        run = self.scheduler.render(scene, config)
        self._publish(run, 1)
        return run

    def render_update(
        self,
        previous_run: RenderRun,
        updated_scene: Scene,
        config: RenderConfig | None = None,
    ) -> IncrementalReport:
        config = config or previous_run.config
        chunks = [
            chunk
            for chunk in previous_run.chunks
            if not (
                updated_scene.width != previous_run.scene.width
                or updated_scene.height != previous_run.scene.height
            )
        ]
        if len(chunks) != len(previous_run.chunks):
            chunks = previous_run.chunks
        affected, reasons, changes, conservative = analyze_dependencies(
            previous_run.scene,
            updated_scene,
            chunks,
            shadow_distance=config.shadow_distance,
        )
        preloaded = {
            chunk.id: previous_run.results[chunk.id]
            for chunk in chunks
            if chunk.id in previous_run.results and chunk.id not in affected
        }
        # The old preview stays readable until the new run has fully succeeded.
        old_preview = self.preview.snapshot()
        run = self.scheduler.render(
            updated_scene,
            config,
            preloaded=preloaded,
            preloaded_status="reused",
            reuse_existing=True,
        )
        if old_preview is not None:
            assert self.preview.snapshot().version == old_preview.version
        if run.error is None:
            self._publish(run, (old_preview.version if old_preview else 0) + 1)
        reused = sorted(preloaded, key=lambda value: int(value[1:]))
        report = IncrementalReport(
            run=run,
            affected=sorted(affected, key=lambda value: int(value[1:])),
            reused=reused,
            reasons=reasons,
            changed_density_blocks=changes,
            conservative_full_frame=conservative,
        )
        run.incremental = report.to_dict()
        return report

    def _publish(self, run: RenderRun, version: int) -> None:
        self.preview.publish(
            PublishedFrame(
                version=version,
                scene=run.scene,
                config=run.config,
                image=run.image,
                transmittance=run.transmittance,
                results=run.results,
                statuses=run.statuses,
            )
        )
