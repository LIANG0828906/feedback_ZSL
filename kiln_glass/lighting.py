"""Layered transmission, stress-fringe scattering and multi-light soft
shadow computation.

All sampling is deterministic: golden-spiral disc samples plus hash jitter
streams keyed by (light id, pixel index, sample index), so repeated runs
and independently recomputed tiles agree byte for byte.
"""

from __future__ import annotations

import numpy as np

from . import xrand
from .fringes import FringeParams, evaluate_fringes, generate_fringe_params, scatter_color
from .geometry import intersect_aabbs
from .model import Brick, PointLight, Scene

EPS = 1e-6


def fringe_cache(scene: Scene) -> dict[int, FringeParams]:
    """Precompute (or independently recompute) fringe tables for all bricks."""
    return {
        brick.brick_id: generate_fringe_params(
            brick.fringe_seed, brick.brick_id, scene.config.bubble_count
        )
        for brick in scene.bricks
    }


def soft_visibility(
    points: np.ndarray,
    light: PointLight,
    bricks: tuple[Brick, ...],
    blocker_layer: int,
    sample_density: int,
    pixel_index: np.ndarray,
    penumbra: np.ndarray,
) -> np.ndarray:
    """Fraction of deterministic shadow-ray samples reaching ``light``.

    Only bricks on layers strictly behind the hit layer (smaller layer index)
    can block the light, matching physical layer ordering from the kiln
    chamber outward.  Out-of-receiver samples simply count as unoccluded.
    """
    n = points.shape[0]
    blockers = [b for b in bricks if b.layer < blocker_layer]
    if not blockers:
        return np.ones(n, dtype=np.float64)
    boxes_min = np.stack([b.aabb_min for b in blockers])
    boxes_max = np.stack([b.aabb_max for b in blockers])
    spiral = xrand.golden_spiral_samples(sample_density)

    to_light = light.position[None, :] - points
    base_len = np.linalg.norm(to_light, axis=1, keepdims=True)
    base_dir = to_light / np.maximum(base_len, EPS)
    # A stable surface frame built from the dominant-tangent axes.
    ref = np.array([0.0, 1.0, 0.0])
    tangent = np.cross(base_dir, ref)
    fallback = np.abs(base_dir[:, 1]) > 0.9
    tangent = np.where(
        fallback[:, None],
        np.cross(base_dir, np.array([1.0, 0.0, 0.0])),
        tangent,
    )
    tangent /= np.maximum(np.linalg.norm(tangent, axis=1, keepdims=True), EPS)
    bitangent = np.cross(base_dir, tangent)

    pixel_u64 = pixel_index.astype(np.uint64)
    sample_idx = np.arange(sample_density, dtype=np.uint64)
    # Jitter grids are fully vectorised: shape (S, N).
    jitter_x = (
        np.stack(
            [
                xrand.uniform(light.light_id, int(sample), xrand.CH_JITTER, pixel_u64)
                for sample in sample_idx
            ]
        )
        - 0.5
    )
    jitter_y = (
        np.stack(
            [
                xrand.uniform(light.light_id, int(sample), xrand.CH_JITTER_V, pixel_u64)
                for sample in sample_idx
            ]
        )
        - 0.5
    )
    spiral_x = spiral[:, 0][:, None]
    spiral_y = spiral[:, 1][:, None]
    offset_x = penumbra[None, :] * (spiral_x + jitter_x)
    offset_y = penumbra[None, :] * (spiral_y + jitter_y)

    # Expand to (S*N, 3) for one batched slab intersection.
    total = sample_density * n
    base_points = np.repeat(points[None, :, :], sample_density, axis=0)
    tangent_rep = np.repeat(tangent[None, :, :], sample_density, axis=0)
    bitangent_rep = np.repeat(bitangent[None, :, :], sample_density, axis=0)
    sample_points = (
        base_points
        + tangent_rep * offset_x[..., None]
        + bitangent_rep * offset_y[..., None]
    ).reshape(total, 3)
    directions = light.position[None, :] - sample_points
    directions /= np.maximum(np.linalg.norm(directions, axis=1, keepdims=True), EPS)
    t_table = intersect_aabbs(sample_points, directions, boxes_min, boxes_max)
    reach = np.linalg.norm(light.position[None, :] - sample_points, axis=1)
    blocked = np.any(
        np.isfinite(t_table) & (t_table < reach[:, None] - EPS), axis=1
    ).reshape(sample_density, n)
    return (~blocked).astype(np.float64).mean(axis=0)


def local_brick_coords(points: np.ndarray, brick: Brick) -> np.ndarray:
    return (points - brick.origin[None, :]) / brick.size[None, :]


