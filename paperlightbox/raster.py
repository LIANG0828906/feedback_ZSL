"""Tiled deterministic software rasterization of the paper-cut lightbox.

Rendering happens in screen tiles so progress can be snapshotted and resumed.
Each fragment is shaded with fiber-modulated transmission of the point light
(area-light disk samples create a soft penumbra whose width grows with the
air gap), a fiber-tinted Lambert diffuse term plus an ambient floor, and
premultiplied-alpha back-to-front compositing whose order is the declared
layer index rather than the input list order.
"""

from __future__ import annotations

import numpy as np

from .geometry import grid_ray_hit
from .rng import lattice_hash
from .scene import ordered_layers
from .texture import apply_fiber

GOLDEN_ANGLE = np.pi * (3.0 - np.sqrt(5.0))


def camera_basis(scene: dict):
    cam = scene["camera"]
    eye = np.asarray(cam["position"], dtype=np.float64)
    forward = np.asarray(cam["target"], dtype=np.float64) - eye
    forward /= np.linalg.norm(forward)
    up = np.asarray(cam.get("up", [0.0, 1.0, 0.0]), dtype=np.float64)
    right = np.cross(forward, up)
    right /= np.linalg.norm(right)
    true_up = np.cross(right, forward)
    return eye, forward, right, true_up


def camera_rays(scene: dict, xs: np.ndarray, ys: np.ndarray):
    """Return ray origins and unit directions for pixel center coordinates."""

    image = scene["image"]
    width, height = int(image["width"]), int(image["height"])
    eye, forward, right, up = camera_basis(scene)
    aspect = width / height
    half_tan = np.tan(np.deg2rad(float(scene["camera"]["fov_y_deg"])) * 0.5)

    ndc_x = (xs.astype(np.float64) + 0.5) / width * 2.0 - 1.0
    ndc_y = 1.0 - (ys.astype(np.float64) + 0.5) / height * 2.0
    dirs = (forward[None, :]
            + right[None, :] * (ndc_x[:, None] * aspect * half_tan)
            + up[None, :] * (ndc_y[:, None] * half_tan))
    dirs /= np.linalg.norm(dirs, axis=1, keepdims=True)
    origins = np.broadcast_to(eye, dirs.shape).copy()
    return origins, dirs


def soft_shadow_fraction(points: np.ndarray, layer_index: int,
                         layers: list, light: dict,
                         sample_density: int, pixel_seeds: np.ndarray):
    """Fraction of the area light reaching each paper point.

    Only layers strictly inside the shaded layer (smaller index) can occlude.
    A disk sample that misses every inner paper region (hole or outside the
    paper rectangle) counts as fully unoccluded, so edges never gain a hard
    black rim.
    """

    n = points.shape[0]
    light_pos = np.asarray(light["position"], dtype=np.float64)
    radius = float(light["radius"])
    count = int(sample_density)
    if layer_index == 0 or radius <= 0.0:
        return np.ones(n, dtype=np.float64)

    rotation = (pixel_seeds * 2.618033988749895) % (2.0 * np.pi)
    accum = np.zeros(n, dtype=np.float64)
    for sample_idx in range(count):
        frac = (sample_idx + 0.5) / count
        angle = sample_idx * GOLDEN_ANGLE + rotation
        radial = radius * np.sqrt(frac)
        sample_pos = light_pos[None, :] + np.stack(
            [radial * np.cos(angle), radial * np.sin(angle),
             np.zeros(n)], axis=1)

        dirs = points - sample_pos
        dirs /= np.linalg.norm(dirs, axis=1, keepdims=True)
        blocked = np.zeros(n, dtype=bool)
        for inner in layers[:layer_index]:
            hit = grid_ray_hit(sample_pos, dirs, inner["mesh"])
            blocked |= hit["hit"]
        accum += (~blocked).astype(np.float64)
    return accum / count


def tone_map(value: np.ndarray) -> np.ndarray:
    """Monotonic soft-knee response; output stays strictly below 1."""

    return value / (1.0 + value)


