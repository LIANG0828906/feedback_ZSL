"""Deterministic lattice projections, soft visibility, and landing reports."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .geometry import LayerGeometry, lattice_pattern, smoothstep


def penumbra_width(
    projection: dict[str, Any],
    *,
    receiver_distance: float,
    source_distance: float,
) -> float:
    """Scene-declared monotone penumbra model.

    Both terms are deliberately positive: an art supervisor can audit that an
    increasing lattice-to-receiver gap and an increasing light-to-lattice gap
    widen the transition.  The max clamp only limits pathological scenes.
    """

    width = float(projection["penumbra_base"]) * (
        1.0
        + float(projection["penumbra_distance_gain"]) * max(float(receiver_distance), 0.0)
        + float(projection["penumbra_source_gain"]) * max(float(source_distance), 0.0)
    )
    return min(width, float(projection["max_penumbra"]))


def _sample_offsets(sample_count: int) -> np.ndarray:
    if sample_count == 1:
        coordinates = np.asarray([0.0], dtype=np.float64)
    else:
        coordinates = np.linspace(-0.5, 0.5, int(sample_count), dtype=np.float64)
    first, second = np.meshgrid(coordinates, coordinates, indexing="xy")
    return np.stack((first.reshape(-1), second.reshape(-1)), axis=1)


def visibility_for_points(
    points: np.ndarray,
    receiver_layer: int | None,
    lights: list[dict[str, Any]],
    geometries: list[LayerGeometry],
    scene: dict[str, Any],
) -> np.ndarray:
    """Return visibility[point, light] after all inner lattice projections."""

    points = np.asarray(points, dtype=np.float64).reshape(-1, 3)
    if points.shape[0] == 0:
        return np.zeros((0, len(lights)), dtype=np.float64)
    lattices = [geometry for geometry in geometries if geometry.kind == "lattice"]
    visibility = np.ones((points.shape[0], len(lights)), dtype=np.float64)
    projection = scene["projection"]
    offsets = _sample_offsets(int(projection["samples_per_axis"]))
    for light_index, light in enumerate(lights):
        light_position = np.asarray(light["position"], dtype=np.float64)
        to_light = light_position[None, :] - points
        light_coverage = np.zeros(points.shape[0], dtype=np.float64)
        for lattice in lattices:
            if receiver_layer is not None and lattice.layer >= receiver_layer:
                continue
            denominator = to_light @ lattice.normal
            numerator = (lattice.reference[None, :] - points) @ lattice.normal
            parameter = np.full(points.shape[0], np.nan, dtype=np.float64)
            valid = np.abs(denominator) > 1e-10
            parameter[valid] = numerator[valid] / denominator[valid]
            valid &= (parameter > 1e-8) & (parameter < 1.0 - 1e-8)
            if not np.any(valid):
                continue
            indices = np.nonzero(valid)[0]
            plane_points = points[indices] + to_light[indices] * parameter[indices, None]
            inside = lattice.inside_mesh(plane_points, tolerance=1e-8)
            if not np.any(inside):
                continue
            indices = indices[inside]
            plane_points = plane_points[inside]
            receiver_distance = np.linalg.norm(plane_points - points[indices], axis=1)
            source_distance = np.linalg.norm(light_position[None, :] - plane_points, axis=1)
            widths = np.minimum(
                float(projection["penumbra_base"])
                * (
                    1.0
                    + float(projection["penumbra_distance_gain"]) * receiver_distance
                    + float(projection["penumbra_source_gain"]) * source_distance
                ),
                float(projection["max_penumbra"]),
            )

            coverage_sum = np.zeros(indices.shape[0], dtype=np.float64)
            sample_softness = np.maximum(
                float(lattice.definition["pattern"]["edge_softness"]),
                widths / float(offsets.shape[0]),
            )
            for offset_u, offset_v in offsets:
                sample_points = (
                    plane_points
                    + lattice.u_axis[None, :] * (offset_u * widths)[:, None]
                    + lattice.v_axis[None, :] * (offset_v * widths)[:, None]
                )
                sample_inside = lattice.inside_mesh(sample_points, tolerance=1e-8)
                coverage, _ = lattice_pattern(
                    lattice,
                    sample_points,
                    softness=sample_softness,
                )
                coverage_sum += coverage * sample_inside
            coverage_mean = coverage_sum / float(offsets.shape[0])
            light_coverage[indices] = 1.0 - (
                (1.0 - light_coverage[indices]) * (1.0 - coverage_mean)
            )
        visibility[:, light_index] = 1.0 - (
            float(scene["global"]["shadow_strength"]) * light_coverage
        )
    return np.clip(visibility, 0.0, 1.0)


def _line_positions(
    minimum: float,
    maximum: float,
    pitch: float,
    offset: float,
    count: int,
) -> list[float]:
    first_index = int(math.ceil((minimum - offset) / pitch))
    last_index = int(math.floor((maximum - offset) / pitch))
    positions = [
        offset + index * pitch
        for index in range(first_index, last_index + 1)
        if minimum - 1e-10 <= offset + index * pitch <= maximum + 1e-10
    ]
    if not positions:
        return [0.5 * (minimum + maximum)]
    if len(positions) <= count:
        return positions
    indices = np.rint(np.linspace(0, len(positions) - 1, count)).astype(np.int64)
    return [positions[int(index)] for index in dict.fromkeys(indices.tolist())]


def sample_occluder_points(
    geometry: LayerGeometry,
    samples_per_axis: int,
) -> list[dict[str, Any]]:
    layer = geometry.definition
    pattern = layer["pattern"]
    pitch = float(pattern["pitch"])
    offset = (int(layer["pattern_seed"]) * 0.0137) % pitch
    u_min, u_max, v_min, v_max = geometry.uv_bounds
    if pattern["style"] == "diamond":
        inverse_sqrt_two = 1.0 / math.sqrt(2.0)
        corners_u = np.asarray([u_min, u_max, u_max, u_min])
        corners_v = np.asarray([v_min, v_min, v_max, v_max])
        first_corners = (corners_u + corners_v) * inverse_sqrt_two
        second_corners = (corners_u - corners_v) * inverse_sqrt_two
        first_positions = _line_positions(
            float(np.min(first_corners)),
            float(np.max(first_corners)),
            pitch,
            offset,
            samples_per_axis,
        )
        second_positions = _line_positions(
            float(np.min(second_corners)),
            float(np.max(second_corners)),
            pitch,
            offset,
            samples_per_axis,
        )
        uv_candidates = [
            (
                (first + second) * inverse_sqrt_two,
                (first - second) * inverse_sqrt_two,
            )
            for first in first_positions
            for second in second_positions
        ]
    else:
        u_positions = _line_positions(u_min, u_max, pitch, offset, samples_per_axis)
        v_positions = _line_positions(v_min, v_max, pitch, offset, samples_per_axis)
        uv_candidates = [(u, v) for u in u_positions for v in v_positions]

    points: list[dict[str, Any]] = []
    for u_value, v_value in uv_candidates:
        if not (u_min - 1e-9 <= u_value <= u_max + 1e-9):
            continue
        if not (v_min - 1e-9 <= v_value <= v_max + 1e-9):
            continue
        world = geometry.world_from_uv(u_value, v_value)
        if not bool(geometry.inside_mesh(world[None, :], tolerance=1e-8)[0]):
            continue
        points.append(
            {
                "u": float(u_value),
                "v": float(v_value),
                "point": np.asarray(world, dtype=np.float64),
            }
        )
    if not points:
        center = geometry.world_from_uv(
            0.5 * (u_min + u_max),
            0.5 * (v_min + v_max),
        )
        points.append(
            {
                "u": 0.5 * (u_min + u_max),
                "v": 0.5 * (v_min + v_max),
                "point": center,
            }
        )
    return points


def _round_vector(values: np.ndarray) -> list[float]:
    return [round(float(value), 9) for value in values]


def _ground_projection(
    source: np.ndarray,
    light_position: np.ndarray,
    ground: dict[str, Any],
) -> tuple[np.ndarray | None, bool]:
    denominator = float(source[1] - light_position[1])
    if abs(denominator) <= 1e-10:
        return None, False
    parameter = (float(ground["y"]) - float(light_position[1])) / denominator
    if parameter < 1.0 - 1e-9:
        return None, False
    landing = light_position + parameter * (source - light_position)
    inside = bool(
        float(ground["x"][0]) <= landing[0] <= float(ground["x"][1])
        and float(ground["z"][0]) <= landing[2] <= float(ground["z"][1])
    )
    return landing, inside


def _plane_projection(
    source: np.ndarray,
    light_position: np.ndarray,
    target: LayerGeometry,
) -> tuple[np.ndarray | None, bool]:
    direction = source - light_position
    denominator = float(np.dot(target.normal, direction))
    if abs(denominator) <= 1e-10:
        return None, False
    parameter = float(np.dot(target.normal, target.reference - light_position)) / denominator
    if parameter < 1.0 - 1e-9:
        return None, False
    landing = light_position + parameter * direction
    inside = bool(target.inside_mesh(landing[None, :], tolerance=1e-8)[0])
    return landing, inside


def build_projection_report(
    scene: dict[str, Any],
    geometries: list[LayerGeometry],
    scene_content_digest: str,
) -> dict[str, Any]:
    projection = scene["projection"]
    samples_per_axis = int(projection["landing_samples_per_axis"])
    lights = sorted(scene["lights"], key=lambda light: str(light["id"]))
    lattices = sorted(
        (geometry for geometry in geometries if geometry.kind == "lattice"),
        key=lambda geometry: (geometry.layer, geometry.layer_id),
    )
    landings: list[dict[str, Any]] = []
    for lattice in lattices:
        sources = sample_occluder_points(lattice, samples_per_axis)
        outer_gauzes = sorted(
            (
                geometry
                for geometry in geometries
                if geometry.kind == "gauze" and geometry.layer > lattice.layer
            ),
            key=lambda geometry: (geometry.layer, geometry.layer_id),
        )
        adjacent_outer = outer_gauzes[0] if outer_gauzes else None
        for light in lights:
            light_position = np.asarray(light["position"], dtype=np.float64)
            for source_index, source_record in enumerate(sources):
                source = source_record["point"]
                targets: list[tuple[str, LayerGeometry | None]] = []
                if adjacent_outer is not None:
                    targets.append(("gauze", adjacent_outer))
                targets.append(("ground", None))
                for target_kind, target in targets:
                    if target_kind == "ground":
                        landing, inside = _ground_projection(source, light_position, scene["ground"])
                        receiver_layer = None
                        receiver_layer_id = None
                    else:
                        assert target is not None
                        landing, inside = _plane_projection(source, light_position, target)
                        receiver_layer = target.layer
                        receiver_layer_id = target.layer_id
                    if landing is None:
                        continue
                    receiver_distance = float(np.linalg.norm(landing - source))
                    source_distance = float(np.linalg.norm(source - light_position))
                    width = penumbra_width(
                        projection,
                        receiver_distance=receiver_distance,
                        source_distance=source_distance,
                    )
                    landings.append(
                        {
                            "id": (
                                f"{light['id']}:{lattice.layer}:{target_kind}:{source_index:03d}"
                            ),
                            "light_id": str(light["id"]),
                            "lattice_layer": lattice.layer,
                            "lattice_layer_id": lattice.layer_id,
                            "receiver_kind": target_kind,
                            "receiver_layer": receiver_layer,
                            "receiver_layer_id": receiver_layer_id,
                            "source_uv": [
                                round(float(source_record["u"]), 9),
                                round(float(source_record["v"]), 9),
                            ],
                            "source": _round_vector(source),
                            "landing": _round_vector(landing),
                            "inside_receiver": inside,
                            "occluded": inside,
                            "distance_lattice_to_receiver": round(receiver_distance, 9),
                            "distance_light_to_lattice": round(source_distance, 9),
                            "penumbra_width": round(width, 9),
                        }
                    )
    landings.sort(
        key=lambda item: (
            item["light_id"],
            item["lattice_layer"],
            item["receiver_kind"],
            item["id"],
        )
    )

    audit = _penumbra_audit(scene, samples_per_axis)
    return {
        "schema_version": "veil-projection/1",
        "scene_digest": scene_content_digest,
        "algorithm_version": "veil-cg/1",
        "samples_per_axis": int(projection["samples_per_axis"]),
        "landing_samples_per_axis": samples_per_axis,
        "lights": [
            {
                "id": str(light["id"]),
                "position": _round_vector(np.asarray(light["position"], dtype=np.float64)),
            }
            for light in lights
        ],
        "lattice_layers": [
            {
                "layer": lattice.layer,
                "id": lattice.layer_id,
                "width": float(lattice.definition["width"]),
                "reflectance": float(lattice.definition["reflectance"]),
            }
            for lattice in lattices
        ],
        "landings": landings,
        "counts": {
            "total": len(landings),
            "inside_receiver": sum(1 for item in landings if item["inside_receiver"]),
            "per_light": {
                str(light["id"]): sum(1 for item in landings if item["light_id"] == light["id"])
                for light in lights
            },
        },
        "penumbra_audit": audit,
    }


def _edge_luma_profile(width: float, samples_per_axis: int) -> list[dict[str, float]]:
    count = max(9, samples_per_axis * 4 + 1)
    positions = np.linspace(-width, width, count)
    shadow = 1.0 - smoothstep(-width * 0.5, width * 0.5, positions)
    luma = 0.075 + 0.72 * (1.0 - 0.82 * shadow)
    return [
        {"offset": round(float(position), 9), "luma": round(float(value), 9)}
        for position, value in zip(positions, luma, strict=True)
    ]


def _penumbra_audit(scene: dict[str, Any], samples_per_axis: int) -> dict[str, Any]:
    projection = scene["projection"]
    receiver_distances = [0.15, 0.35, 0.75, 1.5]
    source_distances = [0.35, 0.75, 1.5, 3.0]
    receiver_series = []
    for distance in receiver_distances:
        width = penumbra_width(
            projection,
            receiver_distance=distance,
            source_distance=1.0,
        )
        receiver_series.append(
            {
                "distance_lattice_to_receiver": distance,
                "distance_light_to_lattice": 1.0,
                "penumbra_width": round(width, 9),
                "edge_profile": _edge_luma_profile(width, samples_per_axis),
            }
        )
    source_series = []
    for distance in source_distances:
        width = penumbra_width(
            projection,
            receiver_distance=0.75,
            source_distance=distance,
        )
        source_series.append(
            {
                "distance_lattice_to_receiver": 0.75,
                "distance_light_to_lattice": distance,
                "penumbra_width": round(width, 9),
                "edge_profile": _edge_luma_profile(width, samples_per_axis),
            }
        )
    receiver_widths = [item["penumbra_width"] for item in receiver_series]
    source_widths = [item["penumbra_width"] for item in source_series]
    return {
        "receiver_distance_series": receiver_series,
        "source_distance_series": source_series,
        "monotonic_with_receiver_distance": all(
            later >= earlier for earlier, later in zip(receiver_widths, receiver_widths[1:])
        ),
        "monotonic_with_source_distance": all(
            later >= earlier for earlier, later in zip(source_widths, source_widths[1:])
        ),
    }

