from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .model import Mesh


def _segment_closest(
    points: np.ndarray, a: np.ndarray, b: np.ndarray
) -> np.ndarray:
    ab = b - a
    denom = np.sum(ab * ab, axis=-1)
    safe = np.where(denom > 1e-30, denom, 1.0)
    t = np.sum((points - a) * ab, axis=-1) / safe
    t = np.clip(t, 0.0, 1.0)
    result = a + t[..., None] * ab
    return np.where((denom > 1e-30)[..., None], result, a)


def _closest_point_on_triangles(
    points: np.ndarray,
    triangles: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return closest points, triangle ids and squared distances.

    points: (P, 3), triangles: (T, 3, 3).
    """

    if len(points) == 0 or len(triangles) == 0:
        return (
            np.empty((len(points), 3), dtype=np.float64),
            np.zeros((len(points),), dtype=np.int64),
            np.full((len(points),), np.inf, dtype=np.float64),
        )
    p = points[:, None, :]
    a = triangles[None, :, 0, :]
    b = triangles[None, :, 1, :]
    c = triangles[None, :, 2, :]
    ab = b - a
    ac = c - a
    n = np.cross(ab, ac)
    n2 = np.sum(n * n, axis=-1)
    plane_offset = np.sum((p - a) * n, axis=-1) / np.where(n2 > 1e-30, n2, 1.0)
    projected = p - plane_offset[..., None] * n

    d00 = np.sum(ab * ab, axis=-1)
    d01 = np.sum(ab * ac, axis=-1)
    d11 = np.sum(ac * ac, axis=-1)
    relative = projected - a
    d20 = np.sum(relative * ab, axis=-1)
    d21 = np.sum(relative * ac, axis=-1)
    denom = d00 * d11 - d01 * d01
    safe_denom = np.where(np.abs(denom) > 1e-30, denom, 1.0)
    v = (d11 * d20 - d01 * d21) / safe_denom
    w = (d00 * d21 - d01 * d20) / safe_denom
    u = 1.0 - v - w
    epsilon = 1e-9
    inside = (
        (np.abs(denom) > 1e-30)
        & (u >= -epsilon)
        & (v >= -epsilon)
        & (w >= -epsilon)
    )

    q_ab = _segment_closest(p, a, b)
    q_ac = _segment_closest(p, a, c)
    q_bc = _segment_closest(p, b, c)
    d_ab = np.sum((p - q_ab) ** 2, axis=-1)
    d_ac = np.sum((p - q_ac) ** 2, axis=-1)
    d_bc = np.sum((p - q_bc) ** 2, axis=-1)
    choices = np.stack((d_ab, d_ac, d_bc), axis=-1)
    choice = np.argmin(choices, axis=-1)
    closest_edges = np.take_along_axis(
        np.stack((q_ab, q_ac, q_bc), axis=2),
        choice[..., None, None],
        axis=2,
    )[:, :, 0, :]
    closest = np.where(inside[..., None], projected, closest_edges)
    distance2 = np.sum((p - closest) ** 2, axis=-1)
    triangle_ids = np.argmin(distance2, axis=1)
    rows = np.arange(len(points))
    return closest[rows, triangle_ids], triangle_ids, distance2[rows, triangle_ids]


def _triangle_data(mesh: Mesh) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    idx, vertices, face_ids = mesh.triangle_arrays()
    if len(idx) == 0:
        return (
            np.empty((0, 3, 3), dtype=np.float64),
            np.empty((0, 3), dtype=np.float64),
            np.empty((0,), dtype=np.int64),
        )
    normals = np.cross(vertices[:, 1] - vertices[:, 0], vertices[:, 2] - vertices[:, 0])
    lengths = np.linalg.norm(normals, axis=1)
    valid = lengths > 1e-15
    normals[valid] /= lengths[valid, None]
    return vertices, normals, face_ids


def _surface_samples(mesh: Mesh) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    face_normals = mesh.face_normals()
    points: list[np.ndarray] = []
    owners: list[int] = []
    normals: list[np.ndarray] = []
    eligible: list[bool] = []
    for face_id, a, b, c in mesh.triangle_iter():
        corners = mesh.vertices[np.asarray((a, b, c), dtype=np.int64)]
        centroid = np.mean(corners, axis=0)
        points.extend(list(corners))
        points.append(centroid)
        owners.extend([face_id] * 4)
        normals.extend([face_normals[face_id]] * 4)
        eligible.extend([False, False, False, True])
    if not points:
        return (
            np.empty((0, 3), dtype=np.float64),
            np.empty((0,), dtype=np.int64),
            np.empty((0, 3), dtype=np.float64),
            np.empty((0,), dtype=bool),
        )
    return (
        np.asarray(points),
        np.asarray(owners, dtype=np.int64),
        np.asarray(normals),
        np.asarray(eligible, dtype=bool),
    )


def _direction_report(
    sample_mesh: Mesh,
    reference_mesh: Mesh,
    tolerance: float,
    normal_tolerance_deg: float,
) -> dict[str, Any]:
    sample_points, owner_faces, owner_normals, normal_eligible = _surface_samples(sample_mesh)
    triangles, triangle_normals, triangle_faces = _triangle_data(reference_mesh)
    if len(sample_points) == 0 or len(triangles) == 0:
        return {
            "samples": 0,
            "max_distance": 0.0,
            "mean_distance": 0.0,
            "p95_distance": 0.0,
            "max_normal_angle_deg": 0.0,
            "mean_normal_angle_deg": 0.0,
            "bad_faces": [],
        }
    _, nearest, distance = _closest_point_on_triangles(sample_points, triangles)
    nearest_faces = triangle_faces[nearest]
    nearest_normals = triangle_normals[nearest]
    dots = np.sum(owner_normals * nearest_normals, axis=1)
    dots = np.clip(dots, -1.0, 1.0)
    valid_normal = normal_eligible & (np.linalg.norm(owner_normals, axis=1) > 0.5) & (
        np.linalg.norm(nearest_normals, axis=1) > 0.5
    )
    angles = np.zeros_like(distance)
    angles[valid_normal] = np.degrees(np.arccos(dots[valid_normal]))

    face_max_distance: dict[int, float] = {}
    face_max_angle: dict[int, float] = {}
    for face_id, dist, angle, use_angle in zip(
        owner_faces.tolist(), distance.tolist(), angles.tolist(), normal_eligible.tolist()
    ):
        face_max_distance[face_id] = max(face_max_distance.get(face_id, 0.0), float(math.sqrt(dist)))
        if use_angle:
            face_max_angle[face_id] = max(face_max_angle.get(face_id, 0.0), float(angle))
    bad = sorted(
        face_id
        for face_id in face_max_distance
        if face_max_distance[face_id] > tolerance or face_max_angle[face_id] > normal_tolerance_deg
    )
    return {
        "samples": int(len(sample_points)),
        "max_distance": float(np.max(np.sqrt(distance))),
        "mean_distance": float(np.mean(np.sqrt(distance))),
        "p95_distance": float(np.percentile(np.sqrt(distance), 95)),
        "max_normal_angle_deg": float(np.max(angles[valid_normal])) if np.any(valid_normal) else 0.0,
        "mean_normal_angle_deg": float(np.mean(angles[valid_normal])) if np.any(valid_normal) else 0.0,
        "bad_faces": bad,
        "bad_face_distance": {str(k): face_max_distance[k] for k in bad},
        "bad_face_normal_angle_deg": {str(k): face_max_angle[k] for k in bad},
    }


def compare_meshes(
    truth: Mesh,
    candidate: Mesh,
    *,
    tolerance: float = 1e-5,
    normal_tolerance_deg: float = 2.0,
) -> dict[str, Any]:
    if tolerance < 0.0 or not math.isfinite(tolerance):
        raise ValueError("distance tolerance must be finite and non-negative")
    forward = _direction_report(candidate, truth, tolerance, normal_tolerance_deg)
    reverse = _direction_report(truth, candidate, tolerance, normal_tolerance_deg)
    candidate_bad = sorted(set(forward["bad_faces"]))
    truth_bad = sorted(set(reverse["bad_faces"]))
    ok = not candidate_bad and not truth_bad
    return {
        "code": "OK" if ok else "E_PRECISION_TOLERANCE",
        "tolerance": float(tolerance),
        "normal_tolerance_deg": float(normal_tolerance_deg),
        "truth": truth.metrics(),
        "candidate": candidate.metrics(),
        "candidate_to_truth": forward,
        "truth_to_candidate": reverse,
        "candidate_bad_faces": candidate_bad,
        "truth_bad_faces": truth_bad,
        "max_bidirectional_distance": max(
            float(forward["max_distance"]), float(reverse["max_distance"])
        ),
        "max_bidirectional_normal_angle_deg": max(
            float(forward["max_normal_angle_deg"]), float(reverse["max_normal_angle_deg"])
        ),
    }


def write_precision_report(report: dict[str, Any], path: str | Path) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False) + "\n"
    path.write_text(payload, encoding="utf-8")


def summarize_precision(report: dict[str, Any]) -> str:
    forward = report["candidate_to_truth"]
    reverse = report["truth_to_candidate"]
    return (
        f"max bidirectional deviation={report['max_bidirectional_distance']:.6g}; "
        f"candidate->truth mean={forward['mean_distance']:.6g}, p95={forward['p95_distance']:.6g}; "
        f"truth->candidate mean={reverse['mean_distance']:.6g}, p95={reverse['p95_distance']:.6g}; "
        f"max normal angle={report['max_bidirectional_normal_angle_deg']:.6g} deg; "
        f"bad candidate faces={len(report['candidate_bad_faces'])}"
    )
