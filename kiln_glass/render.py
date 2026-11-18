"""Tile-based deterministic renderer with resumable snapshots."""

from __future__ import annotations

import os
import signal
from dataclasses import dataclass

import numpy as np

from .fringes import FringeParams
from .geometry import grout_landing_points, mesh_owner_consistency, pixel_rays, primary_visibility
from .io_utils import (
    color_digest,
    read_ownership,
    read_pam,
    write_json,
    write_ownership,
    write_pam,
)
from .band_report import write_band_report
from .lighting import composite_layer, fringe_cache, shade_brick
from .model import Scene
from .scene import canonical_scene_bytes
from .snapshot import (
    SnapshotState,
    commit_snapshot,
    discard_snapshot,
    load_snapshot,
    scene_fingerprint,
    tile_key,
)


class GracefulInterrupt(Exception):
    """Raised after SIGINT so the current tile has already been committed."""


@dataclass
class Tile:
    index: int
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


def build_tiles(scene: Scene) -> list[Tile]:
    cfg = scene.config
    tiles: list[Tile] = []
    index = 0
    for y0 in range(0, cfg.image_height, cfg.tile_height):
        for x0 in range(0, cfg.image_width, cfg.tile_width):
            tiles.append(
                Tile(
                    index=index,
                    x0=x0,
                    y0=y0,
                    x1=min(x0 + cfg.tile_width, cfg.image_width),
                    y1=min(y0 + cfg.tile_height, cfg.image_height),
                )
            )
            index += 1
    return tiles


def scene_summary(scene: Scene) -> dict:
    return {
        "scene_seed": scene.raw["scene_seed"],
        "brick_count": len(scene.bricks),
        "layer_count": len(scene.layers),
        "light_count": len(scene.lights),
        "fringe_seeds": sorted(brick.fringe_seed for brick in scene.bricks),
        "image_size": [scene.config.image_width, scene.config.image_height],
        "tile_size": [scene.config.tile_width, scene.config.tile_height],
        "view_count": len(scene.view_angles),
    }


