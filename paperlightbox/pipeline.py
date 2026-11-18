"""Render orchestration: tiled rendering, snapshots, probes and image output."""

from __future__ import annotations

import os

import json

import numpy as np
from PIL import Image

from .geometry import grid_ray_hit, perspective_project_z
from .raster import (render_tile, soft_shadow_fraction, tile_id, tile_ranges)
from .rng import lattice_hash
from .scene import ordered_layers
from .snapshot import SnapshotStore


def _stop_requested(stop_file: str | None) -> bool:
    return bool(stop_file) and os.path.exists(stop_file)


def assemble_image(scene: dict, store: SnapshotStore,
                   completed: set) -> dict:
    width = int(scene["image"]["width"])
    height = int(scene["image"]["height"])
    rgba = np.zeros((height, width, 4), dtype=np.float64)
    depth = np.zeros((height, width), dtype=np.float64)
    stats = {}
    render = scene["render"]
    ranges = tile_ranges(width, height,
                         int(render["tile_width"]),
                         int(render["tile_height"]))
    for x0, y0, tw, th in ranges:
        tid = tile_id(x0, y0, render["tile_width"], render["tile_height"],
                      width, height)
        if tid not in completed:
            raise KeyError(f"tile {tid} missing during assembly")
        tile = store.read_tile(tid)
        rgba[y0:y0 + th, x0:x0 + tw] = tile["rgba"]
        depth[y0:y0 + th, x0:x0 + tw] = tile["optical_depth"]
        for layer, entry in tile["layer_stats"].items():
            bucket = stats.setdefault(
                layer, {"visible_fragments": 0, "transmittance_sum": 0.0})
            bucket["visible_fragments"] += int(entry["visible_fragments"])
            bucket["transmittance_sum"] += float(entry["transmittance_sum"])
    for bucket in stats.values():
        bucket["mean_transmittance"] = (
            bucket["transmittance_sum"] / bucket["visible_fragments"]
            if bucket["visible_fragments"] else 0.0)
    return {"rgba": rgba, "optical_depth": depth, "stats": stats}


def save_png(rgba: np.ndarray, path: str) -> None:
    quantized = np.round(np.clip(rgba, 0.0, 1.0) * 255.0).astype(np.uint8)
    image = Image.fromarray(quantized, mode="RGBA")
    image.save(path, format="PNG", optimize=True)


def save_depth(depth: np.ndarray, path: str) -> None:
    np.save(path, depth.astype(np.float32))