def shade_layer_points(layer: dict, points: np.ndarray,
                       incidence: np.ndarray, shadow: np.ndarray,
                       scene: dict):
    light = scene["light"]
    light_pos = np.asarray(light["position"], dtype=np.float64)
    distance = np.linalg.norm(points - light_pos[None, :], axis=1)
    intensity = float(light["intensity"]) / np.maximum(distance, 1e-6) ** 2

    effective_trans, tinted_color = apply_fiber(
        float(layer["transmittance"]),
        np.asarray(layer["scatter_color"], dtype=np.float64),
        points, int(layer["seed"]))

    irradiance = intensity * shadow
    lit = irradiance * np.clip(incidence, 0.0, 1.0)
    transmitted = effective_trans * lit
    ambient = float(scene["render"].get("ambient", 0.1))
    diffuse = tinted_color * (ambient + (1.0 - ambient) * lit)[:, None]

    # Transmitted light picks up the paper tint, keeping inner layers red
    # rather than washing out to white.
    rgb_linear = diffuse * (1.0 - effective_trans[:, None] * 0.5) \
        + tinted_color * transmitted[:, None]
    return tone_map(rgb_linear), effective_trans, tinted_color


def render_tile(scene: dict, x0: int, y0: int, tile_w: int, tile_h: int) -> dict:
    """Render one screen tile; returns float RGBA and optical depth buffers."""

    image = scene["image"]
    width, height = int(image["width"]), int(image["height"])
    x1, y1 = min(x0 + tile_w, width), min(y0 + tile_h, height)
    tw, th = x1 - x0, y1 - y0
    pixel_count = tw * th

    ys, xs = np.mgrid[y0:y1, x0:x1]
    flat_x, flat_y = xs.reshape(-1), ys.reshape(-1)
    origins, dirs = camera_rays(scene, flat_x, flat_y)
    layers = ordered_layers(scene)
    light = scene["light"]
    sample_density = int(scene["render"]["sample_density"])

    pixel_seeds = lattice_hash(flat_x, flat_y, np.zeros_like(flat_x),
                               int(scene.get("seed", 7)) + 17)

    composite = np.zeros((pixel_count, 4), dtype=np.float64)
    background = np.asarray(image.get("background", [0.0, 0.0, 0.0, 1.0]),
                            dtype=np.float64)
    composite[:, :] = background[None, :]
    optical_depth = np.zeros(pixel_count, dtype=np.float64)
    layer_stats = {}

    # Back to front: inner layer first, outer layers over-composite it.
    for layer in layers:
        index = int(layer["index"])
        hit = grid_ray_hit(origins, dirs, layer["mesh"])
        solid = hit["hit"]
        visible = int(solid.sum())
        trans_values = np.zeros(pixel_count, dtype=np.float64)
        if visible == 0:
            layer_stats[index] = {"visible_fragments": 0,
                                  "mean_transmittance": 0.0,
                                  "transmittance_sum": 0.0}
            continue

        pts = hit["point"][solid]
        incidence = -dirs[solid, 2]
        seeds = pixel_seeds[solid]
        shadow = soft_shadow_fraction(
            pts, index, layers, light, sample_density, seeds)
        rgb, effective_trans, _ = shade_layer_points(
            layer, pts, incidence, shadow, scene)
        trans_values[solid] = effective_trans

        layer_rgba = np.zeros((pixel_count, 4), dtype=np.float64)
        layer_rgba[solid, :3] = rgb
        layer_rgba[solid, 3] = 1.0

        over = solid.astype(np.float64)[:, None]
        composite = layer_rgba * over + composite * (1.0 - over)
        optical_depth += np.where(
            solid, -np.log(np.clip(trans_values, 1e-6, 1.0)), 0.0)
        layer_stats[index] = {
            "visible_fragments": visible,
            "mean_transmittance": float(effective_trans.mean()),
            "transmittance_sum": float(effective_trans.sum()),
        }

    composite = np.clip(composite, 0.0, 1.0)
    return {
        "rgba": composite.reshape(th, tw, 4),
        "optical_depth": optical_depth.reshape(th, tw),
        "layer_stats": layer_stats,
        "tile_x": x0,
        "tile_y": y0,
    }


def tile_ranges(width: int, height: int, tile_w: int, tile_h: int):
    ranges = []
    for y0 in range(0, height, tile_h):
        for x0 in range(0, width, tile_w):
            ranges.append((x0, y0,
                           min(tile_w, width - x0),
                           min(tile_h, height - y0)))
    return ranges


def tile_id(x0: int, y0: int, tile_w: int, tile_h: int,
            width: int, height: int) -> int:
    columns = (width + tile_w - 1) // tile_w
    return (y0 // tile_h) * columns + (x0 // tile_w)
