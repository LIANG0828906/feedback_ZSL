"""Dependency-aware incremental rendering with atomic snapshot publication."""

from __future__ import annotations

import threading
import copy
from dataclasses import dataclass, field
from typing import Any, Iterable

import numpy as np

from .binary import build_scene, validate_scene
from .config import RenderConfig, validate_config
from .errors import BudgetError, BusinessError, ErrorCode
from .geometry import aabb_corners
from .hashing import hash_arrays, hash_json, sha256_bytes
from .model import Medium, Scene
from .renderer import (
    RenderResult,
    SeamCheck,
    TileDependencies,
    TileResult,
    _check_seams,
    _core_summary,
    _execute_admissions,
    _render_truth,
    _stitch,
    _tone_map_ppm,
    medium_hash,
)
from .scheduler import ScheduleResult, Tile, partition_screen, schedule_tiles


@dataclass(frozen=True)
class ChangeSet:
    camera_changed: bool = False
    light_changed: bool = False
    medium_material_changed: frozenset[int] = frozenset()
    medium_geometry_changed: frozenset[int] = frozenset()
    density_chunks_changed: frozenset[tuple[int, int, int, int]] = frozenset()
    topology_changed: bool = False
    reason: str = ""

    def is_empty(self) -> bool:
        return not (
            self.camera_changed
            or self.light_changed
            or self.medium_material_changed
            or self.medium_geometry_changed
            or self.density_chunks_changed
            or self.topology_changed
        )


@dataclass(frozen=True)
class Snapshot:
    version: int
    scene: Scene
    config: RenderConfig
    scene_hash: str
    tile_results: tuple[TileResult, ...]
    image_ppm: bytes
    summary: dict[str, Any]
    trace: tuple[dict[str, Any], ...]

    def tile_map(self) -> dict[str, TileResult]:
        return {result.tile.tile_id: result for result in self.tile_results}


def _medium_parameters_hash(medium: Medium) -> str:
    return hash_json(
        {
            "sigma_s": medium.sigma_s.tolist(),
            "sigma_a": medium.sigma_a.tolist(),
            "g": medium.g,
            "density_scale": medium.density_scale,
            "density_field_id": medium.density_field_id,
        }
    )


def _medium_geometry_hash(medium: Medium) -> str:
    return hash_json(
        {
            "kind": medium.kind,
            "shape_a": medium.shape_a.tolist(),
            "shape_b": medium.shape_b.tolist(),
        }
    )


def infer_changes(old: Scene, new: Scene) -> ChangeSet:
    topology = False
    old_ids = sorted(old.media_by_id())
    new_ids = sorted(new.media_by_id())
    if old_ids != new_ids or sorted(old.density_fields) != sorted(new.density_fields):
        topology = True
    camera_changed = hash_arrays(
        [
            ("intrinsics", old.camera.intrinsics),
            ("world_to_camera", old.camera.world_to_camera),
        ]
    ) != hash_arrays(
        [
            ("intrinsics", new.camera.intrinsics),
            ("world_to_camera", new.camera.world_to_camera),
        ]
    )
    light_changed = hash_arrays(
        [("direction", old.light.direction), ("radiance", old.light.radiance)]
    ) != hash_arrays(
        [("direction", new.light.direction), ("radiance", new.light.radiance)]
    )
    old_media = old.media_by_id()
    new_media = new.media_by_id()
    material: set[int] = set()
    geometry: set[int] = set()
    for medium_id in set(old_ids) & set(new_ids):
        if _medium_parameters_hash(old_media[medium_id]) != _medium_parameters_hash(
            new_media[medium_id]
        ):
            material.add(medium_id)
        if _medium_geometry_hash(old_media[medium_id]) != _medium_geometry_hash(
            new_media[medium_id]
        ):
            geometry.add(medium_id)
    changed_chunks: set[tuple[int, int, int, int]] = set()
    field_ids = set(old.density_fields) | set(new.density_fields)
    for field_id in field_ids:
        old_field = old.density_fields.get(field_id)
        new_field = new.density_fields.get(field_id)
        if old_field is None or new_field is None:
            topology = True
            continue
        if (
            old_field.dims != new_field.dims
            or old_field.chunk_shape != new_field.chunk_shape
        ):
            topology = True
            continue
        keys = set(old_field.chunks) | set(new_field.chunks)
        for key in keys:
            old_chunk = old_field.chunks.get(key)
            new_chunk = new_field.chunks.get(key)
            if old_chunk is None or new_chunk is None:
                changed_chunks.add((field_id, key[0], key[1], key[2]))
            elif hash_arrays([("chunk", old_chunk)]) != hash_arrays(
                [("chunk", new_chunk)]
            ):
                changed_chunks.add((field_id, key[0], key[1], key[2]))
    if (
        camera_changed
        or light_changed
        or material
        or geometry
        or changed_chunks
        or topology
        or old.width != new.width
        or old.height != new.height
    ):
        topology = topology or old.width != new.width or old.height != new.height
    return ChangeSet(
        camera_changed=camera_changed,
        light_changed=light_changed,
        medium_material_changed=frozenset(sorted(material)),
        medium_geometry_changed=frozenset(sorted(geometry)),
        density_chunks_changed=frozenset(sorted(changed_chunks)),
        topology_changed=topology,
        reason="inferred_from_scene_diff",
    )


