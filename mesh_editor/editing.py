from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Callable, Iterable

import numpy as np

from .errors import EditCancelled
from .model import Face, Mesh, compact_mesh, compute_vertex_normals, transform_points


@dataclass(frozen=True)
class Selection:
    face_indices: tuple[int, ...] = ()
    vertex_indices: tuple[int, ...] = ()

    @classmethod
    def all_faces(cls, mesh: Mesh) -> "Selection":
        return cls(tuple(range(len(mesh.faces))), ())

    def resolve_vertices(self, mesh: Mesh) -> tuple[int, ...]:
        result = set(int(i) for i in self.vertex_indices)
        for face_id in self.face_indices:
            if face_id < 0 or face_id >= len(mesh.faces):
                raise IndexError(f"face selection index {face_id} is out of range")
            result.update(mesh.faces[face_id].vertices)
        for vertex_id in result:
            if vertex_id < 0 or vertex_id >= len(mesh.vertices):
                raise IndexError(f"vertex selection index {vertex_id} is out of range")
        return tuple(sorted(result))

    def to_dict(self) -> dict[str, list[int]]:
        return {
            "faces": sorted(set(self.face_indices)),
            "vertices": sorted(set(self.vertex_indices)),
        }


@dataclass
class EditResult:
    mesh: Mesh
    metrics_before: dict[str, object]
    metrics_after: dict[str, object]
    operation: dict[str, object]
    affected_faces: tuple[int, ...] = ()

    def trace(self) -> dict[str, object]:
        return {
            "operation": self.operation,
            "metrics_before": self.metrics_before,
            "metrics_after": self.metrics_after,
            "affected_faces": list(self.affected_faces),
        }


def transform_selection(
    mesh: Mesh,
    selection: Selection,
    translation: Iterable[float] = (0.0, 0.0, 0.0),
    rotation_degrees: Iterable[float] = (0.0, 0.0, 0.0),
    scale: float | Iterable[float] = 1.0,
) -> EditResult:
    vertex_ids = selection.resolve_vertices(mesh)
    if not vertex_ids:
        raise ValueError("selection contains no vertices")
    t = [float(x) for x in translation]
    r = [float(x) for x in rotation_degrees]
    s = [float(scale)] * 3 if isinstance(scale, (int, float)) else [float(x) for x in scale]
    if len(t) != 3 or len(r) != 3 or len(s) != 3:
        raise ValueError("translation, rotation and scale require three components")
    if any(not math.isfinite(x) for x in t + r + s) or any(x == 0.0 for x in s):
        raise ValueError("transform values must be finite and scale must be non-zero")
    before = mesh.metrics()
    ids = np.asarray(vertex_ids, dtype=np.int64)
    pivot = np.mean(mesh.vertices[ids], axis=0)
    vertices = mesh.vertices.copy()
    vertices[ids] = transform_points(vertices[ids], pivot, t, r, s)
    new_faces = [
        Face(face.vertices, face.texcoords, None, face.group)
        for face in mesh.faces
    ]
    result = Mesh(
        vertices,
        new_faces,
        list(mesh.texcoords) if mesh.texcoords is not None else None,
        None,
        {
            **mesh.metadata,
            "last_edit": "transform_selection",
        },
    )
    result.normals = compute_vertex_normals(result)
    operation = {
        "type": "transform_selection",
        "selection": selection.to_dict(),
        "translation": t,
        "rotation_degrees": r,
        "scale": s,
        "pivot": [float(x) for x in pivot],
    }
    return EditResult(
        mesh=result,
        metrics_before=before,
        metrics_after=result.metrics(),
        operation=operation,
        affected_faces=tuple(selection.face_indices),
    )


def _edge_candidates(mesh: Mesh) -> list[tuple[float, int, int]]:
    seen: set[tuple[int, int]] = set()
    candidates: list[tuple[float, int, int]] = []
    for face in mesh.faces:
        ring = face.vertices
        for i, a in enumerate(ring):
            b = ring[(i + 1) % len(ring)]
            edge = (min(a, b), max(a, b))
            if edge in seen:
                continue
            seen.add(edge)
            length = float(np.linalg.norm(mesh.vertices[edge[0]] - mesh.vertices[edge[1]]))
            candidates.append((length, edge[0], edge[1]))
    candidates.sort()
    return candidates


def _collapse_once(mesh: Mesh, u: int, v: int) -> Mesh | None:
    if u == v:
        return None
    vertices = mesh.vertices.copy()
    vertices[u] = (vertices[u] + vertices[v]) * 0.5
    new_faces: list[Face] = []
    for face in mesh.faces:
        mapped: list[int] = []
        for index in face.vertices:
            mapped.append(u if index == v else index)
        cleaned: list[int] = []
        for index in mapped:
            if not cleaned or cleaned[-1] != index:
                cleaned.append(index)
        if len(cleaned) > 1 and cleaned[0] == cleaned[-1]:
            cleaned.pop()
        if len(cleaned) < 3 or len(set(cleaned)) < 3:
            continue
        try:
            new_faces.append(Face(tuple(cleaned), face.texcoords, None, face.group))
        except ValueError:
            return None
    if not new_faces:
        return None
    try:
        result = compact_mesh(vertices, new_faces, None, list(mesh.texcoords) if mesh.texcoords else None)
    except ValueError:
        return None
    result.metadata = {**mesh.metadata, "last_edit": "edge_collapse"}
    result.normals = compute_vertex_normals(result)
    return result


def simplify_edge_collapse(
    mesh: Mesh,
    *,
    target_ratio: float = 0.5,
    target_faces: int | None = None,
    max_steps: int | None = None,
    progress: Callable[[float], None] | None = None,
    cancelled: Callable[[], bool] | None = None,
) -> EditResult:
    if not math.isfinite(target_ratio) or target_ratio <= 0.0 or target_ratio > 1.0:
        raise ValueError("target ratio must be in (0, 1]")
    before = mesh.metrics()
    initial_faces = len(mesh.faces)
    desired = target_faces if target_faces is not None else max(1, int(round(initial_faces * target_ratio)))
    desired = max(1, min(desired, initial_faces))
    if max_steps is None:
        max_steps = max(1, initial_faces * 4)
    current = mesh.copy()
    steps = 0
    while len(current.faces) > desired and steps < max_steps:
        if cancelled is not None and cancelled():
            raise EditCancelled()
        candidates = _edge_candidates(current)
        collapsed = None
        used: set[tuple[int, int]] = set()
        for length, u, v in candidates:
            if length <= 1e-15 or (u, v) in used:
                continue
            candidate = _collapse_once(current, u, v)
            if candidate is not None and len(candidate.faces) < len(current.faces):
                collapsed = candidate
                break
            used.add((u, v))
        if collapsed is None:
            break
        current = collapsed
        steps += 1
        if progress is not None:
            progress(min(0.99, steps / max(1, initial_faces - desired)))
    if cancelled is not None and cancelled():
        raise EditCancelled()
    operation = {
        "type": "edge_collapse",
        "target_ratio": float(target_ratio),
        "target_faces": int(desired),
        "max_steps": int(max_steps),
        "steps_applied": int(steps),
    }
    if progress is not None:
        progress(1.0)
    return EditResult(
        mesh=current,
        metrics_before=before,
        metrics_after=current.metrics(),
        operation=operation,
        affected_faces=tuple(range(len(current.faces))),
    )
