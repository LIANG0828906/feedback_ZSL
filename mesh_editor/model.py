from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass, field, replace
from typing import Iterable, Iterator, Sequence

import numpy as np


@dataclass(frozen=True)
class Face:
    vertices: tuple[int, ...]
    texcoords: tuple[int, ...] | None = None
    normals: tuple[int, ...] | None = None
    group: str = "default"

    def __post_init__(self) -> None:
        if len(self.vertices) < 3:
            raise ValueError("a face needs at least three vertices")
        if len(set(self.vertices)) != len(self.vertices):
            raise ValueError("a face cannot repeat a vertex index")
        if self.texcoords is not None and len(self.texcoords) != len(self.vertices):
            raise ValueError("texture-coordinate count must match face corners")
        if self.normals is not None and len(self.normals) != len(self.vertices):
            raise ValueError("normal count must match face corners")


def _float_list(array: np.ndarray) -> list[list[float]]:
    return [[float(x) for x in row] for row in np.asarray(array, dtype=np.float64)]


@dataclass
class Mesh:
    """Internal mesh representation.

    Faces may be polygons. Rendering, volume and precision sampling fan-triangulate
    them deterministically. Vertex, texture-coordinate and normal indices are all
    global, so v2 face attributes and sharing survive internal upgrades.
    """

    vertices: np.ndarray
    faces: list[Face]
    texcoords: list[tuple[float, ...]] | None = None
    normals: np.ndarray | None = None
    metadata: dict[str, object] = field(default_factory=dict)

    def __post_init__(self) -> None:
        self.vertices = np.ascontiguousarray(self.vertices, dtype=np.float64)
        if self.vertices.ndim != 2 or self.vertices.shape[1] != 3:
            raise ValueError("vertices must have shape (n, 3)")
        if not np.isfinite(self.vertices).all():
            raise ValueError("vertices contain non-finite values")
        if self.normals is not None:
            self.normals = np.ascontiguousarray(self.normals, dtype=np.float64)
            if self.normals.ndim != 2 or self.normals.shape[1] != 3:
                raise ValueError("normals must have shape (n, 3)")
            if not np.isfinite(self.normals).all():
                raise ValueError("normals contain non-finite values")
        self.validate_indices()

    def validate_indices(self) -> None:
        nv = len(self.vertices)
        nt = len(self.texcoords) if self.texcoords is not None else 0
        nn = len(self.normals) if self.normals is not None else 0
        for face_id, face in enumerate(self.faces):
            for idx in face.vertices:
                if idx < 0 or idx >= nv:
                    raise ValueError(f"face {face_id} references vertex {idx}")
            if face.texcoords is not None:
                if self.texcoords is None:
                    raise ValueError(f"face {face_id} has texture coordinates but none exist")
                for idx in face.texcoords:
                    if idx < 0 or idx >= nt:
                        raise ValueError(f"face {face_id} references texcoord {idx}")
            if face.normals is not None:
                if self.normals is None:
                    raise ValueError(f"face {face_id} has normals but none exist")
                for idx in face.normals:
                    if idx < 0 or idx >= nn:
                        raise ValueError(f"face {face_id} references normal {idx}")

    def copy(self) -> "Mesh":
        return Mesh(
            self.vertices.copy(),
            [replace(f) for f in self.faces],
            list(self.texcoords) if self.texcoords is not None else None,
            self.normals.copy() if self.normals is not None else None,
            dict(self.metadata),
        )

    def triangle_iter(self) -> Iterator[tuple[int, int, int, int]]:
        """Yield (face_id, a, b, c) for a deterministic fan triangulation."""

        for face_id, face in enumerate(self.faces):
            v = face.vertices
            for i in range(1, len(v) - 1):
                yield face_id, v[0], v[i], v[i + 1]

    def triangle_arrays(self) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        tris: list[tuple[int, int, int]] = []
        face_ids: list[int] = []
        for face_id, a, b, c in self.triangle_iter():
            tris.append((a, b, c))
            face_ids.append(face_id)
        if not tris:
            return (
                np.empty((0, 3), dtype=np.int64),
                np.empty((0, 3), dtype=np.float64),
                np.empty((0,), dtype=np.int64),
            )
        idx = np.asarray(tris, dtype=np.int64)
        return idx, self.vertices[idx], np.asarray(face_ids, dtype=np.int64)

    def face_normals(self) -> np.ndarray:
        result = np.zeros((len(self.faces), 3), dtype=np.float64)
        for face_id, a, b, c in self.triangle_iter():
            p0 = self.vertices[a]
            n = np.cross(self.vertices[b] - p0, self.vertices[c] - p0)
            result[face_id] += n
        lengths = np.linalg.norm(result, axis=1)
        valid = lengths > 1e-15
        result[valid] /= lengths[valid, None]
        return result

    def triangulated_normals(self) -> np.ndarray:
        idx, tri_vertices, _ = self.triangle_arrays()
        if len(idx) == 0:
            return np.empty((0, 3), dtype=np.float64)
        n = np.cross(tri_vertices[:, 1] - tri_vertices[:, 0], tri_vertices[:, 2] - tri_vertices[:, 0])
        lengths = np.linalg.norm(n, axis=1)
        valid = lengths > 1e-15
        n[valid] /= lengths[valid, None]
        return n

    def bounding_box(self) -> tuple[list[float], list[float]]:
        if len(self.vertices) == 0:
            return [0.0, 0.0, 0.0], [0.0, 0.0, 0.0]
        return (
            [float(x) for x in np.min(self.vertices, axis=0)],
            [float(x) for x in np.max(self.vertices, axis=0)],
        )

    def volume(self) -> float:
        total = 0.0
        for _, a, b, c in self.triangle_iter():
            p0 = self.vertices[a]
            p1 = self.vertices[b]
            p2 = self.vertices[c]
            total += float(np.dot(p0, np.cross(p1, p2))) / 6.0
        return abs(total)

    def metrics(self) -> dict[str, object]:
        bbox_min, bbox_max = self.bounding_box()
        return {
            "vertices": int(len(self.vertices)),
            "faces": int(len(self.faces)),
            "triangles": int(sum(max(0, len(f.vertices) - 2) for f in self.faces)),
            "bbox_min": bbox_min,
            "bbox_max": bbox_max,
            "bbox_size": [float(bbox_max[i] - bbox_min[i]) for i in range(3)],
            "volume": float(self.volume()),
            "groups": sorted({f.group for f in self.faces}),
        }

    def used_vertices(self) -> set[int]:
        return {idx for face in self.faces for idx in face.vertices}

    def canonical_dict(self) -> dict[str, object]:
        return {
            "vertices": _float_list(self.vertices),
            "faces": [
                {
                    "vertices": list(f.vertices),
                    "texcoords": list(f.texcoords) if f.texcoords is not None else None,
                    "normals": list(f.normals) if f.normals is not None else None,
                    "group": f.group,
                }
                for f in self.faces
            ],
            "texcoords": _float_list(np.asarray(self.texcoords, dtype=np.float64))
            if self.texcoords is not None
            else None,
            "normals": _float_list(self.normals) if self.normals is not None else None,
        }

    def content_fingerprint(self) -> str:
        canonical = repr(self.canonical_dict()).encode("utf-8")
        return "sha256:" + hashlib.sha256(canonical).hexdigest()


