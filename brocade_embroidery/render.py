"""Render orchestration, interruption handling and report generation."""

from __future__ import annotations

import json
import os
import signal
from dataclasses import dataclass

import numpy as np
from PIL import Image

from .geometry import Camera, orbit_eye
from .rasterizer import (
    LayerTables,
    component_id_map,
    prepare_layer_geometry,
    render_tile,
    tile_layout,
)
from .sampling import build_embroidery_table, build_hole_table
from .scene_gen import scene_digest
from .shadow import (
    build_receiver_field,
    floor_receiver_spec,
    layer_receiver_spec,
)
from .snapshot import SnapshotStore
from .validation import ensure_valid_scene


class ShutdownRequested(Exception):
    """Raised after SIGINT once the current tile snapshot is committed."""


@dataclass
class FrameArtifacts:
    frame_index: int
    azimuth: float
    rgba: np.ndarray
    ownership: np.ndarray
    report: dict


def build_tables(scene: dict) -> LayerTables:
    embroidery = {}
    holes = {}
    for layer in scene["layers"]:
        material = layer["material"]
        if layer["kind"] == "satin":
            embroidery[layer["layer_index"]] = build_embroidery_table(
                material["embroidery_seed"], layer["layer_index"],
                material["table_resolution"])
        else:
            holes[layer["layer_index"]] = build_hole_table(
                material["hole_seed"], layer["layer_index"],
                material["hole_table_resolution"], material["hole_rate"])
    return LayerTables(embroidery=embroidery, holes=holes)


def build_shadow_fields(scene: dict, tables: LayerTables):
    fields: dict[tuple, object] = {}
    landing = []
    sample_density = scene["frame"]["penumbra_samples"]
    field_resolution = max(128, sample_density * 12)
    layers = scene["layers"]
    for position, kesi_layer in enumerate(layers):
        if kesi_layer["kind"] != "kesi":
            continue
        hole_table = tables.holes[kesi_layer["layer_index"]]
        for receiver in layers[position + 1:]:
            spec = layer_receiver_spec(receiver, field_resolution)
            for light in scene["lights"]:
                field, points = build_receiver_field(
                    scene, kesi_layer, hole_table, spec, light,
                    sample_density)
                fields[(receiver["layer_index"], light["id"])] = field
                landing.extend(points)
        floor_spec = floor_receiver_spec(scene, field_resolution)
        for light in scene["lights"]:
            field, points = build_receiver_field(
                scene, kesi_layer, hole_table, floor_spec, light,
                sample_density)
            fields[("floor", light["id"])] = field
            landing.extend(points)
    return fields, landing


def make_camera(scene: dict, azimuth: float) -> Camera:
    camera_cfg = scene["camera"]
    target = np.asarray(camera_cfg["target"], dtype=np.float64)
    eye = orbit_eye(target, float(camera_cfg["radius"]), float(azimuth),
                    float(camera_cfg["elevation_deg"]))
    return Camera(
        eye=eye,
        target=target,
        up=np.asarray(camera_cfg["up"], dtype=np.float64),
        fov_y_deg=float(scene["frame"]["fov_y_deg"]),
        width=int(scene["frame"]["width"]),
        height=int(scene["frame"]["height"]),
    )


def _assemble(scene: dict, store: SnapshotStore, layout,
              completed: list[dict]) -> tuple[np.ndarray, np.ndarray]:
    frame = scene["frame"]
    rgba = np.zeros((frame["height"], frame["width"], 4), dtype=np.float32)
    ownership = np.zeros((frame["height"], frame["width"]), dtype=np.uint16)
    records = {block["tile_id"]: block for block in completed}
    for tile_id, tile_x, tile_y, width, height in layout:
        tile_rgba, tile_owner = store.read_tile(
            tile_id, (height, width))
        rgba[tile_y:tile_y + height, tile_x:tile_x + width] = tile_rgba
        ownership[tile_y:tile_y + height, tile_x:tile_x + width] = tile_owner
        assert records[tile_id]["tile_shape"] == [height, width]
    return rgba, ownership


