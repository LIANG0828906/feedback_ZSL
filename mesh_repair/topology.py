from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .mesh import Mesh


Edge = tuple[int, int]


@dataclass(frozen=True)
class EdgeIncidence:
    edge: Edge
    faces: tuple[int, ...]
    directed: tuple[tuple[int, int], ...]

    @property
    def is_boundary(self) -> bool:
        return len(self.faces) == 1

    @property
    def is_nonmanifold(self) -> bool:
        return len(self.faces) > 2


@dataclass
class Topology:
    mesh: Mesh
    edge_table: dict[Edge, tuple[int, ...]]
    directed_edges: dict[Edge, tuple[tuple[int, int], ...]]
    vertex_faces: tuple[tuple[int, ...], ...]
    vertex_neighbors: tuple[tuple[int, ...], ...]

    @property
    def edge_count(self) -> int:
        return len(self.edge_table)

    @property
    def boundary_edges(self) -> tuple[EdgeIncidence, ...]:
        return tuple(
            EdgeIncidence(edge, faces, self.directed_edges.get(edge, ()))
            for edge, faces in self.edge_table.items()
            if len(faces) == 1
        )

    @property
    def nonmanifold_edges(self) -> tuple[EdgeIncidence, ...]:
        return tuple(
            EdgeIncidence(edge, faces, self.directed_edges.get(edge, ()))
            for edge, faces in self.edge_table.items()
            if len(faces) > 2
        )

    @property
    def manifold_edge_count(self) -> int:
        return sum(1 for faces in self.edge_table.values() if len(faces) <= 2)

    def incidence_histogram(self) -> dict[str, int]:
        histogram: dict[str, int] = {"1": 0, "2": 0, "nonmanifold": 0}
        for faces in self.edge_table.values():
            if len(faces) == 1:
                histogram["1"] += 1
            elif len(faces) == 2:
                histogram["2"] += 1
            else:
                histogram["nonmanifold"] += 1
        return histogram

    def face_components(self, *, manifold_only: bool = False) -> list[list[int]]:
        """Return deterministic face components.

        With ``manifold_only`` faces are connected across edges of incidence
        two only. This is used to split non-manifold edge fans into independent
        face groups without changing face order.
        """

        parent = list(range(self.mesh.face_count))

        def find(x: int) -> int:
            while parent[x] != x:
                parent[x] = parent[parent[x]]
                x = parent[x]
            return x

        def union(a: int, b: int) -> None:
            ra, rb = find(a), find(b)
            if ra == rb:
                return
            if ra > rb:
                ra, rb = rb, ra
            parent[rb] = ra

        for faces in self.edge_table.values():
            if len(faces) == 2 and (manifold_only or len(faces) <= 2):
                union(faces[0], faces[1])
        groups: dict[int, list[int]] = {}
        for face_id in range(self.mesh.face_count):
            groups.setdefault(find(face_id), []).append(face_id)
        return sorted(groups.values(), key=lambda group: group[0])

    def summary(self) -> dict[str, object]:
        boundary = self.boundary_edges
        nonmanifold = self.nonmanifold_edges
        return {
            "vertex_count": self.mesh.vertex_count,
            "face_count": self.mesh.face_count,
            "edge_count": self.edge_count,
            "edge_incidence": self.incidence_histogram(),
            "boundary_edge_count": len(boundary),
            "nonmanifold_edge_count": len(nonmanifold),
            "nonmanifold_edges": [list(item.edge) for item in nonmanifold],
            "face_component_count": len(self.face_components()),
        }


def build_topology(mesh: Mesh) -> Topology:
    edge_faces: dict[Edge, list[int]] = {}
    edge_directed: dict[Edge, list[tuple[int, int]]] = {}
    vertex_faces: list[list[int]] = [[] for _ in range(mesh.vertex_count)]
    vertex_neighbors: list[set[int]] = [set() for _ in range(mesh.vertex_count)]

    for face_id, face in enumerate(mesh.faces):
        a, b, c = (int(face[0]), int(face[1]), int(face[2]))
        for vertex in (a, b, c):
            vertex_faces[vertex].append(face_id)
        for u, v in ((a, b), (b, c), (c, a)):
            edge = (u, v) if u < v else (v, u)
            edge_faces.setdefault(edge, []).append(face_id)
            edge_directed.setdefault(edge, []).append((u, v))
            vertex_neighbors[u].add(v)
            vertex_neighbors[v].add(u)

    return Topology(
        mesh=mesh,
        edge_table={edge: tuple(faces) for edge, faces in sorted(edge_faces.items())},
        directed_edges={
            edge: tuple(directed) for edge, directed in sorted(edge_directed.items())
        },
        vertex_faces=tuple(tuple(faces) for faces in vertex_faces),
        vertex_neighbors=tuple(tuple(sorted(neighbors)) for neighbors in vertex_neighbors),
    )


def face_components_from_edges(
    face_count: int, edge_faces: Iterable[tuple[int, ...]], *, manifold_only: bool = False
) -> list[list[int]]:
    parent = list(range(face_count))

    def find(x: int) -> int:
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a: int, b: int) -> None:
        ra, rb = find(a), find(b)
        if ra == rb:
            return
        if ra > rb:
            ra, rb = rb, ra
        parent[rb] = ra

    for faces in edge_faces:
        if len(faces) == 2 and (manifold_only or len(faces) <= 2):
            union(faces[0], faces[1])
    groups: dict[int, list[int]] = {}
    for face_id in range(face_count):
        groups.setdefault(find(face_id), []).append(face_id)
    return sorted(groups.values(), key=lambda group: group[0])
