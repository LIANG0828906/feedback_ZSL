from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterator

import numpy as np


@dataclass
class Mesh:
    """A triangular mesh with compact float64 vertices and int64 indices."""

    vertices: np.ndarray
    faces: np.ndarray

    def __post_init__(self) -> None:
        self.vertices = np.asarray(self.vertices, dtype=np.float64)
        self.faces = np.asarray(self.faces, dtype=np.int64)
        if self.vertices.ndim != 2 or self.vertices.shape[1] != 3:
            raise ValueError("vertices must have shape (N, 3)")
        if self.faces.ndim != 2 or self.faces.shape[1] != 3:
            raise ValueError("faces must have shape (M, 3)")

    @property
    def vertex_count(self) -> int:
        return int(self.vertices.shape[0])

    @property
    def face_count(self) -> int:
        return int(self.faces.shape[0])

    def copy(self) -> "Mesh":
        return Mesh(self.vertices.copy(), self.faces.copy())

    def referenced_vertices(self) -> np.ndarray:
        if self.face_count == 0:
            return np.empty(0, dtype=np.int64)
        return np.unique(self.faces.reshape(-1))

    def bounding_box(self) -> tuple[np.ndarray, np.ndarray]:
        if self.vertex_count == 0:
            return np.zeros(3), np.zeros(3)
        return self.vertices.min(axis=0), self.vertices.max(axis=0)

    def face_centroids(self) -> np.ndarray:
        if self.face_count == 0:
            return np.empty((0, 3), dtype=np.float64)
        return self.vertices[self.faces].mean(axis=1)

    def compact(self) -> tuple["Mesh", np.ndarray]:
        """Drop unreferenced vertices and return old-to-new mapping (-1 if dropped)."""

        old_to_new = np.full(self.vertex_count, -1, dtype=np.int64)
        if self.face_count == 0:
            return Mesh(np.empty((0, 3), np.float64), np.empty((0, 3), np.int64)), old_to_new
        used = np.unique(self.faces.reshape(-1))
        old_to_new[used] = np.arange(used.size, dtype=np.int64)
        return Mesh(self.vertices[used], old_to_new[self.faces]), old_to_new

    def iter_faces(self) -> Iterator[tuple[int, np.ndarray, float]]:
        for face_id, face in enumerate(self.faces):
            a, b, c = self.vertices[face]
            normal = np.cross(b - a, c - a)
            yield face_id, face, float(np.linalg.norm(normal))


def empty_mesh() -> Mesh:
    return Mesh(np.empty((0, 3), np.float64), np.empty((0, 3), np.int64))


def as_json_number(value: float) -> float:
    if np.isfinite(value):
        return float(value)
    return 0.0
