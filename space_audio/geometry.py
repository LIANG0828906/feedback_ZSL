"""Deterministic multi-layer occlusion geometry.

Every occlusion layer is generated from the scene seed plus the parameters
stored in the scene JSON. Re-running with the same scene reproduces the exact
same solids, per-segment occlusion booleans and diffraction path lengths.

Layer solids are described on an axis-aligned plane (default plane x = d).
A source->listener segment intersects the plane at point (y, z); that point is
tested against the layer's generated solids.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .rng import DeterministicRng, seed_mix

DEFAULT_BOUNDS = {"ymin": -3.0, "ymax": 3.0, "zmin": 0.0, "zmax": 4.0}


@dataclass(frozen=True)
class Circle:
    cy: float
    cz: float
    radius: float
    name: str


@dataclass(frozen=True)
class Rect:
    cy: float
    cz: float
    half_y: float
    half_z: float
    name: str


Solid = Circle | Rect


def generate_layer_geometry(layer: dict, scene_seed: int) -> dict:
    """Generate the deterministic solid list of one occlusion layer."""
    layer_type = layer["type"]
    layer_id = layer["id"]
    geom = dict(layer.get("geometry", {}))
    bounds = dict(DEFAULT_BOUNDS)
    bounds.update(geom.get("bounds", {}))
    rng = DeterministicRng(seed_mix(int(scene_seed), "geometry", layer_id))
    jitter = float(geom.get("jitter", 0.0))
    solids: list[Solid] = []

    if layer_type == "colonnade":
        count = int(geom["column_count"])
        radius = float(geom["column_radius"])
        centers = np.linspace(bounds["ymin"], bounds["ymax"], count)
        for index, center_y in enumerate(centers):
            j_y = rng.uniform_range(-jitter, jitter)
            j_z = rng.uniform_range(-jitter, jitter)
            solids.append(
                Circle(float(center_y) + j_y, (bounds["zmin"] + bounds["zmax"]) / 2 + j_z,
                       radius, f"column_{index}")
            )
    elif layer_type == "arch":
        opening_width = float(geom["opening_width"])
        opening_height = float(geom["opening_height"])
        pillar_width = float(geom["pillar_width"])
        ymin, ymax = bounds["ymin"], bounds["ymax"]
        zmax = bounds["zmax"]
        pillar_half_y = pillar_width / 2.0
        pillar_center_y = opening_width / 2.0 + pillar_half_y
        pillar_half_z = zmax / 2.0
        for side in (-1.0, 1.0):
            solids.append(
                Rect(side * pillar_center_y + rng.uniform_range(-jitter, jitter),
                     pillar_half_z, pillar_half_y, pillar_half_z, f"pillar_{'L' if side < 0 else 'R'}")
            )
        beam_center_z = opening_height + (zmax - opening_height) / 2.0
        beam_half_z = (zmax - opening_height) / 2.0
        beam_half_y = (ymax - ymin) / 2.0
        solids.append(Rect(0.0 + rng.uniform_range(-jitter, jitter), beam_center_z,
                           beam_half_y, beam_half_z, "arch_beam"))
    elif layer_type == "fence":
        count = int(geom["bar_count"])
        bar_width = float(geom["bar_width"])
        span = bounds["ymax"] - bounds["ymin"]
        spacing = span / count
        for index in range(count):
            center_y = bounds["ymin"] + spacing * (index + 0.5)
            solids.append(
                Rect(center_y + rng.uniform_range(-jitter, jitter),
                     (bounds["zmin"] + bounds["zmax"]) / 2.0,
                     bar_width / 2.0, (bounds["zmax"] - bounds["zmin"]) / 2.0,
                     f"bar_{index}")
            )
    else:  # pragma: no cover - validated earlier
        raise ValueError(f"unknown layer type {layer_type}")

    return {
        "layer_id": layer_id,
        "plane": float(layer["plane"]),
        "bounds": bounds,
        "solids": solids,
    }


def build_scene_geometry(layers: list[dict], scene_seed: int) -> list[dict]:
    return [generate_layer_geometry(layer, scene_seed) for layer in layers]


def _plane_intersection(source: np.ndarray, listener: np.ndarray, plane_x: float):
    x0, x1 = float(source[0]), float(listener[0])
    if math.isclose(x1 - x0, 0.0, abs_tol=1e-12):
        return None
    t = (plane_x - x0) / (x1 - x0)
    if t < 0.0 or t > 1.0:
        return None
    hit = source + t * (listener - source)
    return t, hit


def _circle_escape(solid: Circle, y: float, z: float) -> float:
    return abs(math.hypot(y - solid.cy, z - solid.cz) - solid.radius)


def _rect_escape(solid: Rect, y: float, z: float) -> float:
    dx = abs(y - solid.cy) - solid.half_y
    dz = abs(z - solid.cz) - solid.half_z
    if dx <= 0.0 and dz <= 0.0:
        return min(solid.half_y - abs(y - solid.cy), solid.half_z - abs(z - solid.cz))
    outside_x = max(dx, 0.0)
    outside_z = max(dz, 0.0)
    return math.hypot(outside_x, outside_z)


def _solid_escape(solid: Solid, y: float, z: float) -> float:
    if isinstance(solid, Circle):
        return _circle_escape(solid, y, z)
    return _rect_escape(solid, y, z)


def evaluate_layer(geometry: dict, source: np.ndarray, listener: np.ndarray) -> dict:
    """Return occlusion boolean and diffraction detour length for one layer."""
    plane_x = geometry["plane"]
    intersection = _plane_intersection(source, listener, plane_x)
    if intersection is None:
        return {"occluded": False, "detour": 0.0, "solid": None}
    _, hit = intersection
    y, z = float(hit[1]), float(hit[2])
    bounds = geometry["bounds"]
    if not (bounds["ymin"] <= y <= bounds["ymax"] and bounds["zmin"] <= z <= bounds["zmax"]):
        return {"occluded": False, "detour": 0.0, "solid": None}
    best = None
    best_escape = math.inf
    for solid in geometry["solids"]:
        escape = _solid_escape(solid, y, z)
        if escape < best_escape:
            best_escape = escape
            best = solid
    inside = (
        (isinstance(best, Circle) and math.hypot(y - best.cy, z - best.cz) <= best.radius)
        or (
            isinstance(best, Rect)
            and abs(y - best.cy) <= best.half_y
            and abs(z - best.cz) <= best.half_z
        )
    )
    if not inside:
        return {"occluded": False, "detour": 0.0, "solid": None}
    # Detour: travel to the solid edge and back, twice the escape distance.
    detour = 2.0 * best_escape
    return {"occluded": True, "detour": float(detour), "solid": best.name}


def evaluate_all(geometries: list[dict], source: np.ndarray, listener: np.ndarray) -> list[dict]:
    """Evaluate every layer. Order independent: one call per layer, no shared state."""
    return [evaluate_layer(geometry, source, listener) for geometry in geometries]