def compute_vertex_normals(mesh: Mesh) -> np.ndarray:
    normals = np.zeros_like(mesh.vertices, dtype=np.float64)
    for _, a, b, c in mesh.triangle_iter():
        p0 = mesh.vertices[a]
        n = np.cross(mesh.vertices[b] - p0, mesh.vertices[c] - p0)
        normals[a] += n
        normals[b] += n
        normals[c] += n
    lengths = np.linalg.norm(normals, axis=1)
    valid = lengths > 1e-15
    normals[valid] /= lengths[valid, None]
    return normals


def compact_mesh(
    vertices: np.ndarray,
    faces: Sequence[Face],
    normals: np.ndarray | None = None,
    texcoords: list[tuple[float, ...]] | None = None,
) -> Mesh:
    used = sorted({idx for face in faces for idx in face.vertices})
    remap = {old: new for new, old in enumerate(used)}
    new_vertices = vertices[np.asarray(used, dtype=np.int64)] if used else np.empty((0, 3))
    new_normals = normals[np.asarray(used, dtype=np.int64)] if normals is not None else None
    new_faces = [
        replace(
            face,
            vertices=tuple(remap[i] for i in face.vertices),
        )
        for face in faces
    ]
    return Mesh(new_vertices, new_faces, texcoords, new_normals)


def transform_points(
    points: np.ndarray,
    pivot: np.ndarray,
    translation: Iterable[float],
    rotation_degrees: Iterable[float],
    scale: float | Iterable[float],
) -> np.ndarray:
    t = np.asarray(list(translation), dtype=np.float64)
    r = np.asarray(list(rotation_degrees), dtype=np.float64) * (math.pi / 180.0)
    s = np.asarray(scale if not isinstance(scale, (int, float)) else [scale] * 3, dtype=np.float64)
    if t.shape != (3,) or r.shape != (3,) or s.shape != (3,):
        raise ValueError("translation, rotation and scale must have three components")
    cx, cy, cz = np.cos(r)
    sx, sy, sz = np.sin(r)
    rx = np.array([[1, 0, 0], [0, cx, -sx], [0, sx, cx]], dtype=np.float64)
    ry = np.array([[cy, 0, sy], [0, 1, 0], [-sy, 0, cy]], dtype=np.float64)
    rz = np.array([[cz, -sz, 0], [sz, cz, 0], [0, 0, 1]], dtype=np.float64)
    rotation = rz @ ry @ rx
    local = (points - pivot) * s
    return (local @ rotation.T) + pivot + t
