from __future__ import annotations

import math

import numpy as np

from .scene import AABB, Scene, Sphere
from .util import sha256_bytes, sha256_json

EPSILON = 1.0e-9


def first_hit(
    scene: Scene,
    origins: np.ndarray,
    directions: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    """Return (occluder index, distance) for first hit; -1/inf means sky.

    Origins may be a single point (3,) or many points (P, 3). Directions are
    unit vectors (D, 3). The returned arrays always have shape (P, D).
    """
    origins = np.asarray(origins, dtype=np.float64)
    directions = np.asarray(directions, dtype=np.float64)
    if origins.ndim == 1:
        origins = origins[None, :]
    if directions.ndim != 2 or directions.shape[1] != 3:
        raise ValueError("directions must have shape (D, 3)")
    if origins.ndim != 2 or origins.shape[1] != 3:
        raise ValueError("origins must have shape (P, 3)")

    point_count = origins.shape[0]
    direction_count = directions.shape[0]
    best_distance = np.full((point_count, direction_count), np.inf, dtype=np.float64)
    best_index = np.full((point_count, direction_count), -1, dtype=np.int32)

    occluders = scene.sorted_occluders()
    origin_grid = origins[:, None, :]
    direction_grid = directions[None, :, :]
    with np.errstate(divide="ignore", invalid="ignore", over="ignore"):
        for index, occluder in enumerate(occluders):
            if isinstance(occluder, AABB):
                minimum = np.asarray(occluder.minimum, dtype=np.float64)
                maximum = np.asarray(occluder.maximum, dtype=np.float64)
                parallel = np.abs(direction_grid) <= 1.0e-15
                outside = np.logical_or(origin_grid < minimum, origin_grid > maximum)
                invalid_parallel = np.any(parallel & outside, axis=2)
                inverse = np.where(parallel, 0.0, 1.0 / direction_grid)
                first = (minimum[None, None, :] - origin_grid) * inverse
                second = (maximum[None, None, :] - origin_grid) * inverse
                near = np.where(parallel, -np.inf, np.minimum(first, second))
                far = np.where(parallel, np.inf, np.maximum(first, second))
                t_near = np.max(near, axis=2)
                t_far = np.min(far, axis=2)
                hit = (
                    (t_far >= np.maximum(t_near, EPSILON))
                    & (t_near <= 1.0e6)
                    & ~invalid_parallel
                )
                distance = np.where(t_near > EPSILON, t_near, t_far)
            elif isinstance(occluder, Sphere):
                center = np.asarray(occluder.center, dtype=np.float64)
                offset = origin_grid - center[None, None, :]
                b = np.sum(offset * direction_grid, axis=2)
                c = np.sum(offset * offset, axis=2) - occluder.radius * occluder.radius
                discriminant = b * b - c
                hit = discriminant >= 0.0
                root = np.sqrt(np.maximum(discriminant, 0.0))
                near = -b - root
                far = -b + root
                distance = np.where(near > EPSILON, near, far)
                hit &= distance > EPSILON
            else:  # pragma: no cover - construction validates this
                continue

            closer = hit & (distance < best_distance)
            best_distance[closer] = distance[closer]
            best_index[closer] = index

    return best_index, best_distance


def ray_distance_for_occluder(
    occluder: AABB | Sphere,
    origins: np.ndarray,
    directions: np.ndarray,
) -> np.ndarray:
    """Distance to one occluder for origins (P,3) and directions (D,3)."""
    origins = np.asarray(origins, dtype=np.float64)
    directions = np.asarray(directions, dtype=np.float64)
    if origins.ndim == 1:
        origins = origins[None, :]
    origin_grid = origins[:, None, :]
    direction_grid = directions[None, :, :]
    with np.errstate(divide="ignore", invalid="ignore", over="ignore"):
        if isinstance(occluder, AABB):
            minimum = np.asarray(occluder.minimum, dtype=np.float64)
            maximum = np.asarray(occluder.maximum, dtype=np.float64)
            parallel = np.abs(direction_grid) <= 1.0e-15
            outside = np.logical_or(origin_grid < minimum, origin_grid > maximum)
            invalid_parallel = np.any(parallel & outside, axis=2)
            inverse = np.where(parallel, 0.0, 1.0 / direction_grid)
            first = (minimum[None, None, :] - origin_grid) * inverse
            second = (maximum[None, None, :] - origin_grid) * inverse
            near = np.where(parallel, -np.inf, np.minimum(first, second))
            far = np.where(parallel, np.inf, np.maximum(first, second))
            t_near = np.max(near, axis=2)
            t_far = np.min(far, axis=2)
            hit = (
                (t_far >= np.maximum(t_near, EPSILON))
                & (t_near <= 1.0e6)
                & ~invalid_parallel
            )
            distance = np.where(t_near > EPSILON, t_near, t_far)
        elif isinstance(occluder, Sphere):
            center = np.asarray(occluder.center, dtype=np.float64)
            offset = origin_grid - center[None, None, :]
            b = np.sum(offset * direction_grid, axis=2)
            c = np.sum(offset * offset, axis=2) - occluder.radius * occluder.radius
            discriminant = b * b - c
            hit = discriminant >= 0.0
            root = np.sqrt(np.maximum(discriminant, 0.0))
            near = -b - root
            far = -b + root
            distance = np.where(near > EPSILON, near, far)
            hit &= distance > EPSILON
        else:  # pragma: no cover
            raise ValueError("unsupported occluder")
    return np.where(hit, distance, np.inf)


def visibility_fraction(hit_ids: np.ndarray) -> np.ndarray:
    hit_ids = np.asarray(hit_ids)
    if hit_ids.ndim == 1:
        return np.asarray(float(np.mean(hit_ids < 0)), dtype=np.float64)
    return np.mean(hit_ids < 0, axis=1)


def occluder_material_arrays(scene: Scene) -> tuple[np.ndarray, np.ndarray]:
    occluders = scene.sorted_occluders()
    materials = scene.sorted_materials()
    material_index = {material.name: i for i, material in enumerate(materials)}
    albedo = np.asarray([material.albedo for material in materials], dtype=np.float64)
    gain = np.asarray([material.bounce_gain for material in materials], dtype=np.float64)
    hit_material = np.asarray(
        [material_index[occluder.material] for occluder in occluders], dtype=np.int32
    )
    return albedo, gain, hit_material


def blocked_radiance(
    scene: Scene,
    hit_ids: np.ndarray,
) -> np.ndarray:
    """Indirect RGB radiance returned by the first-hit material."""
    albedo, gain, hit_material = occluder_material_arrays(scene)
    ambient = scene.sky.average_radiance()
    blocked_materials = np.zeros((*np.asarray(hit_ids).shape, 3), dtype=np.float64)
    valid = hit_ids >= 0
    if np.any(valid):
        material_ids = hit_material[hit_ids[valid]]
        blocked_materials[valid] = albedo[material_ids] * ambient[None, :] * gain[
            material_ids
        ][:, None]
    return blocked_materials


def occlusion_digest(hit_ids: np.ndarray, distances: np.ndarray) -> str:
    ids = np.asarray(hit_ids, dtype="<i4")
    safe_distance = np.nan_to_num(
        np.asarray(distances, dtype=np.float64),
        nan=1.0e30,
        posinf=1.0e30,
        neginf=1.0e30,
    )
    quantized = np.round(safe_distance, 9).astype("<f8")
    return sha256_bytes(ids.tobytes() + quantized.tobytes())


def object_geometry_hash(scene: Scene, object_id: str) -> str | None:
    occluder = scene.occluder(object_id)
    if occluder is None:
        return None
    return sha256_json(occluder.fingerprint())


def object_center(occluder: AABB | Sphere) -> tuple[float, float, float]:
    if isinstance(occluder, AABB):
        return occluder.center
    return occluder.center


def object_radius(occluder: AABB | Sphere) -> float:
    if isinstance(occluder, AABB):
        return occluder.radius
    return occluder.radius


def direction_angles(directions: np.ndarray) -> np.ndarray:
    directions = np.asarray(directions)
    return np.stack(
        [
            np.arctan2(directions[:, 2], directions[:, 0]),
            np.arcsin(np.clip(directions[:, 1], -1.0, 1.0)),
        ],
        axis=1,
    )


def safe_unit(vector: np.ndarray) -> np.ndarray:
    vector = np.asarray(vector, dtype=np.float64)
    length = float(np.linalg.norm(vector))
    if length <= 1.0e-15:
        raise ValueError("zero-length vector")
    return vector / length

