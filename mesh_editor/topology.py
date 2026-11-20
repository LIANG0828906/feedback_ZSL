from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Callable

import numpy as np

from .model import Face, Mesh, compact_mesh, compute_vertex_normals


@dataclass
class WeldResult:
    mesh: Mesh
    tolerance: float
    old_to_new: tuple[int, ...]
    merged_vertices: int
    affected_faces: tuple[int, ...]
    removed_faces: tuple[int, ...]
    topology: "HalfEdgeTopology"

    def mapping_dict(self) -> dict[str, int]:
        return {str(old): new for old, new in enumerate(self.old_to_new)}

    def report(self) -> dict[str, object]:
        return {
            "tolerance": self.tolerance,
            "input_vertices": len(self.old_to_new),
            "output_vertices": int(len(self.mesh.vertices)),
            "merged_vertices": int(self.merged_vertices),
            "affected_faces": list(self.affected_faces),
            "removed_faces": list(self.removed_faces),
            "old_to_new": self.mapping_dict(),
            "non_manifold_edges": self.topology.non_manifold_edge_count,
            "boundary_loops": len(self.topology.boundary_loops),
        }


@dataclass(frozen=True)
class HalfEdge:
    index: int
    origin: int
    destination: int
    face: int
    next: int
    twin: int = -1


@dataclass
class HalfEdgeTopology:
    halfedges: list[HalfEdge]
    edge_to_halfedges: dict[tuple[int, int], list[int]]
    face_adjacency: list[set[int]]
    boundary_loops: list[tuple[int, ...]]
    boundary_chains: list[tuple[int, ...]]
    non_manifold_edges: list[tuple[int, int]]

    @property
    def non_manifold_edge_count(self) -> int:
        return len(self.non_manifold_edges)

    @property
    def boundary_edge_count(self) -> int:
        return sum(1 for he in self.halfedges if he.twin < 0)

    def summary(self) -> dict[str, object]:
        return {
            "halfedges": len(self.halfedges),
            "edges": len(self.edge_to_halfedges),
            "boundary_edges": self.boundary_edge_count,
            "boundary_loops": [list(loop) for loop in self.boundary_loops],
            "boundary_chains": [list(chain) for chain in self.boundary_chains],
            "non_manifold_edges": [list(edge) for edge in self.non_manifold_edges],
        }


def _cell(point: np.ndarray, tolerance: float) -> tuple[int, int, int]:
    return (
        int(math.floor(float(point[0]) / tolerance)),
        int(math.floor(float(point[1]) / tolerance)),
        int(math.floor(float(point[2]) / tolerance)),
    )


def weld_vertices(
    mesh: Mesh,
    tolerance: float,
    *,
    progress: Callable[[float], None] | None = None,
    cancelled: Callable[[], bool] | None = None,
) -> WeldResult:
    if not math.isfinite(tolerance) or tolerance < 0.0:
        raise ValueError("weld tolerance must be a finite, non-negative number")
    positions = np.asarray(mesh.vertices, dtype=np.float64)
    old_to_new = [-1] * len(positions)
    representatives: list[int] = []
    buckets: dict[tuple[int, int, int], list[int]] = {}
    tol2 = tolerance * tolerance

    for old_index, point in enumerate(positions):
        if cancelled is not None and old_index % 2048 == 0 and cancelled():
            from .errors import EditCancelled

            raise EditCancelled()
        match = -1
        if tolerance == 0.0:
            for rep in reversed(buckets.get(_cell(point, 1.0), [])):
                if np.array_equal(positions[rep], point):
                    match = rep
                    break
        else:
            cell = _cell(point, tolerance)
            candidates: list[int] = []
            for dx in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    for dz in (-1, 0, 1):
                        candidates.extend(buckets.get((cell[0] + dx, cell[1] + dy, cell[2] + dz), ()))
            for rep in sorted(candidates):
                delta = positions[rep] - point
                if float(np.dot(delta, delta)) <= tol2 + 1e-30:
                    match = rep
                    break
        if match >= 0:
            old_to_new[old_index] = old_to_new[match]
        else:
            new_index = len(representatives)
            representatives.append(old_index)
            old_to_new[old_index] = new_index
            buckets.setdefault(_cell(point, tolerance if tolerance > 0.0 else 1.0), []).append(old_index)
        if progress is not None and (old_index & 1023) == 0:
            progress(0.7 * (old_index + 1) / max(1, len(positions)))

    representatives_array = np.asarray(representatives, dtype=np.int64)
    new_vertices = (
        positions[representatives_array] if len(representatives_array) else np.empty((0, 3), dtype=np.float64)
    )
    new_faces: list[Face] = []
    affected: list[int] = []
    removed: list[int] = []
    for face_id, face in enumerate(mesh.faces):
        mapped = tuple(old_to_new[i] for i in face.vertices)
        cleaned: list[int] = []
        for index in mapped:
            if not cleaned or cleaned[-1] != index:
                cleaned.append(index)
        if len(cleaned) > 1 and cleaned[0] == cleaned[-1]:
            cleaned.pop()
        if len(cleaned) < 3 or len(set(cleaned)) < 3:
            removed.append(face_id)
            affected.append(face_id)
            continue
        if tuple(cleaned) != face.vertices:
            affected.append(face_id)
        new_faces.append(Face(tuple(cleaned), face.texcoords, None, face.group))
        if progress is not None and (face_id & 511) == 0:
            progress(0.7 + 0.2 * (face_id + 1) / max(1, len(mesh.faces)))

    welded = Mesh(
        new_vertices,
        new_faces,
        list(mesh.texcoords) if mesh.texcoords is not None else None,
        None,
        {**mesh.metadata, "weld_tolerance": float(tolerance)},
    )
    welded.normals = compute_vertex_normals(welded)
    topology = build_halfedge_topology(welded)
    if progress is not None:
        progress(1.0)
    return WeldResult(
        mesh=welded,
        tolerance=float(tolerance),
        old_to_new=tuple(old_to_new),
        merged_vertices=int(len(positions) - len(new_vertices)),
        affected_faces=tuple(sorted(set(affected))),
        removed_faces=tuple(removed),
        topology=topology,
    )


