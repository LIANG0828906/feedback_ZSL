from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, Iterable

import numpy as np

from .boundary import BoundaryLoop
from .errors import BusinessError, E_BOUNDARY_PENDING, E_MESH_INVALID, SUB_INVALID_PARAMETER
from .mesh import Mesh
from .topology import Topology, build_topology


BoundaryReadiness = Callable[[Iterable[int]], tuple[bool, list[int]]]


@dataclass
class FillResult:
    mesh: Mesh
    face_provenance: np.ndarray
    loop_faces: list[dict[str, Any]]
    added_face_ids: list[int]
    added_vertex_ids: list[int]
    dependency_block_ids: list[int]

    @property
    def filled_face_count(self) -> int:
        return len(self.added_face_ids)

    def summary(self) -> dict[str, Any]:
        return {
            "filled_loop_count": len(self.loop_faces),
            "filled_face_count": self.filled_face_count,
            "added_vertex_count": len(self.added_vertex_ids),
            "loops": self.loop_faces,
            "dependency_block_ids": sorted(set(self.dependency_block_ids)),
        }


@dataclass
class SplitResult:
    mesh: Mesh
    face_provenance: np.ndarray
    split_edge_count: int
    face_group_count: int
    duplicated_vertex_count: int
    nonmanifold_edges_before: list[list[int]]

    def summary(self) -> dict[str, Any]:
        return {
            "split_edge_count": self.split_edge_count,
            "face_group_count": self.face_group_count,
            "duplicated_vertex_count": self.duplicated_vertex_count,
            "nonmanifold_edges_before": self.nonmanifold_edges_before,
        }


@dataclass
class FragmentResult:
    mesh: Mesh
    face_provenance: np.ndarray
    removed_component_count: int
    removed_face_count: int
    removed_vertex_count: int
    components: list[dict[str, Any]]
    kept_face_ids: np.ndarray

    def summary(self) -> dict[str, Any]:
        return {
            "removed_component_count": self.removed_component_count,
            "removed_face_count": self.removed_face_count,
            "removed_vertex_count": self.removed_vertex_count,
            "removed_components": self.components,
        }


@dataclass
class NormalResult:
    face_normals: np.ndarray
    face_areas: np.ndarray
    vertex_normals: np.ndarray

    def summary(self) -> dict[str, Any]:
        return {
            "face_count": int(self.face_normals.shape[0]),
            "zero_area_face_count": int(np.count_nonzero(self.face_areas <= 1e-15)),
            "finite_vertex_normal_count": int(np.count_nonzero(np.isfinite(self.vertex_normals).all(axis=1))),
        }


def _validate_loop(mesh: Mesh, loop: BoundaryLoop) -> None:
    vertices = loop.vertices
    if len(vertices) < 3 or len(set(vertices)) < 3:
        raise BusinessError(
            E_MESH_INVALID,
            "boundary loop has fewer than three distinct vertices",
            subcode=SUB_INVALID_PARAMETER,
            location={"loop_id": loop.loop_id, "vertices": list(vertices)},
        )
    if any(vertex < 0 or vertex >= mesh.vertex_count for vertex in vertices):
        raise BusinessError(
            E_MESH_INVALID,
            "boundary loop references an out-of-range vertex",
            subcode=SUB_INVALID_PARAMETER,
            location={"loop_id": loop.loop_id},
        )


