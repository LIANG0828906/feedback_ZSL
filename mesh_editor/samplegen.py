from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .formats import (
    ExportReport,
    export_mesh,
    loss_report,
    parse_mesh,
    serialize_obj,
    serialize_stl_ascii,
    serialize_stl_binary,
)
from .model import Face, Mesh
from .topology import build_halfedge_topology, weld_vertices
from .versioning import VersionRepository, sha256_bytes


def build_demo_mesh() -> Mesh:
    vertices = np.asarray(
        [
            [0.0, 0.0, 0.0],
            [1.0, 0.0, 0.0],
            [1.0, 1.0, 0.0],
            [0.0, 1.0, 0.0],
            [0.0, 0.0, 1.0],
            [1.0, 0.0, 1.0],
            [1.0, 1.0, 1.0],
            [0.0, 1.0, 1.0],
            [0.0, 0.0, 0.0],
            [0.25, 0.25, 0.0],
            [0.25, 0.25, 0.5],
            [0.25, 0.25, 1.0],
        ],
        dtype=np.float64,
    )
    groups = [
        "bottom",
        "bottom",
        "top",
        "top",
        "front",
        "front",
        "right",
        "right",
        "back",
        "back",
        "left",
        "left",
        "defect",
        "defect",
    ]
    vertex_faces = [
        (0, 2, 1),
        (8, 3, 2),
        (4, 5, 6),
        (4, 6, 7),
        (0, 1, 5),
        (0, 5, 4),
        (1, 2, 6),
        (1, 6, 5),
        (2, 3, 7),
        (2, 7, 6),
        (3, 0, 4),
        (3, 4, 7),
        (0, 1, 6),
        (9, 10, 11),
    ]
    texcoords = [(float(vertex[0]), float(vertex[1])) for vertex in vertices]
    faces: list[Face] = []
    for face_id, (indices, group) in enumerate(zip(vertex_faces, groups)):
        corner_uv = tuple(indices)
        corner_normals = (face_id, face_id, face_id)
        faces.append(Face(indices, corner_uv, corner_normals, group))
    face_normals = np.zeros((len(faces), 3), dtype=np.float64)
    for face_id, (a, b, c) in enumerate(vertex_faces):
        normal = np.cross(vertices[b] - vertices[a], vertices[c] - vertices[a])
        length = np.linalg.norm(normal)
        if length > 1e-15:
            face_normals[face_id] = normal / length
    return Mesh(
        vertices,
        faces,
        texcoords,
        face_normals,
        {"source_format": "generated", "format_version": 2, "contains_defects": True},
    )


def _write_bytes(path: Path, payload: bytes) -> dict[str, Any]:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(payload)
    return {"path": path.name, "bytes": len(payload), "sha256": sha256_bytes(payload)}


def generate_project_samples(
    project_root: str | Path,
    *,
    weld_tolerance: float = 1e-6,
    reset_workspace: bool = True,
) -> dict[str, Any]:
    project_root = Path(project_root).resolve()
    data_dir = project_root / "data"
    sample_dir = data_dir / "samples"
    logs_dir = project_root / "logs"
    workspace = data_dir / "workspace"
    sample_dir.mkdir(parents=True, exist_ok=True)
    logs_dir.mkdir(parents=True, exist_ok=True)
    if reset_workspace and workspace.exists():
        shutil.rmtree(workspace)

    mesh = build_demo_mesh()
    default_groups_mesh = Mesh(
        mesh.vertices.copy(),
        [
            Face(face.vertices, face.texcoords, face.normals, "default")
            for face in mesh.faces
        ],
        list(mesh.texcoords) if mesh.texcoords is not None else None,
        mesh.normals.copy() if mesh.normals is not None else None,
        dict(mesh.metadata),
    )
    artifacts: dict[str, Any] = {}
    artifacts["obj_v2"] = _write_bytes(sample_dir / "part_v2.obj", serialize_obj(mesh, 2))
    artifacts["obj_v1"] = _write_bytes(
        sample_dir / "part_v1.obj", serialize_obj(default_groups_mesh, 1)
    )
    artifacts["stl_ascii"] = _write_bytes(
        sample_dir / "part_ascii.stl", serialize_stl_ascii(mesh, 2, strict=False)
    )
    artifacts["stl_binary"] = _write_bytes(
        sample_dir / "part_binary.stl", serialize_stl_binary(mesh, 2, strict=False)
    )

    parsed_v2 = parse_mesh(sample_dir / "part_v2.obj")
    welded = weld_vertices(parsed_v2, weld_tolerance)
    repo = VersionRepository(workspace)
    baseline = repo.create_baseline(
        welded.mesh,
        weld_tolerance=weld_tolerance,
        format_version=2,
        operation={
            "type": "baseline",
            "source": "data/samples/part_v2.obj",
            "weld_tolerance": weld_tolerance,
        },
    )
    repo.write_trace(logs_dir / "baseline_trace.jsonl")
    baseline_mesh, current_entry = repo.load_current()

    raw_topology = build_halfedge_topology(parsed_v2)
    welded_topology = welded.topology
    loss_faces, loss_details = loss_report(mesh, "stl", 2)
    obj_v1_loss_faces, obj_v1_loss_details = loss_report(mesh, "obj", 1)
    report = {
        "schema": "mesh-editor-sample-generation-v1",
        "artifacts": artifacts,
        "counts": {
            "raw_vertices": int(len(parsed_v2.vertices)),
            "raw_faces": int(len(parsed_v2.faces)),
            "welded_vertices": int(len(baseline_mesh.vertices)),
            "welded_faces": int(len(baseline_mesh.faces)),
            "merged_vertices": welded.merged_vertices,
            "raw_non_manifold_edges": raw_topology.non_manifold_edge_count,
            "welded_non_manifold_edges": welded_topology.non_manifold_edge_count,
            "raw_boundary_loops": len(raw_topology.boundary_loops),
            "welded_boundary_loops": len(welded_topology.boundary_loops),
            "degenerate_faces": [13],
        },
        "weld_tolerance": weld_tolerance,
        "weld_mapping": welded.mapping_dict(),
        "stl_loss": {"faces": loss_faces, "details": loss_details},
        "obj_v1_loss": {"faces": obj_v1_loss_faces, "details": obj_v1_loss_details},
        "baseline": {
            "version": baseline["version"],
            "content_hash": baseline["content_hash"],
            "entry_hash": baseline["entry_hash"],
            "metrics": current_entry["metrics_after"],
        },
    }
    log_payload = json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False) + "\n"
    (logs_dir / "sample_generation.log").write_text(log_payload, encoding="utf-8")
    return report