def _project_bounds_to_tiles(
    scene: Scene,
    lo: np.ndarray,
    hi: np.ndarray,
    tile_size: int,
) -> set[int] | None:
    camera_origin = scene.camera.origin
    if np.all(camera_origin >= lo - 1e-12) and np.all(camera_origin <= hi + 1e-12):
        return None
    pixels: list[tuple[float, float]] = []
    for corner in aabb_corners(lo, hi):
        projected = scene.camera.project(corner)
        if projected is None:
            return None
        pixels.append((projected[0], projected[1]))
    xs = [item[0] for item in pixels]
    ys = [item[1] for item in pixels]
    min_x = max(0, int(np.floor(min(xs) / tile_size)))
    max_x = min(
        (scene.width - 1) // tile_size,
        int(np.floor(max(xs) / tile_size)),
    )
    min_y = max(0, int(np.floor(min(ys) / tile_size)))
    max_y = min(
        (scene.height - 1) // tile_size,
        int(np.floor(max(ys) / tile_size)),
    )
    if max_x < min_x or max_y < min_y:
        return set()
    tiles_x = (scene.width + tile_size - 1) // tile_size
    return {
        tile_y * tiles_x + tile_x
        for tile_y in range(min_y, max_y + 1)
        for tile_x in range(min_x, max_x + 1)
    }


def _chunk_world_bounds(
    scene: Scene, field_id: int, chunk_key: tuple[int, int, int]
) -> list[tuple[np.ndarray, np.ndarray]]:
    field = scene.density_fields[field_id]
    nx, ny, nz = field.dims
    cx, cy, cz = field.chunk_shape
    counts = field.chunk_count()
    extents = (
        min(cx, nx - chunk_key[0] * cx),
        min(cy, ny - chunk_key[1] * cy),
        min(cz, nz - chunk_key[2] * cz),
    )
    normalised: list[tuple[float, float]] = []
    for axis in range(3):
        start = chunk_key[axis] * field.chunk_shape[axis]
        end = start + extents[axis] - 1
        denominator = max(field.dims[axis] - 1, 1)
        u0 = max(0.0, (start - 0.5) / denominator)
        u1 = min(1.0, (end + 0.5) / denominator)
        normalised.append((u0, u1))
    results: list[tuple[np.ndarray, np.ndarray]] = []
    for medium in scene.media:
        if medium.density_field_id != field_id:
            continue
        lo, hi = medium.bounds
        span = hi - lo
        c_lo = np.array(
            [lo[axis] + normalised[axis][0] * span[axis] for axis in range(3)],
            dtype=np.float64,
        )
        c_hi = np.array(
            [lo[axis] + normalised[axis][1] * span[axis] for axis in range(3)],
            dtype=np.float64,
        )
        results.append((c_lo, c_hi))
    return results


