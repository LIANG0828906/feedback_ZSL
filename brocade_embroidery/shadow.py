"""Kesi hole projection, soft penumbra and per-light transmission fields."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .geometry import (
    EPS,
    normalize,
    plane_normal_and_point,
    ray_plane_intersection,
)
from .rng import hash_mix
from .sampling import HoleTable, bilinear_sample


@dataclass
class ReceiverField:
    layer_index: int | str
    light_id: str
    origin: np.ndarray
    u_axis: np.ndarray
    v_axis: np.ndarray
    extent_u: float
    extent_v: float
    resolution_u: int
    resolution_v: int
    transmission: np.ndarray

    def sample(self, u: np.ndarray, v: np.ndarray) -> np.ndarray:
        return bilinear_sample(self.transmission, u, v)


@dataclass
class LandingPoint:
    light_id: str
    kesi_layer: int
    receiver: str
    hole_id: int
    world: tuple[float, float, float]
    receiver_uv: tuple[float, float]
    inside_receiver: bool
    penumbra_width: float

    def as_dict(self) -> dict:
        return {
            "light_id": self.light_id,
            "kesi_layer": self.kesi_layer,
            "receiver": self.receiver,
            "hole_id": self.hole_id,
            "world_xyz": [round(value, 6) for value in self.world],
            "receiver_uv": [round(value, 6) for value in self.receiver_uv],
            "inside_receiver": bool(self.inside_receiver),
            "penumbra_width": round(float(self.penumbra_width), 6),
        }


def layer_uv_axes(layer: dict) -> tuple[np.ndarray, np.ndarray]:
    normal = normalize(np.asarray(layer["plane"]["normal"], dtype=np.float64))
    reference = np.array([0.0, 1.0, 0.0])
    if abs(np.dot(normal, reference)) > 0.95:
        reference = np.array([1.0, 0.0, 0.0])
    u_axis = normalize(np.cross(reference, normal))
    v_axis = normalize(np.cross(normal, u_axis))
    return u_axis, v_axis


def layer_receiver_spec(layer: dict, resolution: int) -> dict:
    point = np.asarray(layer["plane"]["point"], dtype=np.float64)
    u_axis, v_axis = layer_uv_axes(layer)
    return {
        "layer_index": layer["layer_index"],
        "normal": normalize(np.asarray(layer["plane"]["normal"])),
        "point": point,
        "u_axis": u_axis,
        "v_axis": v_axis,
        "extent_u": float(layer["width"]),
        "extent_v": float(layer["height"]),
        "resolution_u": resolution,
        "resolution_v": resolution,
    }


def floor_receiver_spec(scene: dict, resolution: int) -> dict:
    normal, point = plane_normal_and_point(scene["floor"])
    u_axis = normalize(np.cross(np.array([0.0, 0.0, 1.0]), normal))
    v_axis = normalize(np.cross(normal, u_axis))
    return {
        "layer_index": "floor",
        "normal": normal,
        "point": point,
        "u_axis": u_axis,
        "v_axis": v_axis,
        "extent_u": float(scene["floor"]["extent_x"] * 2.0),
        "extent_v": float(scene["floor"]["extent_z"] * 2.0),
        "resolution_u": resolution,
        "resolution_v": resolution,
    }


def penumbra_width(light_to_kesi: float, kesi_to_receiver: float,
                   sample_density: int) -> float:
    """Stylised monotone penumbra width.

    The business rule requires the width to grow monotonically with both the
    light->Kesi and Kesi->receiver distances.  It shrinks as the declared
    sampling density grows because denser sampling can resolve narrower
    transitions.
    """
    light_to_kesi = max(0.0, float(light_to_kesi))
    kesi_to_receiver = max(0.0, float(kesi_to_receiver))
    density = max(1, int(sample_density))
    return (0.035
            + 0.010 * light_to_kesi
            + 0.028 * kesi_to_receiver) / math.sqrt(density)


def _world_to_receiver(point: np.ndarray, spec: dict) -> tuple[float, float, bool]:
    delta = point - spec["point"]
    u = float(np.dot(delta, spec["u_axis"]))
    v = float(np.dot(delta, spec["v_axis"]))
    inside = (abs(u) <= 0.5 * spec["extent_u"]
              and abs(v) <= 0.5 * spec["extent_v"])
    u_normalized = (u + 0.5 * spec["extent_u"]) / spec["extent_u"]
    v_normalized = (v + 0.5 * spec["extent_v"]) / spec["extent_v"]
    return u_normalized, v_normalized, inside


def _splat_transmission(field: np.ndarray, u: float, v: float,
                        width: float, strength: float) -> None:
    resolution_u, resolution_v = field.shape
    # The soft footprint is roughly two penumbra half-widths across.  A fixed
    # Gaussian support keeps the edge smooth without a binary cut.
    radius = max(1.0, width * resolution_u * 1.4)
    center_x = u * resolution_u - 0.5
    center_y = v * resolution_v - 0.5
    x_min = max(0, int(math.floor(center_x - radius)))
    x_max = min(resolution_u, int(math.ceil(center_x + radius)) + 1)
    y_min = max(0, int(math.floor(center_y - radius)))
    y_max = min(resolution_v, int(math.ceil(center_y + radius)) + 1)
    if x_min >= x_max or y_min >= y_max:
        return
    xs = np.arange(x_min, x_max, dtype=np.float64)
    ys = np.arange(y_min, y_max, dtype=np.float64)
    grid_x, grid_y = np.meshgrid(xs, ys)
    dx = (grid_x - center_x) / resolution_u
    dy = (grid_y - center_y) / resolution_v
    distance = np.sqrt(dx * dx + dy * dy)
    # Gaussian soft edge: no threshold cut, so no hard black boundary.  The
    # core attenuation is capped to retain floor colour instead of pure black.
    falloff = np.exp(-0.5 * (distance / max(width, EPS)) ** 2)
    block = field[y_min:y_max, x_min:x_max]
    # Multiplicative stacking preserves overlap of the two lights.  A modest
    # per-hole attenuation leaves combined shadows soft rather than pitch black.
    block *= (1.0 - min(0.32, float(strength)) * falloff)


def build_receiver_field(
    scene: dict,
    kesi_layer: dict,
    hole_table: HoleTable,
    receiver_spec: dict,
    light: dict,
    sample_density: int,
) -> tuple[ReceiverField, list[LandingPoint]]:
    """Project Kesi holes from one light onto one receiver plane."""
    light_position = np.asarray(light["position"], dtype=np.float64)
    _, kesi_point = plane_normal_and_point(kesi_layer["plane"])
    kesi_u, kesi_v = layer_uv_axes(kesi_layer)
    grid_side = max(6, hole_table.resolution // 4)
    transmittance = float(kesi_layer["material"]["transmittance"])

    field = np.ones((receiver_spec["resolution_v"],
                     receiver_spec["resolution_u"]), dtype=np.float32)
    landing_points: list[LandingPoint] = []
    receiver_normal = receiver_spec["normal"]
    receiver_point = receiver_spec["point"]

    for hole_id in range(grid_side * grid_side):
        row = hole_id // grid_side
        col = hole_id % grid_side
        selection = hash_mix(hole_table.seed, kesi_layer["layer_index"],
                              grid_side, row, col, 77)
        # Match hole-cell selection in sampling.build_hole_table.
        hole_rate = kesi_layer["material"]["hole_rate"]
        if selection % np.uint64(1000) >= np.uint64(round(hole_rate * 1000)):
            continue
        u = (col + 0.5) / grid_side
        v = (row + 0.5) / grid_side
        source = (
            kesi_point
            + (u - 0.5) * kesi_layer["width"] * kesi_u
            + (v - 0.5) * kesi_layer["height"] * kesi_v
        )
        direction = source - light_position
        hit, finite = ray_plane_intersection(
            light_position, direction[None, :],
            receiver_normal, receiver_point)
        hit = hit[0]
        if not bool(finite[0]):
            continue
        light_distance = float(np.linalg.norm(source - light_position))
        # Euclidean source->receiver distance is robust even when the Kesi and
        # receiver planes are nearly perpendicular (e.g. layer to stage floor).
        receiver_distance = float(np.linalg.norm(hit - source))
        width = penumbra_width(light_distance, abs(receiver_distance),
                               sample_density)
        receiver_u, receiver_v, inside = _world_to_receiver(hit, receiver_spec)
        landing_points.append(LandingPoint(
            light_id=light["id"],
            kesi_layer=int(kesi_layer["layer_index"]),
            receiver=str(receiver_spec["layer_index"]),
            hole_id=hole_id,
            world=tuple(float(x) for x in hit),
            receiver_uv=(receiver_u, receiver_v),
            inside_receiver=inside,
            penumbra_width=width,
        ))
        # Out-of-receiver hits are treated as unoccluded: they simply do not
        # splat inside the field, hence never create a hard edge boundary.
        if inside:
            _splat_transmission(field, receiver_u, receiver_v, width,
                                transmittance)
    field = np.clip(field, 0.18, 1.0, dtype=np.float32)
    return (
        ReceiverField(
            layer_index=receiver_spec["layer_index"],
            light_id=light["id"],
            origin=receiver_point.copy(),
            u_axis=receiver_spec["u_axis"],
            v_axis=receiver_spec["v_axis"],
            extent_u=receiver_spec["extent_u"],
            extent_v=receiver_spec["extent_v"],
            resolution_u=receiver_spec["resolution_u"],
            resolution_v=receiver_spec["resolution_v"],
            transmission=field.astype(np.float32),
        ),
        landing_points,
    )


def world_to_receiver_uv(point: np.ndarray, field: ReceiverField) -> tuple[float, float]:
    delta = point - field.origin
    u = float(np.dot(delta, field.u_axis))
    v = float(np.dot(delta, field.v_axis))
    u = (u + 0.5 * field.extent_u) / field.extent_u
    v = (v + 0.5 * field.extent_v) / field.extent_v
    return u, v
