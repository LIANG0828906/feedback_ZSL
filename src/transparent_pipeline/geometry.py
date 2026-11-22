"""Deterministic ray/boundary intersection and refraction."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .errors import geometry_error, parameter_error
from .util import finite_positive, normalize

RAY_EPSILON = 1.0e-9
SURFACE_EPSILON = 2.0e-7


@dataclass
class Hit:
    t: float
    point: np.ndarray
    normal: np.ndarray
    medium_id: str
    boundary_type: str


@dataclass
class MediumSegment:
    start: np.ndarray
    end: np.ndarray
    direction: np.ndarray
    length: float


@dataclass
class MediumPath:
    medium_id: str
    entry_point: np.ndarray
    exit_point: np.ndarray
    entry_normal: np.ndarray
    exit_normal: np.ndarray | None
    entry_direction: np.ndarray
    exit_direction: np.ndarray | None
    segments: list[MediumSegment] = field(default_factory=list)
    tir_count: int = 0
    complete: bool = True

    @property
    def path_length(self) -> float:
        return float(sum(segment.length for segment in self.segments))


def _resolve_boundary(medium: dict[str, Any]) -> dict[str, Any]:
    return medium["boundary"]


def _sphere_roots(origin: np.ndarray, direction: np.ndarray, center: np.ndarray, radius: float) -> list[float]:
    offset = origin - center
    a = float(np.dot(direction, direction))
    b = 2.0 * float(np.dot(offset, direction))
    c = float(np.dot(offset, offset)) - radius * radius
    discriminant = b * b - 4.0 * a * c
    if discriminant < 0.0:
        return []
    root = math.sqrt(max(0.0, discriminant))
    denominator = 2.0 * a
    return sorted(((-b - root) / denominator, (-b + root) / denominator))


def _box_interval(origin: np.ndarray, direction: np.ndarray, lower: np.ndarray, upper: np.ndarray) -> tuple[float, float, int, int]:
    t_min = -math.inf
    t_max = math.inf
    axis_min = 0
    axis_max = 0
    for axis in range(3):
        if abs(float(direction[axis])) < 1.0e-15:
            if origin[axis] < lower[axis] or origin[axis] > upper[axis]:
                return math.inf, -math.inf, -1, -1
            continue
        inv = 1.0 / float(direction[axis])
        t1 = float((lower[axis] - origin[axis]) * inv)
        t2 = float((upper[axis] - origin[axis]) * inv)
        if t1 > t2:
            t1, t2 = t2, t1
        if t1 > t_min:
            t_min = t1
            axis_min = axis
        if t2 < t_max:
            t_max = t2
            axis_max = axis
    return t_min, t_max, axis_min, axis_max


def _box_normal(point: np.ndarray, lower: np.ndarray, upper: np.ndarray, axis: int, direction: np.ndarray) -> np.ndarray:
    normal = np.zeros(3, dtype=np.float64)
    if point[axis] - lower[axis] < upper[axis] - point[axis]:
        normal[axis] = -1.0 if direction[axis] > 0.0 else 1.0
    else:
        normal[axis] = 1.0 if direction[axis] > 0.0 else -1.0
    return normal


def _triangle_hit(
    origin: np.ndarray,
    direction: np.ndarray,
    first: np.ndarray,
    second: np.ndarray,
    third: np.ndarray,
) -> tuple[float, np.ndarray] | None:
    edge1 = second - first
    edge2 = third - first
    pvec = np.cross(direction, edge2)
    determinant = float(np.dot(edge1, pvec))
    if abs(determinant) < 1.0e-12:
        return None
    inv_det = 1.0 / determinant
    tvec = origin - first
    u = float(np.dot(tvec, pvec)) * inv_det
    if u < 0.0 or u > 1.0:
        return None
    qvec = np.cross(tvec, edge1)
    v = float(np.dot(direction, qvec)) * inv_det
    if v < 0.0 or u + v > 1.0:
        return None
    t = float(np.dot(edge2, qvec)) * inv_det
    if t <= RAY_EPSILON:
        return None
    normal = normalize(np.cross(edge1, edge2), "boundary.normal")
    if float(np.dot(normal, direction)) > 0.0:
        normal = -normal
    return t, normal


def intersect_boundary(
    medium: dict[str, Any],
    origin: np.ndarray,
    direction: np.ndarray,
    minimum_t: float = RAY_EPSILON,
) -> list[Hit]:
    """Return all positive boundary intersections sorted by t."""

    boundary = _resolve_boundary(medium)
    kind = boundary["type"]
    hits: list[Hit] = []
    if kind == "sphere":
        center = np.asarray(boundary["center"], dtype=np.float64)
        radius = float(boundary["radius"])
        for t in _sphere_roots(origin, direction, center, radius):
            if t > minimum_t and math.isfinite(t):
                point = origin + t * direction
                normal = (point - center) / radius
                hits.append(Hit(t, point, normal, medium["id"], kind))
    elif kind == "box":
        lower = np.asarray(boundary["min"], dtype=np.float64)
        upper = np.asarray(boundary["max"], dtype=np.float64)
        t_near, t_far, axis_near, axis_far = _box_interval(origin, direction, lower, upper)
        if not math.isfinite(t_near) or not math.isfinite(t_far) or t_far < t_near:
            return hits
        if t_near > minimum_t:
            point = origin + t_near * direction
            normal = _box_normal(point, lower, upper, axis_near, direction)
            hits.append(Hit(t_near, point, normal, medium["id"], kind))
        if math.isfinite(t_far) and t_far > minimum_t and abs(t_far - t_near) > RAY_EPSILON:
            point = origin + t_far * direction
            normal = _box_normal(point, lower, upper, axis_far, direction)
            hits.append(Hit(t_far, point, normal, medium["id"], kind))
    elif kind == "mesh":
        vertices = [np.asarray(vertex, dtype=np.float64) for vertex in boundary["vertices"]]
        for face in boundary["faces"]:
            result = _triangle_hit(origin, direction, vertices[face[0]], vertices[face[1]], vertices[face[2]])
            if result is not None:
                t, normal = result
                hits.append(Hit(t, origin + t * direction, normal, medium["id"], kind))
    else:
        raise geometry_error(f"medium[{medium['id']}].boundary.type", f"unsupported boundary type {kind!r}")
    hits.sort(key=lambda hit: (hit.t, hit.medium_id))
    return hits


def nearest_scene_hit(scene: Any, origin: np.ndarray, direction: np.ndarray) -> Hit | None:
    nearest: Hit | None = None
    for medium in scene.media:
        hits = intersect_boundary(medium, origin, direction)
        if hits and (nearest is None or hits[0].t < nearest.t - RAY_EPSILON):
            nearest = hits[0]
        elif hits and nearest is not None and abs(hits[0].t - nearest.t) <= RAY_EPSILON:
            if hits[0].medium_id < nearest.medium_id:
                nearest = hits[0]
    return nearest


def refract_direction(
    incident: np.ndarray,
    normal: np.ndarray,
    n_incident: float,
    n_transmitted: float,
) -> tuple[np.ndarray, bool]:
    """Return (direction, total_internal_reflection).

    ``normal`` may point either way; it is oriented against the incident ray.
    """

    if not math.isfinite(n_incident) or not math.isfinite(n_transmitted):
        raise parameter_error("refraction.ior", "refractive indices must be finite")
    if n_incident <= 0.0 or n_transmitted <= 0.0:
        raise parameter_error("refraction.ior", "refractive indices must be positive")
    direction = normalize(np.asarray(incident, dtype=np.float64), "refraction.incident")
    oriented_normal = normalize(np.asarray(normal, dtype=np.float64), "refraction.normal")
    cosine = -float(np.dot(direction, oriented_normal))
    if cosine < 0.0:
        oriented_normal = -oriented_normal
        cosine = -cosine
    cosine = min(1.0, max(0.0, cosine))
    eta = n_incident / n_transmitted
    k = 1.0 - eta * eta * (1.0 - cosine * cosine)
    if k < 0.0:
        reflected = direction - 2.0 * float(np.dot(direction, oriented_normal)) * oriented_normal
        return normalize(reflected, "refraction.reflected"), True
    refracted = eta * direction + (eta * cosine - math.sqrt(k)) * oriented_normal
    return normalize(refracted, "refraction.refracted"), False


def reflect_direction(incident: np.ndarray, normal: np.ndarray) -> np.ndarray:
    direction = normalize(np.asarray(incident, dtype=np.float64), "reflection.incident")
    surface_normal = normalize(np.asarray(normal, dtype=np.float64), "reflection.normal")
    reflected = direction - 2.0 * float(np.dot(direction, surface_normal)) * surface_normal
    return normalize(reflected, "reflection.reflected")


def trace_medium_path(
    medium: dict[str, Any],
    entry: Hit,
    incident: np.ndarray,
    wavelength_index: int,
    max_segments: int = 8,
) -> MediumPath:
    """Trace through one homogeneous medium, including TIR branches."""

    if max_segments <= 0:
        raise parameter_error("render.max_segments", "max_segments must be positive")
    ior = float(medium["ior"][wavelength_index])
    if not math.isfinite(ior) or ior <= 1.0:
        raise parameter_error(f"medium[{medium['id']}].ior[{wavelength_index}]", "refractive index must be > 1")

    incident = normalize(np.asarray(incident, dtype=np.float64), "ray.direction")
    direction, tir = refract_direction(incident, entry.normal, 1.0, ior)
    path = MediumPath(
        medium_id=medium["id"],
        entry_point=entry.point.copy(),
        exit_point=entry.point.copy(),
        entry_normal=entry.normal.copy(),
        exit_normal=None,
        entry_direction=direction.copy(),
        exit_direction=None,
        tir_count=1 if tir else 0,
    )
    # Entry TIR is physically impossible from air to a denser medium, but keeping
    # the branch makes the function safe when used directly with arbitrary media.
    if tir:
        direction = reflect_direction(incident, entry.normal)

    point = entry.point.copy()
    for _ in range(max_segments):
        hits = intersect_boundary(medium, point, direction, minimum_t=SURFACE_EPSILON)
        if not hits:
            path.complete = False
            path.exit_point = point
            path.exit_direction = direction
            return path
        hit = hits[0]
        segment = MediumSegment(point.copy(), hit.point.copy(), direction.copy(), float(hit.t))
        path.segments.append(segment)
        path.exit_point = hit.point.copy()
        path.exit_normal = hit.normal.copy()
        path.exit_direction = direction.copy()
        outgoing, exit_tir = refract_direction(direction, hit.normal, ior, 1.0)
        if not exit_tir:
            path.exit_direction = outgoing
            return path
        path.tir_count += 1
        direction = reflect_direction(direction, hit.normal)
        point = hit.point.copy()

    path.complete = False
    return path


def camera_rays(camera: dict[str, Any], sample_index: int, width: int | None = None, height: int | None = None) -> tuple[np.ndarray, np.ndarray]:
    trajectory = camera["trajectory"]
    if not 0 <= sample_index < len(trajectory):
        raise parameter_error("camera.sample_index", "camera trajectory sample index is out of range")
    sample = trajectory[sample_index]
    eye = np.asarray(sample["eye"], dtype=np.float64)
    target = np.asarray(sample["target"], dtype=np.float64)
    up = np.asarray(sample["up"], dtype=np.float64)
    forward = normalize(target - eye, "camera.forward")
    right = normalize(np.cross(forward, up), "camera.right")
    true_up = normalize(np.cross(right, forward), "camera.up")
    width = camera["width"] if width is None else width
    height = camera["height"] if height is None else height
    aspect = width / height
    tangent = math.tan(math.radians(float(camera["fov_y_deg"])) * 0.5)
    y, x = np.mgrid[0:height, 0:width].astype(np.float64)
    ndc_x = (2.0 * (x + 0.5) / width - 1.0) * aspect * tangent
    ndc_y = (1.0 - 2.0 * (y + 0.5) / height) * tangent
    directions = (
        forward[None, None, :]
        + ndc_x[:, :, None] * right[None, None, :]
        + ndc_y[:, :, None] * true_up[None, None, :]
    )
    directions /= np.linalg.norm(directions, axis=2, keepdims=True)
    origins = np.broadcast_to(eye, directions.shape).copy()
    return origins, directions


def ray_for_pixel(camera: dict[str, Any], sample_index: int, x: int, y: int) -> tuple[np.ndarray, np.ndarray]:
    origins, directions = camera_rays(camera, sample_index, camera["width"], camera["height"])
    return origins[y, x].copy(), directions[y, x].copy()


__all__ = [
    "Hit",
    "MediumPath",
    "MediumSegment",
    "camera_rays",
    "intersect_boundary",
    "nearest_scene_hit",
    "ray_for_pixel",
    "reflect_direction",
    "refract_direction",
    "trace_medium_path",
]