class IncrementalRenderer:
    """Versioned renderer whose published snapshot is replaced atomically."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._snapshot: Snapshot | None = None
        self._version = 0

    def get_snapshot(self) -> Snapshot | None:
        with self._lock:
            return self._snapshot

    def render(
        self,
        scene: Scene,
        config: RenderConfig,
        changes: ChangeSet | None = None,
        validate_seams: bool = False,
        cancel_event: threading.Event | None = None,
    ) -> RenderResult:
        validate_scene(scene)
        validate_config(config)
        current_scene_hash = sha256_bytes(build_scene(scene))
        with self._lock:
            previous = self._snapshot
        tiles = partition_screen(scene.width, scene.height, config.tile_size)
        all_by_seq = {tile.sequence: tile for tile in tiles}
        all_by_id = {tile.tile_id: tile for tile in tiles}
        reasons: list[str] = []
        affected: set[int]

        if previous is None:
            affected = set(all_by_seq)
            reasons.append("no_previous_snapshot")
            reuse_map: dict[str, TileResult] = {}
        else:
            inferred = None
            same_object_mutated = (
                previous.scene is scene and current_scene_hash != previous.scene_hash
            )
            if changes is None:
                inferred = infer_changes(previous.scene, scene)
                changes = inferred
                if changes.reason:
                    reasons.append(changes.reason)
            else:
                inferred = infer_changes(previous.scene, scene)
            if config.content_hash != previous.config.content_hash:
                affected = set(all_by_seq)
                reasons.append("render_config_changed")
                reuse_map = {}
            elif same_object_mutated:
                affected = set(all_by_seq)
                reasons.append("scene_object_mutated_in_place")
                reuse_map = {}
            elif inferred is not None and (
                (inferred.camera_changed and not changes.camera_changed)
                or (inferred.light_changed and not changes.light_changed)
                or (
                    inferred.medium_material_changed
                    - changes.medium_material_changed
                )
                or (
                    inferred.medium_geometry_changed
                    - changes.medium_geometry_changed
                )
                or (
                    inferred.density_chunks_changed
                    - changes.density_chunks_changed
                )
                or (inferred.topology_changed and not changes.topology_changed)
            ):
                affected = set(all_by_seq)
                reasons.append("unclassified_scene_change")
                reuse_map = {}
            elif changes.topology_changed or scene.width != previous.scene.width or scene.height != previous.scene.height:
                affected = set(all_by_seq)
                reasons.append("scene_topology_changed")
                reuse_map = {}
            elif changes.camera_changed:
                affected = set(all_by_seq)
                reasons.append("camera_changed")
                reuse_map = {}
            elif changes.light_changed:
                affected = set(all_by_seq)
                reasons.append("light_changed")
                reuse_map = {}
            elif changes.is_empty() and sha256_bytes(build_scene(scene)) != sha256_bytes(
                build_scene(previous.scene)
            ):
                affected = set(all_by_seq)
                reasons.append("unclassified_scene_change")
                reuse_map = {}
            else:
                old_map = previous.tile_map()
                if set(old_map) != set(all_by_id):
                    affected = set(all_by_seq)
                    reasons.append("tile_layout_changed")
                    reuse_map = {}
                else:
                    reuse_map = dict(old_map)
                    affected = set()
                    unknown = False
                    for medium_id in sorted(
                        changes.medium_material_changed
                        | changes.medium_geometry_changed
                    ):
                        if medium_id not in previous.scene.media_by_id() and medium_id not in scene.media_by_id():
                            unknown = True
                            break
                        for tile_id, result in old_map.items():
                            if medium_id in result.dependencies.medium_hashes:
                                affected.add(result.tile.sequence)
                    if unknown:
                        affected = set(all_by_seq)
                        reasons.append("unknown_medium_change")
                        reuse_map = {}
                    else:
                        if changes.medium_material_changed:
                            reasons.append("medium_material_changed")
                        if changes.medium_geometry_changed:
                            reasons.append("medium_geometry_changed")
                            old_media = previous.scene.media_by_id()
                            new_media = scene.media_by_id()
                            projection_failed = False
                            for medium_id in changes.medium_geometry_changed:
                                for source_scene, media_map in (
                                    (previous.scene, old_media),
                                    (scene, new_media),
                                ):
                                    medium = media_map.get(medium_id)
                                    if medium is None:
                                        continue
                                    lo, hi = medium.bounds
                                    projected = _project_bounds_to_tiles(
                                        source_scene, lo, hi, config.tile_size
                                    )
                                    if projected is None:
                                        projection_failed = True
                                        break
                                    affected.update(projected)
                                if projection_failed:
                                    break
                            if projection_failed:
                                affected = set(all_by_seq)
                                reasons.append("geometry_projection_unbounded")
                                reuse_map = {}
                        if changes.density_chunks_changed:
                            reasons.append("density_chunk_changed")
                            projection_failed = False
                            for field_id, cx, cy, cz in changes.density_chunks_changed:
                                world_bounds: list[tuple[np.ndarray, np.ndarray]] = []
                                for source_scene in (previous.scene, scene):
                                    field = source_scene.density_fields.get(field_id)
                                    if field is None:
                                        projection_failed = True
                                        break
                                    world_bounds.extend(
                                        _chunk_world_bounds(
                                            source_scene, field_id, (cx, cy, cz)
                                        )
                                    )
                                if projection_failed:
                                    break
                                for lo, hi in world_bounds:
                                    projected = _project_bounds_to_tiles(
                                        scene, lo, hi, config.tile_size
                                    )
                                    if projected is None:
                                        projection_failed = True
                                        break
                                    affected.update(projected)
                                if projection_failed:
                                    break
                            for tile_id, result in old_map.items():
                                if any(
                                    key in result.dependencies.density_chunk_hashes
                                    for key in changes.density_chunks_changed
                                ):
                                    affected.add(result.tile.sequence)
                            if projection_failed:
                                affected = set(all_by_seq)
                                reasons.append("density_projection_unbounded")
                                reuse_map = {}

        affected_tiles = [all_by_seq[sequence] for sequence in sorted(affected)]
        schedule: ScheduleResult = schedule_tiles(
            scene, affected_tiles, config, cancel_event
        )
        if schedule.waiting:
            raise BudgetError(
                "incremental update rejected; previous snapshot retained",
                {
                    "budget": config.sample_budget,
                    "waiting": [task.to_dict() for task in schedule.waiting],
                },
            )
        rendered = _execute_admissions(
            scene, schedule.admitted, config, cancel_event
        )
        rendered_map = {result.tile.tile_id: result for result in rendered}
        merged: list[TileResult] = []
        for tile in sorted(tiles, key=lambda item: item.sequence):
            if tile.tile_id in rendered_map:
                merged.append(rendered_map[tile.tile_id])
            else:
                reused = reuse_map.get(tile.tile_id)
                if reused is None:
                    raise BusinessError(
                        ErrorCode.INTERNAL,
                        "incremental render could not prove tile independence",
                        field="incremental",
                        details={"tile_id": tile.tile_id},
                    )
                merged.append(reused)
        scatter, transmittance, sample_counts = _stitch(scene, merged)
        truth: TileResult | None = None
        if validate_seams and len(merged) > 1 and config.seam.enabled:
            truth = _render_truth(scene, config, cancel_event)
            seam_check = _check_seams(scene, merged, truth, config)
        else:
            seam_check = SeamCheck(
                enabled=False,
                checked_pixels=0,
                max_scatter_delta=0.0,
                max_transmittance_delta=0.0,
                tolerance=config.seam.tolerance,
                truth_stage=config.seam.truth_stage,
                truth_samples=truth.sample_count if truth is not None else 0,
            )
        image_bytes = _tone_map_ppm(scatter)
        rendered_ids = sorted(rendered_map)
        reused_ids = sorted(
            tile.tile_id for tile in tiles if tile.tile_id not in rendered_map
        )
        extra = {
            "incremental": {
                "rendered_tiles": rendered_ids,
                "reused_tiles": reused_ids,
                "invalidated_tiles": sorted(
                    all_by_seq[sequence].tile_id for sequence in sorted(affected)
                ),
                "reasons": sorted(set(reasons)),
                "all_tiles": len(tiles),
                "conservative": any(
                    reason
                    in (
                        "camera_changed",
                        "light_changed",
                        "render_config_changed",
                        "scene_topology_changed",
                        "scene_object_mutated_in_place",
                        "unclassified_scene_change",
                        "geometry_projection_unbounded",
                        "density_projection_unbounded",
                        "unknown_medium_change",
                        "tile_layout_changed",
                    )
                    for reason in reasons
                ),
            }
        }
        summary = _core_summary(
            scene,
            config,
            merged,
            schedule,
            scatter,
            transmittance,
            sample_counts,
            seam_check,
            image_bytes,
            extra,
        )
        trace: list[dict[str, Any]] = [
            {
                "event": "incremental_plan",
                "rendered_tiles": rendered_ids,
                "reused_tiles": reused_ids,
                "reasons": sorted(set(reasons)),
            }
        ]
        trace.extend(schedule.decisions)
        for result in rendered:
            trace.append(
                {
                    "event": "tile_committed",
                    "tile_id": result.tile.tile_id,
                    "submit_seq": result.tile.sequence,
                    "stage": result.stage,
                    "step_multiplier": result.step_multiplier,
                    "degradation_index": result.degradation_index,
                    "budget": config.sample_budget,
                    "sample_count": result.sample_count,
                    "effective_base_step": config.base_step
                    * result.step_multiplier,
                    "content_hash": result.content_hash,
                }
            )
        for result in merged:
            if result.tile.tile_id in rendered_map:
                continue
            trace.append(
                {
                    "event": "tile_reused",
                    "tile_id": result.tile.tile_id,
                    "submit_seq": result.tile.sequence,
                    "stage": result.stage,
                    "step_multiplier": result.step_multiplier,
                    "sample_count": result.sample_count,
                    "content_hash": result.content_hash,
                    "reason": "dependency_hash_unchanged",
                }
            )
        trace.sort(
            key=lambda record: (
                record.get("submit_seq", -1),
                {
                    "schedule": 0,
                    "tile_committed": 1,
                    "tile_reused": 1,
                    "incremental_plan": 2,
                }.get(record["event"], 3),
            )
        )
        result = RenderResult(
            radiance=scatter,
            scatter=scatter,
            transmittance=transmittance,
            image_ppm=image_bytes,
            summary=summary,
            trace=trace,
            tile_results=merged,
            schedule=schedule,
        )
        with self._lock:
            self._version += 1
            self._snapshot = Snapshot(
                version=self._version,
                scene=copy.deepcopy(scene),
                config=config,
                scene_hash=current_scene_hash,
                tile_results=tuple(merged),
                image_ppm=image_bytes,
                summary=summary,
                trace=tuple(trace),
            )
        return result


__all__ = ["ChangeSet", "IncrementalRenderer", "Snapshot", "infer_changes"]