def _write_image(path: str, rgba: np.ndarray) -> None:
    quantized = np.clip(np.rint(rgba * 255.0), 0, 255).astype(np.uint8)
    Image.fromarray(quantized, mode="RGBA").save(path, format="PNG")


def _write_ownership(path: str, ownership: np.ndarray,
                     component_lookup: dict[str, int]) -> None:
    raw_path = os.path.splitext(path)[0] + ".u16"
    ownership.astype("<u2").tofile(raw_path)
    maximum = max(1, max(component_lookup.values()))
    preview = np.clip(
        np.rint(ownership.astype(np.float32) / maximum * 255.0),
        0, 255).astype(np.uint8)
    Image.fromarray(preview, mode="L").save(path, format="PNG")
    sidecar = path + ".json"
    with open(sidecar, "w", encoding="utf-8") as handle:
        json.dump({"component_ids": component_lookup}, handle,
                  sort_keys=True, indent=2)


def _layer_contribution_report(stats_per_tile: list[dict]) -> dict:
    reflection = {}
    transmission = {}
    for stats in stats_per_tile:
        for key, value in stats["reflection_by_layer"].items():
            reflection[key] = reflection.get(key, 0.0) + value
        for key, value in stats["transmission_by_layer"].items():
            transmission[key] = transmission.get(key, 0.0) + value
    totals = {
        key: max(reflection.get(key, 0.0), 1e-12)
        + max(transmission.get(key, 0.0), 1e-12)
        for key in set(reflection) | set(transmission)
    }
    ratios = {}
    for key in sorted(totals, key=int):
        ratios[key] = {
            "reflection_share": round(reflection.get(key, 0.0) / totals[key], 6),
            "transmission_share": round(transmission.get(key, 0.0) / totals[key], 6),
        }
    return ratios


def _stable_statistics(stats_per_tile: list[dict], commits: int) -> dict:
    blocks = []
    total_fragments = 0
    total_reflection = 0.0
    total_samples = 0
    for stats in stats_per_tile:
        visible = int(stats["visible_fragments"])
        reflection_sum = float(stats["reflection_sum"])
        total_fragments += visible
        total_reflection += reflection_sum
        total_samples += int(stats["transmission_sample_count"])
        blocks.append({
            "tile_id": int(stats["tile_id"]),
            "visible_fragments": visible,
            "average_reflectivity": round(
                reflection_sum / max(1, visible), 6),
            "transmission_sample_points": int(
                stats["transmission_sample_count"]),
        })
    return {
        "snapshot_commit_count": int(commits),
        "total_visible_fragments": total_fragments,
        "total_transmission_sample_points": total_samples,
        "frame_average_reflectivity": round(
            total_reflection / max(1, total_fragments), 6),
        "per_block": blocks,
    }


