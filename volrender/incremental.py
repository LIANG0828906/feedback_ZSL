"""Incremental rerendering and atomic version publication."""

from __future__ import annotations

import hashlib
import threading
from dataclasses import dataclass
from typing import Any

import numpy as np

from .config import RenderConfig
from .errors import AppError, ErrorCode
from .render import RenderArtifact, TileResult, make_tiles, render_frame
from .scene import Scene
from .seams import check_seams


@dataclass(frozen=True, slots=True)
class Change:
    kind: str
    medium_id: str | None = None
    block_id: int | None = None
    reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"kind": self.kind}
        if self.medium_id is not None:
            payload["medium_id"] = self.medium_id
        if self.block_id is not None:
            payload["block_id"] = self.block_id
        if self.reason:
            payload["reason"] = self.reason
        return payload


@dataclass(frozen=True, slots=True)
class PublishedVersion:
    version: int
    scene_id: str
    artifact: RenderArtifact
    tile_hashes: dict[int, str]
    publication: dict[str, Any]

    @property
    def buffer_digest(self) -> str:
        return str(self.artifact.summary["buffer_digest"])


def _projected_tile_indices(
    scene: Scene,
    config: RenderConfig,
    medium_ids: set[str],
) -> tuple[set[int], list[str]]:
    tiles = make_tiles(scene, config)
    affected: set[int] = set()
    reasons: list[str] = []
    for medium_id in sorted(medium_ids):
        matching = [
            medium
            for medium in scene.media
            if medium.medium_id == medium_id
        ]
        if not matching:
            return (
                {tile.index for tile in tiles},
                [f"medium {medium_id} not found; conservative full rerender"],
            )
        medium = matching[0]
        lower = medium.bounds_min.copy()
        upper = medium.bounds_max.copy()
        if config.shadow_samples > 0:
            # A changed medium can alter shadow visibility for primary
            # samples outside its direct projection. Sweep its bounds
            # backwards along the light direction by the full shadow range.
            lower = np.minimum(
                lower,
                medium.bounds_min - scene.light.direction * config.shadow_distance,
            )
            upper = np.maximum(
                upper,
                medium.bounds_max - scene.light.direction * config.shadow_distance,
            )
        corners = np.array(
            [
                [x, y, z]
                for x in (lower[0], upper[0])
                for y in (lower[1], upper[1])
                for z in (lower[2], upper[2])
            ],
            dtype=np.float64,
        )
        homogeneous = np.concatenate(
            [
                corners,
                np.ones((corners.shape[0], 1), dtype=np.float64),
            ],
            axis=1,
        )
        camera_points = (scene.camera.world_to_camera @ homogeneous.T).T
        if np.any(camera_points[:, 2] <= 1e-8):
            return (
                {tile.index for tile in tiles},
                [
                    f"medium {medium_id} intersects the camera plane; "
                    "conservative full rerender"
                ],
            )
        pixel_h = (scene.camera.intrinsics @ camera_points[:, :3].T).T
        u = pixel_h[:, 0] / pixel_h[:, 2]
        v = pixel_h[:, 1] / pixel_h[:, 2]
        min_x = max(0.0, float(np.min(u)))
        max_x = min(float(scene.width - 1), float(np.max(u)))
        min_y = max(0.0, float(np.min(v)))
        max_y = min(float(scene.height - 1), float(np.max(v)))
        if min_x > max_x or min_y > max_y:
            continue
        tile_x0 = int(min_x // config.tile_width)
        tile_x1 = int(max_x // config.tile_width)
        tile_y0 = int(min_y // config.tile_height)
        tile_y1 = int(max_y // config.tile_height)
        for tile in tiles:
            tx = tile.x0 // config.tile_width
            ty = tile.y0 // config.tile_height
            if tile_x0 <= tx <= tile_x1 and tile_y0 <= ty <= tile_y1:
                affected.add(tile.index)
        reasons.append(
            f"medium {medium_id} projected to tile columns "
            f"{tile_x0}..{tile_x1}, rows {tile_y0}..{tile_y1}"
        )
    return affected, reasons


def affected_tile_indices(
    old_scene: Scene,
    new_scene: Scene,
    old_config: RenderConfig,
    new_config: RenderConfig,
    change: Change,
) -> tuple[set[int], list[str], bool]:
    all_tiles = {tile.index for tile in make_tiles(new_scene, new_config)}
    if old_scene.scene_id != new_scene.scene_id:
        return all_tiles, ["scene id changed"], True
    if (
        old_scene.width != new_scene.width
        or old_scene.height != new_scene.height
        or not np.array_equal(
            old_scene.camera.intrinsics, new_scene.camera.intrinsics
        )
        or not np.array_equal(
            old_scene.camera.world_to_camera,
            new_scene.camera.world_to_camera,
        )
    ):
        return (
            all_tiles,
            ["camera/image geometry changed; conservative full rerender"],
            True,
        )
    if (
        not np.array_equal(
            old_scene.light.direction, new_scene.light.direction
        )
        or not np.array_equal(
            old_scene.light.color, new_scene.light.color
        )
        or old_scene.light.intensity != new_scene.light.intensity
    ):
        return (
            all_tiles,
            ["lighting changed; conservative full rerender"],
            True,
        )
    if old_config.digest() != new_config.digest():
        return (
            all_tiles,
            ["render/budget configuration changed; cache cannot be reused"],
            True,
        )
    if {medium.medium_id for medium in old_scene.media} != {
        medium.medium_id for medium in new_scene.media
    }:
        return (
            all_tiles,
            ["medium topology changed; conservative full rerender"],
            True,
        )
    if {block.block_id for block in old_scene.density_blocks} != {
        block.block_id for block in new_scene.density_blocks
    }:
        return (
            all_tiles,
            ["density block topology changed; conservative full rerender"],
            True,
        )
    if change.kind == "camera":
        return all_tiles, ["camera changed; all screen tiles affected"], True
    if change.kind == "light":
        return (
            all_tiles,
            ["directional light changed; all shadow rays affected"],
            True,
        )
    if change.kind == "density":
        if change.block_id is None:
            return all_tiles, ["density block id missing"], True
        old_media = {
            block.medium_id
            for block in old_scene.density_blocks
            if block.block_id == change.block_id
        }
        new_media = {
            block.medium_id
            for block in new_scene.density_blocks
            if block.block_id == change.block_id
        }
        medium_ids = old_media | new_media
        if not medium_ids:
            return (
                all_tiles,
                [
                    f"density block {change.block_id} is absent; "
                    "conservative full rerender"
                ],
                True,
            )
        new_affected, new_reasons = _projected_tile_indices(
            new_scene, new_config, medium_ids
        )
        old_affected, old_reasons = _projected_tile_indices(
            old_scene, old_config, medium_ids
        )
        reasons = [
            f"new: {reason}" for reason in new_reasons
        ] + [f"old: {reason}" for reason in old_reasons]
        affected = new_affected | old_affected
        return (
            affected,
            reasons,
            any("conservative full rerender" in reason for reason in reasons),
        )
    if change.kind == "medium":
        if change.medium_id is None:
            return all_tiles, ["medium id missing"], True
        new_affected, new_reasons = _projected_tile_indices(
            new_scene, new_config, {change.medium_id}
        )
        old_affected, old_reasons = _projected_tile_indices(
            old_scene, old_config, {change.medium_id}
        )
        reasons = [
            f"new: {reason}" for reason in new_reasons
        ] + [f"old: {reason}" for reason in old_reasons]
        affected = new_affected | old_affected
        return (
            affected,
            reasons,
            any("conservative full rerender" in reason for reason in reasons),
        )
    return (
        all_tiles,
        [f"unknown change kind {change.kind}; conservative full rerender"],
        True,
    )


def _tile_hash_map(artifact: RenderArtifact) -> dict[int, str]:
    return {
        result.tile.index: result.color_hash()
        for result in artifact.tiles
    }


def _freeze_artifact(artifact: RenderArtifact) -> None:
    artifact.color.setflags(write=False)
    artifact.transmittance.setflags(write=False)
    for result in artifact.tiles:
        result.color.setflags(write=False)
        result.transmittance.setflags(write=False)


class VersionedRenderService:
    """One active render per scene; immutable all-or-nothing publication."""

    def __init__(self, seam_check: bool = True):
        self._versions: dict[str, PublishedVersion] = {}
        self._locks: dict[str, threading.Lock] = {}
        self._store_lock = threading.RLock()
        self._seam_check = seam_check

    def _scene_lock(self, scene_id: str) -> threading.Lock:
        with self._store_lock:
            return self._locks.setdefault(scene_id, threading.Lock())

    def _acquire(self, scene_id: str) -> threading.Lock:
        lock = self._scene_lock(scene_id)
        if not lock.acquire(blocking=False):
            raise AppError(
                ErrorCode.E_BUSY,
                "a render for this scene is already active",
                field="scene_id",
                details={"scene_id": scene_id},
            )
        return lock

    def query(
        self, scene_id: str, version: int | None = None
    ) -> PublishedVersion:
        with self._store_lock:
            published = self._versions.get(scene_id)
            if published is None:
                raise AppError(
                    ErrorCode.E_USAGE,
                    "scene has no published version",
                    field="scene_id",
                    details={"scene_id": scene_id},
                )
            if version is not None and published.version != version:
                raise AppError(
                    ErrorCode.E_USAGE,
                    "requested version is not the published version",
                    field="version",
                    details={
                        "requested": version,
                        "published": published.version,
                    },
                )
            return published

    def render_initial(
        self,
        scene: Scene,
        config: RenderConfig,
        precision: str = "float64",
        *,
        completed: dict[int, TileResult] | None = None,
    ) -> PublishedVersion:
        lock = self._acquire(scene.scene_id)
        try:
            artifact = render_frame(
                scene,
                config,
                precision,
                completed=completed,
            )
            if self._seam_check:
                check_seams(artifact)
            _freeze_artifact(artifact)
            publication = {
                "mode": "initial",
                "scene_id": scene.scene_id,
                "precision": precision,
                "tile_hashes": _tile_hash_map(artifact),
            }
            with self._store_lock:
                previous_version = self._versions.get(scene.scene_id)
                version = (
                    previous_version.version + 1
                    if previous_version is not None
                    else 1
                )
                published = PublishedVersion(
                    version=version,
                    scene_id=scene.scene_id,
                    artifact=artifact,
                    tile_hashes=_tile_hash_map(artifact),
                    publication=publication,
                )
                self._versions[scene.scene_id] = published
                return published
        finally:
            lock.release()

    def render_change(
        self,
        old_scene: Scene,
        new_scene: Scene,
        old_config: RenderConfig,
        new_config: RenderConfig,
        change: Change,
        precision: str = "float64",
    ) -> PublishedVersion:
        scene_id = new_scene.scene_id
        lock = self._acquire(scene_id)
        try:
            with self._store_lock:
                old_published = self._versions.get(scene_id)
            if old_published is None:
                raise AppError(
                    ErrorCode.E_USAGE,
                    "incremental render requires an existing version",
                    field="scene_id",
                    details={"scene_id": scene_id},
                )
            affected, reasons, conservative = affected_tile_indices(
                old_scene,
                new_scene,
                old_config,
                new_config,
                change,
            )
            reused = {
                result.tile.index: result
                for result in old_published.artifact.tiles
                if result.tile.index not in affected
            }
            artifact = render_frame(
                new_scene,
                new_config,
                precision,
                completed=reused,
                selected_tile_indices=affected,
            )
            if self._seam_check:
                check_seams(artifact)
            reuse_report = {
                "mode": "incremental",
                "change": change.to_dict(),
                "affected_tiles": sorted(affected),
                "reused_tiles": sorted(reused),
                "conservative": conservative,
                "reasons": reasons,
            }
            artifact.summary = dict(artifact.summary)
            artifact.summary["incremental"] = reuse_report
            artifact.trace.append(
                {
                    "event": "incremental_publish",
                    "affected_tiles": sorted(affected),
                    "reused_tiles": sorted(reused),
                    "conservative": conservative,
                    "reasons": reasons,
                }
            )
            _freeze_artifact(artifact)
            with self._store_lock:
                version = old_published.version + 1
                published = PublishedVersion(
                    version=version,
                    scene_id=scene_id,
                    artifact=artifact,
                    tile_hashes=_tile_hash_map(artifact),
                    publication=reuse_report,
                )
                self._versions[scene_id] = published
                return published
        finally:
            lock.release()


def content_hash(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()
