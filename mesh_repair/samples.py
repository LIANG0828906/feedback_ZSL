from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .binary import file_sha256, load_mesh, write_mesh
from .boundary import boundary_report
from .mesh import Mesh
from .topology import build_topology
from .weld import weld_vertices


def build_scan_sample(
    *,
    rings: int = 8,
    segments: int = 12,
    duplicate_offset: float = 0.0,
) -> Mesh:
    """Build a deterministic sphere-like scan with all requested defects."""

    vertices: list[list[float]] = [[0.0, 0.0, 1.0]]
    ring_ids: list[list[int]] = []
    for ring in range(1, rings):
        theta = math.pi * ring / rings
        ids: list[int] = []
        for segment in range(segments):
            phi = 2.0 * math.pi * segment / segments
            ids.append(len(vertices))
            vertices.append(
                [
                    math.sin(theta) * math.cos(phi),
                    math.sin(theta) * math.sin(phi),
                    math.cos(theta),
                ]
            )
        ring_ids.append(ids)
    bottom = len(vertices)
    vertices.append([0.0, 0.0, -1.0])

    faces: list[list[int]] = []
    top = 0
    for segment in range(segments):
        nxt = (segment + 1) % segments
        # Omit two adjacent cap faces to create a real four-vertex hole.
        if segment in (0, 1):
            continue
        faces.append([top, ring_ids[0][segment], ring_ids[0][nxt]])

    for ring in range(len(ring_ids) - 1):
        upper = ring_ids[ring]
        lower = ring_ids[ring + 1]
        for segment in range(segments):
            nxt = (segment + 1) % segments
            faces.append([upper[segment], lower[segment], upper[nxt]])
            faces.append([upper[nxt], lower[segment], lower[nxt]])

    last = ring_ids[-1]
    for segment in range(segments):
        nxt = (segment + 1) % segments
        faces.append([bottom, last[segment], last[nxt]])

    # Duplicate seam vertices on one side, producing a visible crack before
    # welding. Exact duplicates are welded at any finite tolerance.
    for ring in (1, 3, 5):
        source = ring_ids[ring - 1][0]
        duplicate = len(vertices)
        point = np.asarray(vertices[source], dtype=np.float64)
        point = point + np.asarray([duplicate_offset, 0.0, 0.0])
        vertices.append(point.tolist())
        for face_id, face in enumerate(faces):
            if source in face and face_id % 3 == 0:
                faces[face_id] = [duplicate if value == source else value for value in face]

    # Duplicate a few interior vertices and remap selected incident faces.
    for source in (
        ring_ids[2][3],
        ring_ids[4][7],
        ring_ids[5][2],
    ):
        duplicate = len(vertices)
        point = np.asarray(vertices[source], dtype=np.float64)
        point = point + np.asarray([0.0, duplicate_offset, 0.0])
        vertices.append(point.tolist())
        hit_count = 0
        for face_id, face in enumerate(faces):
            if source in face and hit_count < 2:
                faces[face_id] = [duplicate if value == source else value for value in face]
                hit_count += 1

    # Add a non-manifold fin attached to an existing interior edge.
    anchor_face = faces[len(faces) // 2]
    u, v, w = anchor_face
    midpoint = (
        np.asarray(vertices[u]) + np.asarray(vertices[v]) + np.asarray(vertices[w])
    ) / 3.0
    normal = np.cross(
        np.asarray(vertices[v]) - np.asarray(vertices[u]),
        np.asarray(vertices[w]) - np.asarray(vertices[u]),
    )
    normal /= max(float(np.linalg.norm(normal)), 1e-15)
    fin_vertex = len(vertices)
    vertices.append((midpoint + normal * 0.35).tolist())
    faces.append([u, v, fin_vertex])

    # Add a two-triangle isolated fragment far from the main shell.
    base = len(vertices)
    vertices.extend(
        [
            [5.0, 5.0, 5.0],
            [5.4, 5.0, 5.0],
            [5.0, 5.4, 5.0],
            [5.0, 5.0, 5.4],
        ]
    )
    faces.append([base, base + 1, base + 2])
    faces.append([base, base + 2, base + 3])

    return Mesh(np.asarray(vertices, dtype=np.float64), np.asarray(faces, dtype=np.int64))


def baseline_summary(mesh: Mesh, *, tolerance: float = 1e-6) -> dict[str, Any]:
    topology = build_topology(mesh)
    weld = weld_vertices(mesh, tolerance)
    welded_topology = build_topology(weld.mesh)
    bbox_min, bbox_max = mesh.bounding_box()
    return {
        "vertices": mesh.vertex_count,
        "faces": mesh.face_count,
        "bbox_min": bbox_min.tolist(),
        "bbox_max": bbox_max.tolist(),
        "raw_topology": topology.summary(),
        "raw_boundaries": boundary_report(mesh, topology),
        "weld_tolerance": tolerance,
        "weld": weld.summary(),
        "welded_topology": welded_topology.summary(),
        "welded_boundaries": boundary_report(weld.mesh, welded_topology),
        "expected_defects": {
            "has_hole": True,
            "has_nonmanifold_edge": len(topology.nonmanifold_edges) > 0,
            "has_duplicate_vertices": weld.duplicate_count > 0,
            "has_isolated_fragment": len(topology.face_components()) > 1,
        },
    }


def generate_sample_bundle(
    outdir: str | Path = "samples",
) -> dict[str, Any]:
    target = Path(outdir)
    target.mkdir(parents=True, exist_ok=True)
    exact = build_scan_sample(duplicate_offset=0.0)
    tolerance = build_scan_sample(duplicate_offset=2e-4)

    exact_path = target / "scan_asset.msh"
    tolerance_path = target / "tolerance_asset.msh"
    f32_path = target / "scan_asset_f32_u16.msh"
    exact_sha = write_mesh(exact_path, exact, coord_type="f64", index_width=4)
    tolerance_sha = write_mesh(tolerance_path, tolerance, coord_type="f64", index_width=4)
    f32_sha = write_mesh(f32_path, exact, coord_type="f32", index_width=2)

    loaded, header = load_mesh(exact_path)
    summary = baseline_summary(loaded)
    summary.update(
        {
            "sample": "scan_asset",
            "path": str(exact_path),
            "sha256": exact_sha,
            "header": header.to_dict(),
            "variants": {
                "tolerance_asset": {
                    "path": str(tolerance_path),
                    "sha256": tolerance_sha,
                    "duplicate_offset": 2e-4,
                },
                "scan_asset_f32_u16": {
                    "path": str(f32_path),
                    "sha256": f32_sha,
                },
            },
        }
    )
    baseline_path = target / "scan_asset_baseline.json"
    baseline_path.write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    summary["baseline_path"] = str(baseline_path)
    return summary


def file_digest(path: str | Path) -> str:
    return file_sha256(path)
