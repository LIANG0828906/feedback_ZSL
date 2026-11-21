"""Deterministic scene sample generator (no external assets)."""

import os

import numpy as np

from .canonical import canonical_hash, write_json_atomic


PROFILES = {
    "tiny": {"zones": 1, "radius": 6.0, "density": "coarse"},
    "medium": {"zones": 3, "radius": 9.0, "density": "medium"},
    "large": {"zones": 5, "radius": 12.0, "density": "fine"},
}

DENSITY = {
    "coarse": {"outline_spacing": 3.0, "shadow_spacing": 2.6, "smear_grid": 4.5},
    "medium": {"outline_spacing": 2.0, "shadow_spacing": 1.6, "smear_grid": 3.0},
    "fine": {"outline_spacing": 1.2, "shadow_spacing": 0.9, "smear_grid": 2.0},
}


def generate_samples(out_dir, seed=20261122):
    """Emit one scene per profile plus a shared baseline manifest."""
    os.makedirs(out_dir, exist_ok=True)
    index = {}
    for name, profile in PROFILES.items():
        rng = np.random.Generator(np.random.PCG64(seed + len(name)))
        scene = _build_scene(name, profile, rng)
        path = os.path.join(out_dir, f"scene_{name}.json")
        write_json_atomic(path, scene)
        index[name] = {
            "scene_file": os.path.basename(path),
            "scene_hash": canonical_hash(scene),
            "density": DENSITY[profile["density"]],
            "zone_count": profile["zones"],
            "path_count": len(scene["paths"]),
        }
    write_json_atomic(os.path.join(out_dir, "samples_index.json"), index)
    return index


def _build_scene(name, profile, rng):
    paths = []
    radius = profile["radius"]
    gap = 2.4 * radius
    palette = [
        [205, 170, 110],
        [130, 170, 190],
        [170, 130, 150],
        [150, 180, 140],
        [190, 150, 120],
    ]
    for z in range(profile["zones"]):
        cx = (z % 3) * gap
        cy = (z // 3) * gap
        sides = 5 + (z % 3)
        points = _polygon(cx, cy, radius, sides, rng)
        segments = _segments_with_quads(points, rng, quads=z % 2)
        paths.append(
            {
                "zone": f"{name}_zone_{z}",
                "order": z,
                "closed": True,
                "fill": palette[z % len(palette)],
                "segments": segments,
            }
        )
    return {"format": "npr-scene/1", "name": name, "paths": paths}


def _polygon(cx, cy, radius, sides, rng):
    angles = np.linspace(0, 2 * np.pi, sides, endpoint=False)
    wobble = rng.uniform(0.82, 1.0, size=sides)
    points = []
    for angle, scale in zip(angles, wobble):
        points.append(
            [
                round(float(cx + radius * scale * np.cos(angle)), 6),
                round(float(cy + radius * scale * np.sin(angle)), 6),
            ]
        )
    return points


def _segments_with_quads(points, rng, quads):
    segments = []
    n = len(points)
    for i in range(n):
        p0 = points[i]
        p1 = points[(i + 1) % n]
        if quads and i % 2 == 0:
            mid = [0.5 * (p0[0] + p1[0]), 0.5 * (p0[1] + p1[1])]
            ctrl = [
                round(mid[0] + float(rng.uniform(-0.6, 0.6)), 6),
                round(mid[1] + float(rng.uniform(-0.6, 0.6)), 6),
            ]
            segments.append({"type": "quad", "p0": p0, "c": ctrl, "p1": p1})
        else:
            segments.append({"type": "line", "p0": p0, "p1": p1})
    return segments