def render_frame(scene: dict, output_dir: str, frame_index: int,
                 azimuth: float, *, allow_resume: bool = True,
                 max_tiles: int | None = None) -> FrameArtifacts:
    ensure_valid_scene(scene)
    os.makedirs(output_dir, exist_ok=True)
    frame_dir = os.path.join(output_dir, f"frame_{frame_index:02d}")
    os.makedirs(frame_dir, exist_ok=True)
    store = SnapshotStore(frame_dir, scene)
    state = store.load() if allow_resume else {
        "completed": [], "commits": 0, "restarted": False}
    if not allow_resume or state.get("restarted"):
        store.reset()
        state = {"completed": [], "commits": 0,
                 "restarted": state.get("restarted", False),
                 "discard_reason": state.get("discard_reason")}
    completed = state["completed"]
    commits = state["commits"]

    tables = build_tables(scene)
    shadow_fields, landing = [], []
    camera = component_lookup = layout = prepared = None
    done_ids = {block["tile_id"] for block in completed}
    stats_per_tile = [block["stats"] for block in completed]
    interrupted = False

    def _handle_sigint(signum, frame):  # noqa: ARG001
        raise ShutdownRequested("SIGINT received; finishing current tile")

    previous_handler = signal.signal(signal.SIGINT, _handle_sigint)
    tiles_done_after_start = 0
    try:
        shadow_fields, landing = build_shadow_fields(scene, tables)
        camera = make_camera(scene, azimuth)
        component_lookup = component_id_map(scene)
        layout = tile_layout(scene)
        prepared = [prepare_layer_geometry(layer, camera)
                    for layer in scene["layers"]]
        for tile in layout:
            tile_id = tile[0]
            if tile_id in done_ids:
                continue
            result = render_tile(scene, camera, tile, tables,
                                 shadow_fields, component_lookup, prepared)
            completed, commits = store.commit_tile(
                tile_id, (result.rgba.shape[0], result.rgba.shape[1]),
                result.rgba, result.ownership, result.stats,
                completed, commits)
            stats_per_tile.append(result.stats)
            tiles_done_after_start += 1
            if max_tiles is not None and tiles_done_after_start >= max_tiles:
                interrupted = True
                break
    except ShutdownRequested:
        interrupted = True
    finally:
        signal.signal(signal.SIGINT, previous_handler)

    if interrupted and not completed:
        # A signal before the first tile still has to produce a recovery
        # report and leave a valid empty snapshot for the next process.
        total_tiles = len(layout) if layout is not None else len(tile_layout(scene))
        recovery = {
            "frame_index": frame_index,
            "azimuth_deg": azimuth,
            "interrupted": True,
            "snapshot_restarted": state.get("restarted", False),
            "discard_reason": state.get("discard_reason"),
            "committed_snapshots": commits,
            "completed_tiles": 0,
            "total_tiles": total_tiles,
            "scene_digest": scene_digest(scene),
        }
        with open(os.path.join(frame_dir, "recovery.json"), "w",
                  encoding="utf-8") as handle:
            json.dump(recovery, handle, sort_keys=True, indent=2)
        return FrameArtifacts(frame_index, azimuth,
                              np.zeros((1, 1, 4), dtype=np.float32),
                              np.zeros((1, 1), dtype=np.uint16),
                              {"recovery": recovery, "incomplete": True})

    recovery = {
        "frame_index": frame_index,
        "azimuth_deg": azimuth,
        "interrupted": interrupted,
        "snapshot_restarted": state.get("restarted", False),
        "discard_reason": state.get("discard_reason"),
        "committed_snapshots": commits,
        "completed_tiles": len(completed),
        "total_tiles": len(layout),
        "scene_digest": scene_digest(scene),
    }
    with open(os.path.join(frame_dir, "recovery.json"), "w",
              encoding="utf-8") as handle:
        json.dump(recovery, handle, sort_keys=True, indent=2)
    if interrupted:
        return FrameArtifacts(frame_index, azimuth,
                              np.zeros((1, 1, 4), dtype=np.float32),
                              np.zeros((1, 1), dtype=np.uint16),
                              {"recovery": recovery, "incomplete": True})

    rgba, ownership = _assemble(scene, store, layout, completed)
    _write_image(os.path.join(frame_dir, "render.rgba.png"), rgba)
    _write_ownership(os.path.join(frame_dir, "ownership.png"),
                     ownership, component_lookup)
    block_report = [
        {
            "tile_id": block["tile_id"],
            "stats": block["stats"],
            "color_summary": block["summary"],
        }
        for block in completed
    ]
    report = {
        "frame_index": frame_index,
        "azimuth_deg": azimuth,
        "scene_digest": scene_digest(scene),
        "recovery": recovery,
        "blocks": block_report,
        "layer_contribution": _layer_contribution_report(stats_per_tile),
        "stable_statistics": _stable_statistics(stats_per_tile, commits),
        "landing_points": [point.as_dict() for point in landing],
        "component_ids": component_lookup,
    }
    with open(os.path.join(frame_dir, "report.json"), "w",
              encoding="utf-8") as handle:
        json.dump(report, handle, sort_keys=True, indent=2)
    return FrameArtifacts(frame_index, azimuth, rgba, ownership, report)