def fill_boundary_loops(
    mesh: Mesh,
    loops: list[BoundaryLoop],
    *,
    face_provenance: np.ndarray | None = None,
    loop_block_ids: dict[int, set[int]] | None = None,
    readiness: BoundaryReadiness | None = None,
) -> FillResult:
    """Fill each loop with a deterministic centroid fan.

    Cross-block loops are rejected with E_BOUNDARY_PENDING until ``readiness``
    confirms every participating block has committed its weld mapping.
    """

    if face_provenance is None:
        face_provenance = np.arange(mesh.face_count, dtype=np.int64)
    else:
        face_provenance = np.asarray(face_provenance, dtype=np.int64).copy()
    if face_provenance.shape[0] != mesh.face_count:
        raise ValueError("face_provenance length must match face count")

    vertices = mesh.vertices.copy()
    faces = mesh.faces.copy()
    provenance = face_provenance.copy()
    loop_faces: list[dict[str, Any]] = []
    added_face_ids: list[int] = []
    added_vertex_ids: list[int] = []
    used_dependencies: list[int] = []

    for loop in loops:
        _validate_loop(mesh, loop)
        dependencies = sorted(
            (loop_block_ids or {}).get(loop.loop_id, set())
        )
        if dependencies:
            ready, missing = (True, []) if readiness is None else readiness(dependencies)
            if not ready:
                raise BusinessError(
                    E_BOUNDARY_PENDING,
                    "cross-block hole fill is waiting for committed weld maps",
                    location={"loop_id": loop.loop_id},
                    context={
                        "required_blocks": dependencies,
                        "missing_blocks": sorted(missing),
                    },
                )
            used_dependencies.extend(dependencies)

        loop_points = vertices[np.asarray(loop.vertices, dtype=np.int64)]
        center = loop_points.mean(axis=0)
        center_id = int(vertices.shape[0])
        vertices = np.vstack((vertices, center))
        added_vertex_ids.append(center_id)
        loop_added: list[int] = []
        for index, current in enumerate(loop.vertices):
            nxt = loop.vertices[(index + 1) % len(loop.vertices)]
            # Existing boundary half-edge is current -> nxt. The fill face
            # traverses nxt -> current so the shared edge orientation agrees.
            new_face_id = int(faces.shape[0])
            faces = np.vstack((faces, np.asarray([[nxt, current, center_id]], dtype=np.int64)))
            provenance = np.append(provenance, np.int64(-1))
            added_face_ids.append(new_face_id)
            loop_added.append(new_face_id)
        loop_faces.append(
            {
                "loop_id": loop.loop_id,
                "boundary_vertex_count": loop.vertex_count,
                "center_vertex": center_id,
                "new_face_ids": loop_added,
                "dependency_block_ids": dependencies,
            }
        )

    return FillResult(
        mesh=Mesh(vertices, faces),
        face_provenance=provenance,
        loop_faces=loop_faces,
        added_face_ids=added_face_ids,
        added_vertex_ids=added_vertex_ids,
        dependency_block_ids=used_dependencies,
    )


def split_nonmanifold_edges(
    mesh: Mesh,
    *,
    face_provenance: np.ndarray | None = None,
) -> SplitResult:
    """Split non-manifold edges by connected manifold face groups.

    The primary pass separates face components connected only through
    two-incidence edges. A deterministic local fallback then detaches any
    residual fan face so the output has no edge with more than two incident
    faces.
    """

    if face_provenance is None:
        face_provenance = np.arange(mesh.face_count, dtype=np.int64)
    provenance = np.asarray(face_provenance, dtype=np.int64).copy()
    topology = build_topology(mesh)
    if not topology.nonmanifold_edges:
        return SplitResult(
            mesh=mesh.copy(),
            face_provenance=provenance,
            split_edge_count=0,
            face_group_count=len(topology.face_components()),
            duplicated_vertex_count=0,
            nonmanifold_edges_before=[],
        )

    nonmanifold_before = [list(item.edge) for item in topology.nonmanifold_edges]
    split_edge_count = len(topology.nonmanifold_edges)
    components = topology.face_components(manifold_only=True)
    vertices = [tuple(float(value) for value in row) for row in mesh.vertices]
    owner: dict[int, int] = {}
    component_vertex_map: list[dict[int, int]] = []
    for component_index, component in enumerate(components):
        used = sorted({int(value) for face_id in component for value in mesh.faces[face_id]})
        mapping: dict[int, int] = {}
        for vertex in used:
            if vertex not in owner:
                owner[vertex] = component_index
                mapping[vertex] = vertex
            else:
                mapping[vertex] = len(vertices)
                vertices.append(vertices[vertex])
        component_vertex_map.append(mapping)

    component_of_face = {
        face_id: component_index
        for component_index, component in enumerate(components)
        for face_id in component
    }
    faces = np.empty_like(mesh.faces)
    for face_id, face in enumerate(mesh.faces):
        mapping = component_vertex_map[component_of_face[face_id]]
        faces[face_id] = [mapping[int(face[0])], mapping[int(face[1])], mapping[int(face[2])]]

    result = Mesh(np.asarray(vertices, dtype=np.float64), faces)
    result = _force_residual_nonmanifold_split(result)
    duplicated = int(result.vertex_count - mesh.vertex_count)
    return SplitResult(
        mesh=result,
        face_provenance=provenance,
        split_edge_count=split_edge_count,
        face_group_count=len(components),
        duplicated_vertex_count=duplicated,
        nonmanifold_edges_before=nonmanifold_before,
    )