def shadow_probes(scene: dict, points_per_layer: int = 51) -> list:
    """Measure the soft edge of a fixed cutout projection on every layer.

    The innermost layer declares a deterministic tunnel of open cells shared
    by every layer.  One boundary of that tunnel is perspective-projected
    from the light onto each outer paper plane; shadow fraction is sampled
    symmetrically across the projected edge and clipped inside the paper.
    Because the same source cell is used everywhere, probe locations remain
    comparable when the air gap changes, and the measured penumbra widens as
    the gap grows.
    """

    layers = ordered_layers(scene)
    light_pos = np.asarray(scene["light"]["position"], dtype=np.float64)
    sample_density = int(scene["render"]["sample_density"])
    probes = []

    base_meta = layers[0]["mesh"]["grid"]
    nx, ny = int(base_meta["nx"]), int(base_meta["ny"])
    cell_w = (base_meta["xmax"] - base_meta["xmin"]) / nx
    cell_h = (base_meta["ymax"] - base_meta["ymin"]) / ny
    tunnel = [int(v) for v in layers[0]["mesh"].get("tunnel_cells", [])]
    if not tunnel:
        return probes
    tunnel_set = set(tunnel)

    def _open_cells(layer_index: int, target_z: float, margin: float,
                    target_meta: dict):
        result = []
        for cell in tunnel:
            cy, cx = divmod(cell, nx)
            center = np.array([
                base_meta["xmin"] + (cx + 0.5) * cell_w,
                base_meta["ymin"] + (cy + 0.5) * cell_h,
                base_meta["z"],
            ])
            projected = perspective_project_z(center, light_pos, target_z)
            if not (target_meta["xmin"] + margin <= projected[0]
                    <= target_meta["xmax"] - margin
                    and target_meta["ymin"] + margin <= projected[1]
                    <= target_meta["ymax"] - margin):
                continue
            ray_dir = projected - light_pos
            ray_dir /= np.linalg.norm(ray_dir)
            blocked = any(
                bool(grid_ray_hit(light_pos[None, :], ray_dir[None, :],
                                  layers[j]["mesh"])["hit"][0])
                for j in range(layer_index))
            if not blocked:
                result.append((cell, cx, cy, projected))
        return result

    for layer_index, layer in enumerate(layers):
        if layer_index == 0:
            continue
        target_meta = layer["mesh"]["grid"]
        target_z = float(target_meta["z"])
        span = cell_w * (4.0 + 2.0 * layer_index)
        open_cells = _open_cells(layer_index, target_z, span * 0.5 + cell_w,
                                 target_meta)
        if not open_cells:
            continue
        # Pick the first open tunnel cell that still has a solid neighbor, so
        # the sampling line crosses a genuine cutout edge.
        chosen = None
        for cell, cx, cy, projected in open_cells:
            if cx > 0 and (cy * nx + cx - 1) not in tunnel_set:
                chosen = (cell, cx, cy)
                break
        if chosen is None:
            cell, cx, cy = open_cells[0][0], open_cells[0][1], open_cells[0][2]
        else:
            cell, cx, cy = chosen
        edge_x = base_meta["xmin"] + cx * cell_w
        edge_y = base_meta["ymin"] + (cy + 0.5) * cell_h
        open_point = perspective_project_z(
            np.array([edge_x + 0.5 * cell_w, edge_y, base_meta["z"]]),
            light_pos, target_z)
        solid_point = perspective_project_z(
            np.array([edge_x - 0.5 * cell_w, edge_y, base_meta["z"]]),
            light_pos, target_z)
        edge_center = perspective_project_z(
            np.array([edge_x, edge_y, base_meta["z"]]),
            light_pos, target_z)
        direction = open_point - solid_point
        direction[2] = 0.0
        norm = np.linalg.norm(direction[:2])
        if norm < 1e-9:
            continue
        direction[:2] /= norm

        offsets = np.linspace(-span, span, points_per_layer)
        points = edge_center[None, :] + offsets[:, None] * direction[None, :]
        points[:, 0] = np.clip(points[:, 0],
                               target_meta["xmin"] + 1e-4,
                               target_meta["xmax"] - 1e-4)
        points[:, 1] = np.clip(points[:, 1],
                               target_meta["ymin"] + 1e-4,
                               target_meta["ymax"] - 1e-4)
        seeds = lattice_hash(np.arange(points_per_layer),
                             np.full(points_per_layer, layer_index),
                             np.zeros(points_per_layer),
                             int(scene.get("seed", 7)) + 17)
        shadow = soft_shadow_fraction(
            points, layer_index, layers, scene["light"],
            sample_density, seeds)
        width = _penumbra_width_world(offsets, shadow)
        probes.append({
            "layer": int(layer["index"]),
            "projected_center": [round(float(v), 6)
                                 for v in edge_center[:2]],
            "sample_points": [round(float(v), 6) for v in points[:, 0]],
            "shadow_luminance": [round(float(v), 6) for v in shadow],
            "penumbra_width_world": round(float(width), 6),
            "air_gap": float(layer["air_gap"]),
        })
    return probes