class Renderer:
    def __init__(self, scene: Scene, output_dir: str):
        self.scene = scene
        self.output_dir = output_dir
        self.tiles = build_tiles(scene)
        self.cache: dict[int, FringeParams] = fringe_cache(scene)
        self.fingerprint = scene_fingerprint(
            canonical_scene_bytes(scene.raw),
            [brick.fringe_seed for brick in scene.bricks],
        )
        self.summary = scene_summary(scene)
        self._interrupt_requested = False
        self._previous_handler = None
        self.interrupt_after_tiles: int | None = None
        self._tiles_committed_this_run = 0
        self.layer_stats: dict[int, dict] = {}
        os.makedirs(output_dir, exist_ok=True)

    # ------------------------------------------------------------------
    def install_sigint(self) -> None:
        self._previous_handler = signal.getsignal(signal.SIGINT)

        def handler(signum, frame):  # pragma: no cover - exercised via tests
            self._interrupt_requested = True

        signal.signal(signal.SIGINT, handler)

    def restore_sigint(self) -> None:
        if self._previous_handler is not None:
            signal.signal(signal.SIGINT, self._previous_handler)
            self._previous_handler = None

    # ------------------------------------------------------------------
    def render_tile(self, view_index: int, tile: Tile) -> dict:
        scene = self.scene
        cfg = scene.config
        yaw = scene.view_angles[view_index]
        origins_full, dirs_full = pixel_rays(
            scene.camera, yaw, cfg.image_width, cfg.image_height
        )
        origins = origins_full[tile.y0:tile.y1, tile.x0:tile.x1].reshape(-1, 3)
        directions = dirs_full[tile.y0:tile.y1, tile.x0:tile.x1].reshape(-1, 3)
        pixel_index = (
            np.arange(tile.y0, tile.y1)[:, None] * cfg.image_width
            + np.arange(tile.x0, tile.x1)[None, :]
        ).reshape(-1).astype(np.int64)

        owner_index, _, hit_points = primary_visibility(origins, directions, scene.bricks)
        rgba = np.zeros((origins.shape[0], 4), dtype=np.float64)
        background = np.array(cfg.background_rgb, dtype=np.float64)
        rgba[:, :3] = background[None, :]
        rgba[:, 3] = 0.0
        ownership = np.full(origins.shape[0], -1, dtype=np.int32)

        # Stable per-pixel processing order removes traversal-order flicker.
        unique_owners = sorted(int(v) for v in np.unique(owner_index) if v >= 0)
        stats_entries = []
        for brick_index in unique_owners:
            brick = scene.bricks[brick_index]
            mask = owner_index == brick_index
            ownership[mask] = brick.brick_id
            shade = shade_brick(
                brick,
                hit_points[mask],
                directions[mask],
                scene,
                self.cache,
                pixel_index[mask],
            )
            # Deterministic overlap handling: the nearer brick (larger layer
            # index, then smaller brick id) composites last and wins.
            alpha = 0.86
            stats = composite_layer(rgba, shade, alpha, mask)
            stats_entries.append(stats)
            bucket = self.layer_stats.setdefault(
                brick.layer,
                {"transmission_sum": 0.0, "scattering_sum": 0.0, "fragments": 0},
            )
            bucket["transmission_sum"] += stats["transmission_sum"]
            bucket["scattering_sum"] += stats["scattering_sum"]
            bucket["fragments"] += int(mask.sum())

        rgba8 = np.clip(np.round(rgba * 255.0), 0, 255).astype(np.uint8)
        rgba8 = rgba8.reshape(tile.height, tile.width, 4)
        ownership2d = ownership.reshape(tile.height, tile.width)
        digest = color_digest(rgba8)
        return {
            "rgba8": rgba8,
            "ownership": ownership2d,
            "digest": digest,
            "stats": stats_entries,
            "owner_index_flat": owner_index,
            "mesh_consistent": mesh_owner_consistency(
                origins, directions, scene.bricks, limit=32
            ),
        }

    # ------------------------------------------------------------------
    def run(self) -> dict:
        state = load_snapshot(self.output_dir, self.fingerprint, self.summary)
        restarted = state is not None
        if state is None:
            state = SnapshotState(
                fingerprint=self.fingerprint,
                scene_summary=self.summary,
                completed={},
                current_view=0,
                interrupted=False,
            )
        landing = grout_landing_points(self.scene, self.scene.receiver)
        self._write_landing(landing)

        for view_index in range(len(self.scene.view_angles)):
            os.makedirs(
                os.path.join(self.output_dir, f"view_{view_index:03d}"),
                exist_ok=True,
            )

        for attempt in range(2):
            report_views = []
            missing_tile_detected = False
            for view_index in range(len(self.scene.view_angles)):
                view_dir = os.path.join(self.output_dir, f"view_{view_index:03d}")
                image = np.zeros(
                    (self.scene.config.image_height,
                     self.scene.config.image_width, 4),
                    dtype=np.uint8,
                )
                owner_map = np.full(
                    (self.scene.config.image_height,
                     self.scene.config.image_width),
                    -1,
                    dtype=np.int32,
                )
                for tile in self.tiles:
                    key = tile_key(view_index, tile.index)
                    if key in state.completed:
                        tile_path = os.path.join(
                            view_dir, f"tile_{tile.index:03d}.pam"
                        )
                        owner_path = os.path.join(
                            view_dir, f"tile_{tile.index:03d}.kgown"
                        )
                        if not (os.path.exists(tile_path)
                                and os.path.exists(owner_path)):
                            discard_snapshot(self.output_dir)
                            missing_tile_detected = True
                            state = SnapshotState(
                                fingerprint=self.fingerprint,
                                scene_summary=self.summary,
                                completed={},
                                current_view=0,
                                interrupted=False,
                            )
                            self.layer_stats.clear()
                            break
                        image[tile.y0:tile.y1, tile.x0:tile.x1] = read_pam(tile_path)
                        owner_map[tile.y0:tile.y1, tile.x0:tile.x1] = (
                            read_ownership(owner_path)
                        )
                        continue
                    result = self.render_tile(view_index, tile)
                    if not result["mesh_consistent"]:
                        raise RuntimeError(
                            f"mesh/AABB ownership mismatch at view {view_index} "
                            f"tile {tile.index}"
                        )
                    image[tile.y0:tile.y1, tile.x0:tile.x1] = result["rgba8"]
                    owner_map[tile.y0:tile.y1, tile.x0:tile.x1] = result["ownership"]
                    write_pam(
                        os.path.join(view_dir, f"tile_{tile.index:03d}.pam"),
                        result["rgba8"],
                    )
                    write_ownership(
                        os.path.join(view_dir, f"tile_{tile.index:03d}.kgown"),
                        result["ownership"],
                    )
                    state.completed[key] = {
                        "view": view_index,
                        "tile": tile.index,
                        "bounds": [tile.x0, tile.y0, tile.x1, tile.y1],
                        "color": result["digest"],
                    }
                    state.current_view = view_index
                    commit_snapshot(self.output_dir, state)
                    self._tiles_committed_this_run += 1
                    self._check_interrupt(state, view_index)

                if missing_tile_detected:
                    break
                write_pam(os.path.join(view_dir, "image.pam"), image)
                write_ownership(
                    os.path.join(view_dir, "ownership.kgown"), owner_map
                )
                write_json(
                    os.path.join(view_dir, "landing.json"),
                    {"view_index": view_index, "landings": landing},
                )
                report_views.append(
                    {
                        "view_index": view_index,
                        "yaw": self.scene.view_angles[view_index],
                        "image": os.path.join(
                            f"view_{view_index:03d}", "image.pam"),
                        "ownership": os.path.join(
                            f"view_{view_index:03d}", "ownership.kgown"),
                    }
                )
            if not missing_tile_detected:
                break

        report = self._build_report(report_views, landing, restarted)
        write_json(os.path.join(self.output_dir, "render_report.json"), report)
        write_band_report(self.scene, self.output_dir)
        for view_index in range(len(self.scene.view_angles)):
            view_dir = os.path.join(self.output_dir, f"view_{view_index:03d}")
            for tile in self.tiles:
                for suffix in (".pam", ".kgown"):
                    tile_file = os.path.join(
                        view_dir, f"tile_{tile.index:03d}{suffix}"
                    )
                    if os.path.exists(tile_file):
                        os.remove(tile_file)
        discard_snapshot(self.output_dir)
        return report

    # ------------------------------------------------------------------
    def _check_interrupt(self, state: SnapshotState, view_index: int) -> None:
        scheduled = (
            self.interrupt_after_tiles is not None
            and self._tiles_committed_this_run >= self.interrupt_after_tiles
        )
        if self._interrupt_requested or scheduled:
            state.interrupted = True
            commit_snapshot(self.output_dir, state)
            recovery = {
                "reason": "sigint" if self._interrupt_requested else "scheduled_interrupt",
                "committed_tiles": len(state.completed),
                "current_view": view_index,
                "fingerprint": self.fingerprint,
                "status": "resumable",
            }
            write_json(os.path.join(self.output_dir, "recovery_report.json"), recovery)
            raise GracefulInterrupt("interrupt committed at tile boundary")

    def _write_landing(self, landing: list[dict]) -> None:
        write_json(
            os.path.join(self.output_dir, "grout_landings.json"),
            {"landings": landing},
        )

    def _build_report(self, report_views: list[dict],
                      landing: list[dict], restarted: bool) -> dict:
        layer_ratios = {}
        for layer_index, bucket in sorted(self.layer_stats.items()):
            total = bucket["transmission_sum"] + bucket["scattering_sum"]
            if total <= 0.0:
                trans_ratio = scat_ratio = 0.0
            else:
                trans_ratio = bucket["transmission_sum"] / total
                scat_ratio = bucket["scattering_sum"] / total
            layer_ratios[str(layer_index)] = {
                "transmission_share": round(trans_ratio, 6),
                "scattering_share": round(scat_ratio, 6),
                "fragments": bucket["fragments"],
            }
        in_bounds = sum(1 for item in landing if item["in_bounds"])
        return {
            "scene_summary": self.summary,
            "restarted_from_snapshot": restarted,
            "views": report_views,
            "layer_contribution_ratios": layer_ratios,
            "landing_count": len(landing),
            "landing_in_bounds": in_bounds,
            "sample_density": self.scene.config.sample_density,
        }