def _force_residual_nonmanifold_split(mesh: Mesh) -> Mesh:
    vertices = mesh.vertices.copy()
    faces = mesh.faces.copy()
    for _ in range(mesh.face_count * 4 + 16):
        topology = build_topology(Mesh(vertices, faces))
        if not topology.nonmanifold_edges:
            return Mesh(vertices, faces)
        incidence = topology.nonmanifold_edges[0]
        u, v = incidence.edge
        # Keep the first two incident faces as one manifold pair; each extra
        # face receives its own copies of both endpoints.
        for face_id in incidence.faces[2:]:
            new_u = int(vertices.shape[0])
            vertices = np.vstack((vertices, vertices[u]))
            new_v = int(vertices.shape[0])
            vertices = np.vstack((vertices, vertices[v]))
            face = faces[face_id].copy()
            face[face == u] = new_u
            face[face == v] = new_v
            faces[face_id] = face
    raise RuntimeError("unable to eliminate non-manifold edges deterministically")


def remove_isolated_fragments(
    mesh: Mesh,
    *,
    min_faces: int = 3,
    face_provenance: np.ndarray | None = None,
) -> FragmentResult:
    if min_faces < 1:
        raise BusinessError(
            E_MESH_INVALID,
            "min_fragment_faces must be at least one",
            subcode=SUB_INVALID_PARAMETER,
            location={"field": "min_fragment_faces"},
        )
    if face_provenance is None:
        face_provenance = np.arange(mesh.face_count, dtype=np.int64)
    provenance = np.asarray(face_provenance, dtype=np.int64)
    topology = build_topology(mesh)
    components = topology.face_components()
    keep_mask = np.ones(mesh.face_count, dtype=bool)
    removed_components: list[dict[str, Any]] = []
    removed_face_ids: list[int] = []
    for component in components:
        if len(component) >= min_faces:
            continue
        face_ids = np.asarray(component, dtype=np.int64)
        keep_mask[face_ids] = False
        points = mesh.vertices[mesh.faces[face_ids].reshape(-1)]
        removed = {
            "face_count": len(component),
            "face_ids": component,
            "bbox_min": [float(value) for value in points.min(axis=0)],
            "bbox_max": [float(value) for value in points.max(axis=0)],
        }
        removed_components.append(removed)
        removed_face_ids.extend(component)
    if not removed_components:
        return FragmentResult(
            mesh=mesh.copy(),
            face_provenance=provenance.copy(),
            removed_component_count=0,
            removed_face_count=0,
            removed_vertex_count=0,
            components=[],
            kept_face_ids=np.arange(mesh.face_count, dtype=np.int64),
        )

    filtered = Mesh(mesh.vertices, mesh.faces[keep_mask])
    compacted, vertex_map = filtered.compact()
    filtered_provenance = provenance[keep_mask]
    removed_vertex_count = int(np.count_nonzero(vertex_map < 0))
    return FragmentResult(
        mesh=compacted,
        face_provenance=filtered_provenance,
        removed_component_count=len(removed_components),
        removed_face_count=len(removed_face_ids),
        removed_vertex_count=removed_vertex_count,
        components=removed_components,
        kept_face_ids=np.flatnonzero(keep_mask).astype(np.int64),
    )


def compute_normals(mesh: Mesh) -> NormalResult:
    triangles = mesh.vertices[mesh.faces] if mesh.face_count else np.empty((0, 3, 3))
    if mesh.face_count:
        cross = np.cross(triangles[:, 1] - triangles[:, 0], triangles[:, 2] - triangles[:, 0])
        areas = np.linalg.norm(cross, axis=1)
        face_normals = np.zeros_like(cross)
        nonzero = areas > 1e-15
        face_normals[nonzero] = cross[nonzero] / areas[nonzero, None]
    else:
        areas = np.empty(0, dtype=np.float64)
        face_normals = np.empty((0, 3), dtype=np.float64)

    vertex_sum = np.zeros((mesh.vertex_count, 3), dtype=np.float64)
    if mesh.face_count:
        for face_id, face in enumerate(mesh.faces):
            vertex_sum[face] += cross[face_id]
    lengths = np.linalg.norm(vertex_sum, axis=1)
    vertex_normals = np.zeros_like(vertex_sum)
    nonzero = lengths > 1e-15
    vertex_normals[nonzero] = vertex_sum[nonzero] / lengths[nonzero, None]
    return NormalResult(face_normals, areas, vertex_normals)


def nonmanifold_edge_count(mesh: Mesh) -> int:
    return len(build_topology(mesh).nonmanifold_edges)