def build_halfedge_topology(mesh: Mesh) -> HalfEdgeTopology:
    mutable: list[dict[str, int]] = []
    edge_to_halfedges: dict[tuple[int, int], list[int]] = {}
    face_adjacency: list[set[int]] = [set() for _ in mesh.faces]
    for face_id, face in enumerate(mesh.faces):
        ring = face.vertices
        start = len(mutable)
        for i, origin in enumerate(ring):
            destination = ring[(i + 1) % len(ring)]
            entry = {
                "index": start + i,
                "origin": int(origin),
                "destination": int(destination),
                "face": face_id,
                "next": start + ((i + 1) % len(ring)),
                "twin": -1,
            }
            mutable.append(entry)
            edge = (min(int(origin), int(destination)), max(int(origin), int(destination)))
            edge_to_halfedges.setdefault(edge, []).append(entry["index"])

    non_manifold: list[tuple[int, int]] = []
    for edge in sorted(edge_to_halfedges):
        he_ids = edge_to_halfedges[edge]
        if len(he_ids) == 2:
            a = mutable[he_ids[0]]
            b = mutable[he_ids[1]]
            if a["origin"] == b["destination"] and a["destination"] == b["origin"]:
                a["twin"] = b["index"]
                b["twin"] = a["index"]
                face_adjacency[a["face"]].add(b["face"])
                face_adjacency[b["face"]].add(a["face"])
        elif len(he_ids) > 2:
            non_manifold.append(edge)
            for i in range(len(he_ids)):
                for j in range(i + 1, len(he_ids)):
                    a = mutable[he_ids[i]]
                    b = mutable[he_ids[j]]
                    face_adjacency[a["face"]].add(b["face"])
                    face_adjacency[b["face"]].add(a["face"])

    halfedges = [HalfEdge(**entry) for entry in mutable]
    boundary = [he for he in halfedges if he.twin < 0]
    outgoing: dict[int, list[int]] = {}
    for he in boundary:
        outgoing.setdefault(he.origin, []).append(he.index)
    for values in outgoing.values():
        values.sort()

    visited: set[int] = set()
    loops: list[tuple[int, ...]] = []
    chains: list[tuple[int, ...]] = []
    for start in sorted(outgoing):
        for he_id in outgoing[start]:
            if he_id in visited:
                continue
            chain: list[int] = []
            current = he_id
            closed = False
            while current not in visited:
                visited.add(current)
                chain.append(current)
                destination = halfedges[current].destination
                candidates = [idx for idx in outgoing.get(destination, ()) if idx not in visited]
                if candidates:
                    current = candidates[0]
                    continue
                if destination == start:
                    closed = True
                break
            if closed and len(chain) >= 3:
                loops.append(tuple(chain))
            else:
                chains.append(tuple(chain))
    return HalfEdgeTopology(
        halfedges=halfedges,
        edge_to_halfedges=edge_to_halfedges,
        face_adjacency=face_adjacency,
        boundary_loops=sorted(loops),
        boundary_chains=sorted(chains),
        non_manifold_edges=non_manifold,
    )