def _penumbra_width_world(xs: np.ndarray, values: np.ndarray) -> float:
    """Average width of the two soft flanks of a projected cutout opening.

    Each flank is measured from its first crossing of the lowest fractional
    disk-sample level to the first sample where the value stops changing in
    that direction, so the metric describes the soft edge itself rather than
    the full opening.  Fractional levels keep the result well defined at any
    declared sampling density.
    """

    values = np.asarray(values, dtype=np.float64)
    if float(values.max()) <= 0.0:
        return 0.0
    low = 0.5 / 9.0

    def level_cross(i: int):
        a, b = float(values[i]), float(values[i + 1])
        if a == b or (a - low) * (b - low) > 0:
            return None
        t = (low - a) / (b - a)
        return float(xs[i] + t * (xs[i + 1] - xs[i]))

    widths = []
    for i in range(len(values) - 1):
        cross = level_cross(i)
        if cross is None or values[i + 1] <= low:
            continue
        j = i + 1
        while j + 1 < len(values) and values[j + 1] > values[j]:
            j += 1
        widths.append(abs(float(xs[j]) - cross))
        break

    for i in range(len(values) - 2, -1, -1):
        cross = level_cross(i)
        if cross is None or values[i] <= low:
            continue
        j = i
        while j - 1 >= 0 and values[j - 1] > values[j]:
            j -= 1
        widths.append(abs(cross - float(xs[j])))
        break

    return float(sum(widths) / len(widths)) if widths else 0.0


def _tile_record(store: "SnapshotStore", tid: int) -> dict:
    entry = store.manifest["tiles"][str(tid)]
    return {
        "tile": tid,
        "color_digest_8bit": entry["color_digest_8bit"],
        "visible_fragments": entry["visible_fragments"],
    }


def render_scene(scene: dict, output_dir: str, snapshot_dir: str,
                 stop_file: str | None = None,
                 progress_callback=None) -> dict:
    """Render with resume support; returns a summary used by the report."""

    from .scene import validate_scene

    validate_scene(scene)
    os.makedirs(output_dir, exist_ok=True)
    os.makedirs(snapshot_dir, exist_ok=True)
    store = SnapshotStore(snapshot_dir, scene)
    completed = store.load()
    recovery_count = 1 if store.resumed else 0

    width = int(scene["image"]["width"])
    height = int(scene["image"]["height"])
    render = scene["render"]
    ranges = tile_ranges(width, height,
                         int(render["tile_width"]),
                         int(render["tile_height"]))
    tile_records = []
    stopped_gracefully = False

    for x0, y0, tw, th in ranges:
        tid = tile_id(x0, y0, render["tile_width"], render["tile_height"],
                      width, height)
        if tid in completed:
            tile_records.append(_tile_record(store, tid))
            continue
        result = render_tile(scene, x0, y0, tw, th)
        store.commit_tile(tid, result["rgba"], result["optical_depth"],
                          result["layer_stats"])
        completed.add(tid)
        tile_records.append(_tile_record(store, tid))
        if progress_callback:
            progress_callback(tid, len(ranges))
        if _stop_requested(stop_file):
            stopped_gracefully = True
            break

    assembled = None
    if len(completed) == len(ranges):
        assembled = assemble_image(scene, store, completed)
        save_png(assembled["rgba"],
                 os.path.join(output_dir, "lightbox.png"))
        save_depth(assembled["optical_depth"],
                   os.path.join(output_dir, "optical_depth.npy"))
    elif stopped_gracefully:
        recovery_report = {
            "status": "interrupted",
            "completed_tiles": sorted(completed),
            "total_tiles": len(ranges),
            "scene_digest": store.digest,
            "recovery_count": 1 if store.resumed else 0,
        }
        with open(os.path.join(output_dir, "recovery_report.json"),
                  "w", encoding="utf-8") as handle:
            json.dump(recovery_report, handle, sort_keys=True, indent=2)
            handle.write("\n")

    return {
        "store": store,
        "completed": sorted(completed),
        "total_tiles": len(ranges),
        "tile_records": tile_records,
        "assembled": assembled,
        "recovery_count": recovery_count,
        "stopped_gracefully": stopped_gracefully,
        "snapshot_dir": snapshot_dir,
        "output_dir": output_dir,
    }
