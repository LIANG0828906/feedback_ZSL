from __future__ import annotations

import math
from typing import Any

import numpy as np

from .mesh import Mesh
from .repair import compute_normals
from .topology import build_topology


def evaluate_repair(
    original: Mesh,
    output: Mesh,
    *,
    face_provenance: np.ndarray | None = None,
    weld_summary: dict[str, Any] | None = None,
    fill_summary: dict[str, Any] | None = None,
    split_summary: dict[str, Any] | None = None,
    fragment_summary: dict[str, Any] | None = None,
    normal_tolerance_deg: float = 30.0,
) -> dict[str, Any]:
    """Compute deterministic repair and normal-deviation metrics."""

    original_normals = compute_normals(original)
    output_normals = compute_normals(output)
    original_centroids = original.face_centroids()
    output_centroids = output.face_centroids()

    angles_deg: np.ndarray
    nearest_faces: np.ndarray
    if output.face_count == 0 or original.face_count == 0:
        angles_deg = np.empty(0, dtype=np.float64)
        nearest_faces = np.empty(0, dtype=np.int64)
    elif original.face_count * output.face_count <= 20_000_000:
        # Small deterministic truth comparison. For large meshes, cap the
        # all-pairs work by comparing provenance-preserved faces and a uniform
        # sample of repaired faces.
        deltas = output_centroids[:, None, :] - original_centroids[None, :, :]
        distance2 = np.einsum("ijk,ijk->ij", deltas, deltas)
        nearest_faces = np.argmin(distance2, axis=1)
        dots = np.einsum(
            "ij,ij->i",
            output_normals.face_normals,
            original_normals.face_normals[nearest_faces],
        )
        angles_deg = np.degrees(np.arccos(np.clip(dots, -1.0, 1.0)))
    else:
        sample_count = min(
            output.face_count,
            max(256, int(math.sqrt(20_000_000 / max(original.face_count, 1)))),
        )
        sample_ids = np.linspace(
            0, output.face_count - 1, sample_count, dtype=np.int64
        )
        deltas = output_centroids[sample_ids, None, :] - original_centroids[None, :, :]
        distance2 = np.einsum("ijk,ijk->ij", deltas, deltas)
        sampled_nearest = np.argmin(distance2, axis=1)
        nearest_faces = np.full(output.face_count, -1, dtype=np.int64)
        nearest_faces[sample_ids] = sampled_nearest
        angles_deg = np.full(output.face_count, np.nan, dtype=np.float64)
        dots = np.einsum(
            "ij,ij->i",
            output_normals.face_normals[sample_ids],
            original_normals.face_normals[sampled_nearest],
        )
        angles_deg[sample_ids] = np.degrees(np.arccos(np.clip(dots, -1.0, 1.0)))

    valid_angles = angles_deg[np.isfinite(angles_deg)]
    if valid_angles.size:
        over = np.flatnonzero(np.isfinite(angles_deg) & (angles_deg > normal_tolerance_deg))
        worst_face = int(np.nanargmax(angles_deg))
        normal_stats = {
            "method": "nearest-original-face-centroid",
            "tolerance_deg": float(normal_tolerance_deg),
            "mean_deg": float(valid_angles.mean()),
            "p95_deg": float(np.percentile(valid_angles, 95)),
            "max_deg": float(valid_angles.max()),
            "over_tolerance_count": int(over.size),
            "over_tolerance_face_ids": over[:20].astype(int).tolist(),
            "worst_face": {
                "output_face": worst_face,
                "angle_deg": float(angles_deg[worst_face]),
                "nearest_original_face": int(nearest_faces[worst_face]),
            },
        }
    else:
        normal_stats = {
            "method": "nearest-original-face-centroid",
            "tolerance_deg": float(normal_tolerance_deg),
            "mean_deg": 0.0,
            "p95_deg": 0.0,
            "max_deg": 0.0,
            "over_tolerance_count": 0,
            "over_tolerance_face_ids": [],
            "worst_face": None,
        }

    provenance_stats = _provenance_normal_stats(
        original_normals.face_normals,
        output_normals.face_normals,
        face_provenance,
        normal_tolerance_deg,
    )
    topology = build_topology(output) if output.face_count else None
    topology_summary = topology.summary() if topology else {
        "vertex_count": output.vertex_count,
        "face_count": 0,
        "edge_count": 0,
        "boundary_edge_count": 0,
        "nonmanifold_edge_count": 0,
        "face_component_count": 0,
    }
    topology_ok = (
        int(topology_summary["boundary_edge_count"]) == 0
        and int(topology_summary["nonmanifold_edge_count"]) == 0
    )
    flagged = (
        normal_stats["over_tolerance_count"] > 0
        or provenance_stats["over_tolerance_count"] > 0
        or not topology_ok
    )
    return {
        "status": "flagged" if flagged else "ok",
        "flagged": flagged,
        "normal_deviation": normal_stats,
        "provenance_normal_deviation": provenance_stats,
        "topology": topology_summary,
        "topology_ok": topology_ok,
        "counts": {
            "input_vertices": original.vertex_count,
            "output_vertices": output.vertex_count,
            "input_faces": original.face_count,
            "output_faces": output.face_count,
            "welded_vertices": (
                int(weld_summary["output_vertex_count"]) if weld_summary else output.vertex_count
            ),
            "duplicate_vertices_merged": (
                int(weld_summary["duplicate_count"]) if weld_summary else 0
            ),
            "filled_faces": (
                int(fill_summary["filled_face_count"]) if fill_summary else 0
            ),
            "split_edges": (
                int(split_summary["split_edge_count"]) if split_summary else 0
            ),
            "removed_fragment_faces": (
                int(fragment_summary["removed_face_count"]) if fragment_summary else 0
            ),
        },
    }


def _provenance_normal_stats(
    original_normals: np.ndarray,
    output_normals: np.ndarray,
    provenance: np.ndarray | None,
    tolerance_deg: float,
) -> dict[str, Any]:
    if provenance is None:
        return {
            "available": False,
            "over_tolerance_count": 0,
            "over_tolerance_face_ids": [],
            "max_deg": 0.0,
        }
    provenance = np.asarray(provenance, dtype=np.int64)
    valid = np.flatnonzero(
        (provenance >= 0)
        & (provenance < original_normals.shape[0])
        & (np.arange(provenance.size) < output_normals.shape[0])
    )
    if valid.size == 0:
        return {
            "available": True,
            "matched_face_count": 0,
            "over_tolerance_count": 0,
            "over_tolerance_face_ids": [],
            "max_deg": 0.0,
            "worst": None,
        }
    dots = np.einsum(
        "ij,ij->i",
        output_normals[valid],
        original_normals[provenance[valid]],
    )
    angles = np.degrees(np.arccos(np.clip(dots, -1.0, 1.0)))
    over = valid[angles > tolerance_deg]
    max_index = int(np.argmax(angles))
    output_face = int(valid[max_index])
    return {
        "available": True,
        "matched_face_count": int(valid.size),
        "tolerance_deg": float(tolerance_deg),
        "mean_deg": float(angles.mean()),
        "max_deg": float(angles.max()),
        "over_tolerance_count": int(over.size),
        "over_tolerance_face_ids": over[:20].astype(int).tolist(),
        "worst": {
            "output_face": output_face,
            "original_face": int(provenance[output_face]),
            "angle_deg": float(angles[max_index]),
        },
    }