def shade_brick(
    brick: Brick,
    hit_points: np.ndarray,
    view_dirs: np.ndarray,
    scene: Scene,
    cache: dict[int, FringeParams],
    pixel_index: np.ndarray,
) -> dict:
    """Compute transmission/scattering contributions for one brick's pixels."""
    params = cache[brick.brick_id]
    local = local_brick_coords(hit_points, brick)
    modulation, bubbles = evaluate_fringes(params, local[:, :2])

    normal = np.array([0.0, 0.0, 1.0])
    cos_incidence = np.clip(np.abs(np.sum(view_dirs * normal[None, :], axis=1)),
                            EPS, 1.0)
    incidence_factor = 0.55 + 0.45 * cos_incidence
    effective_thickness = brick.thickness / cos_incidence
    beer = np.exp(-(1.0 - brick.transmittance) * effective_thickness * 4.0)
    trans_factor = np.clip(beer * incidence_factor * modulation, 0.0, 1.0)

    receiver_z = scene.receiver.z
    gap_z = brick.origin[2]
    light_trans = np.zeros(hit_points.shape[0], dtype=np.float64)
    light_records = []
    for light in scene.lights:
        distance = np.linalg.norm(light.position[None, :] - hit_points, axis=1)
        falloff = light.intensity / np.maximum(distance * distance, 0.25)
        falloff = np.clip(falloff, 0.0, 1.0)
        penumbra = np.full(
            hit_points.shape[0],
            _penumbra_for(light, gap_z, receiver_z, scene.layer_of(brick.layer).grout_width),
        )
        visibility = soft_visibility(
            hit_points, light, scene.bricks, brick.layer,
            scene.config.sample_density, pixel_index, penumbra,
        )
        light_trans += falloff * visibility
        light_records.append(
            {
                "light_id": light.light_id,
                "mean_visibility": float(np.mean(visibility)),
                "penumbra_width": float(penumbra[0]),
            }
        )
    light_trans = np.clip(light_trans, 0.0, 1.0)

    # Ambient fringe floor keeps back-lit outer bricks visibly structured.
    ambient_floor = 0.10 + 0.18 * modulation
    transmitted = trans_factor * (ambient_floor + 0.9 * light_trans)
    transmitted = np.clip(transmitted, 0.0, 0.92)
    scattering_rgb = scatter_color(params, modulation)
    # Back-lit bricks get boosted fringe scattering so stripes stay legible.
    back_lit = 1.0 - np.clip(light_trans, 0.0, 1.0)
    scattering_rgb = scattering_rgb * (0.6 + 0.8 * back_lit)[:, None]
    scattering_rgb = np.clip(scattering_rgb, 0.0, 0.5)

    return {
        "brick_id": brick.brick_id,
        "layer": brick.layer,
        "transmitted": transmitted,
        "scattering": scattering_rgb,
        "modulation": modulation,
        "bubble_mask": bubbles,
        "light_records": light_records,
        "cos_incidence": cos_incidence,
    }


def _penumbra_for(light: PointLight, gap_z: float, receiver_z: float,
                  gap_width: float) -> float:
    from .geometry import penumbra_width

    return penumbra_width(light, gap_z, receiver_z, max(gap_width, 1e-3))


def composite_layer(rgba: np.ndarray, shade: dict, alpha: float,
                    mask: np.ndarray | None = None) -> dict:
    """Premultiplied-alpha ``over`` composite for one brick/layer fragment."""
    target = rgba if mask is None else rgba[mask]
    n = shade["transmitted"].shape[0]
    src_rgb = np.clip(
        np.repeat(shade["transmitted"][:, None], 3, axis=1)
        + shade["scattering"],
        0.0,
        1.0,
    )
    src_a = np.full(n, alpha, dtype=np.float64)
    premul = src_rgb * src_a[:, None]
    out_a = src_a + target[:, 3] * (1.0 - src_a)
    safe_a = np.where(out_a > EPS, out_a, 1.0)
    out_rgb = premul + target[:, :3] * (1.0 - src_a)[:, None]
    contribution_trans = float(np.sum(shade["transmitted"] * src_a))
    contribution_scat = float(np.sum(shade["scattering"] * src_a[:, None]))
    rgba_new = np.concatenate([np.clip(out_rgb, 0.0, 1.0),
                               np.clip(out_a, 0.0, 1.0)[:, None]], axis=1)
    if mask is None:
        rgba[:] = rgba_new
    else:
        rgba[mask] = rgba_new
    return {
        "brick_id": shade["brick_id"],
        "layer": shade["layer"],
        "transmission_sum": contribution_trans,
        "scattering_sum": contribution_scat,
        "mean_modulation": float(np.mean(shade["modulation"])),
        "mean_cos_incidence": float(np.mean(shade["cos_incidence"])),
        "lights": shade["light_records"],
        "safe_denominator": safe_a,
    }
