from __future__ import annotations

import hashlib
import json
from collections import defaultdict
from dataclasses import dataclass
from typing import Any

import numpy as np

from .mesh import Mesh
from .topology import Topology, build_topology
from .weld import weld_vertices


@dataclass(frozen=True)
class BoundaryLoop:
    loop_id: int
    vertices: tuple[int, ...]
    bbox_min: tuple[float, float, float]
    bbox_max: tuple[float, float, float]

    @property
    def vertex_count(self) -> int:
        return len(self.vertices)

    def to_dict(self) -> dict[str, Any]:
        return {
            "loop_id": self.loop_id,
            "vertex_count": self.vertex_count,
            "vertices": list(self.vertices),
            "bbox_min": list(self.bbox_min),
            "bbox_max": list(self.bbox_max),
        }


@dataclass
class BoundaryResult:
    loops: list[BoundaryLoop]
    open_chains: list[tuple[int, ...]]
    branched_vertices: list[int]

    @property
    def is_closed(self) -> bool:
        return not self.open_chains and not self.branched_vertices

    def to_dict(self) -> dict[str, Any]:
        return {
            "loop_count": len(self.loops),
            "loops": [loop.to_dict() for loop in self.loops],
            "open_chain_count": len(self.open_chains),
            "open_chains": [list(chain) for chain in self.open_chains],
            "branched_vertices": list(self.branched_vertices),
            "closed": self.is_closed,
        }


def extract_boundary_loops(mesh: Mesh, topology: Topology | None = None) -> BoundaryResult:
    """Extract directed boundary cycles from one-incidence edges.

    A malformed fan with more than one outgoing boundary edge at a vertex is
    reported as a branched vertex and its walk is emitted as an open chain.
    """

    topology = topology or build_topology(mesh)
    outgoing: dict[int, list[tuple[int, int]]] = {}
    boundary_directed: list[tuple[int, int]] = []
    for incidence in topology.boundary_edges:
        if incidence.directed:
            u, v = incidence.directed[0]
        else:
            u, v = incidence.edge
        edge_key = (min(u, v), max(u, v))
        boundary_directed.append(edge_key)
        outgoing.setdefault(u, []).append((v, edge_key))
    for starts in outgoing.values():
        starts.sort()

    branched = sorted(vertex for vertex, starts in outgoing.items() if len(starts) > 1)
    unused = set(boundary_directed)
    loops: list[BoundaryLoop] = []
    open_chains: list[tuple[int, ...]] = []

    def remove_edge(u: int, v: int) -> bool:
        key = (min(u, v), max(u, v))
        if key not in unused:
            return False
        unused.remove(key)
        return True

    for directed_u, directed_v in _directed_boundary_edges(topology):
        if (min(directed_u, directed_v), max(directed_u, directed_v)) not in unused:
            continue
        chain = [directed_u]
        current = directed_v
        remove_edge(directed_u, directed_v)
        while current != chain[0]:
            chain.append(current)
            candidates = [
                (nxt, edge_key)
                for nxt, edge_key in outgoing.get(current, [])
                if edge_key in unused
                and (min(current, nxt), max(current, nxt)) in unused
            ]
            if not candidates:
                break
            nxt = candidates[0][0]
            remove_edge(current, nxt)
            current = nxt
            if len(chain) > mesh.vertex_count + 1:
                break
        if current == chain[0] and len(chain) >= 3:
            vertices = tuple(chain)
            points = mesh.vertices[np.asarray(vertices, dtype=np.int64)]
            loops.append(
                BoundaryLoop(
                    loop_id=len(loops),
                    vertices=vertices,
                    bbox_min=tuple(float(value) for value in points.min(axis=0)),
                    bbox_max=tuple(float(value) for value in points.max(axis=0)),
                )
            )
        else:
            if current != chain[0] and chain[-1] != current:
                chain.append(current)
            open_chains.append(tuple(chain))

    loops.sort(key=lambda loop: (loop.vertex_count, loop.bbox_min, loop.vertices))
    for loop_id, loop in enumerate(loops):
        if loop.loop_id != loop_id:
            loops[loop_id] = BoundaryLoop(
                loop_id, loop.vertices, loop.bbox_min, loop.bbox_max
            )
    return BoundaryResult(loops=loops, open_chains=open_chains, branched_vertices=branched)


def _directed_boundary_edges(topology: Topology) -> list[tuple[int, int]]:
    directed: list[tuple[int, int]] = []
    seen: set[tuple[int, int]] = set()
    for incidence in topology.boundary_edges:
        edge = incidence.edge
        if edge in seen:
            continue
        seen.add(edge)
        if incidence.directed:
            directed.append(incidence.directed[0])
        else:
            directed.append(edge)
    return directed


def boundary_report(mesh: Mesh, topology: Topology | None = None) -> dict[str, Any]:
    topology = topology or build_topology(mesh)
    result = extract_boundary_loops(mesh, topology)
    return {
        "boundary_edge_count": len(topology.boundary_edges),
        "nonmanifold_edge_count": len(topology.nonmanifold_edges),
        **result.to_dict(),
    }


def _loop_signature(mesh: Mesh, loop: BoundaryLoop, rounding: int = 6) -> tuple[Any, ...]:
    points = np.round(mesh.vertices[np.asarray(loop.vertices)], rounding)
    canonical = sorted(tuple(float(v) for v in point) for point in points)
    digest = hashlib.sha256(
        json.dumps(canonical, separators=(",", ":")).encode("utf-8")
    ).hexdigest()
    return (loop.vertex_count, digest)


def compare_boundary_tolerances(
    mesh: Mesh,
    old_tolerance: float,
    new_tolerance: float,
    *,
    rounding: int = 6,
) -> dict[str, Any]:
    """Re-weld at both tolerances and summarize boundary-loop changes."""

    old_weld = weld_vertices(mesh, old_tolerance)
    new_weld = weld_vertices(mesh, new_tolerance)
    old_boundary = extract_boundary_loops(old_weld.mesh)
    new_boundary = extract_boundary_loops(new_weld.mesh)
    old_keys = [_loop_signature(old_weld.mesh, loop, rounding) for loop in old_boundary.loops]
    new_keys = [_loop_signature(new_weld.mesh, loop, rounding) for loop in new_boundary.loops]
    old_set, new_set = set(old_keys), set(new_keys)
    new_by_bbox: dict[tuple[Any, ...], list[int]] = defaultdict(list)
    for index, loop in enumerate(new_boundary.loops):
        bbox_key = (
            tuple(round(value, rounding) for value in loop.bbox_min),
            tuple(round(value, rounding) for value in loop.bbox_max),
        )
        new_by_bbox[bbox_key].append(index)
    changed: list[dict[str, Any]] = []
    matched_count = 0
    for old_index, loop in enumerate(old_boundary.loops):
        bbox_key = (
            tuple(round(value, rounding) for value in loop.bbox_min),
            tuple(round(value, rounding) for value in loop.bbox_max),
        )
        candidates = new_by_bbox.get(bbox_key)
        if not candidates:
            continue
        new_index = candidates.pop(0)
        matched_count += 1
        new_loop = new_boundary.loops[new_index]
        if loop.vertex_count != new_loop.vertex_count:
            changed.append(
                {
                    "old_loop": loop.to_dict(),
                    "new_loop": new_loop.to_dict(),
                    "vertex_count_delta": new_loop.vertex_count - loop.vertex_count,
                }
            )
    changed.sort(key=lambda item: item["old_loop"]["loop_id"])
    return {
        "old_tolerance": float(old_tolerance),
        "new_tolerance": float(new_tolerance),
        "old_vertex_count": old_weld.mesh.vertex_count,
        "new_vertex_count": new_weld.mesh.vertex_count,
        "old_loop_count": len(old_boundary.loops),
        "new_loop_count": len(new_boundary.loops),
        "loop_count_delta": len(new_boundary.loops) - len(old_boundary.loops),
        "removed_loop_signatures": len(old_set - new_set),
        "added_loop_signatures": len(new_set - old_set),
        "matched_loop_count": matched_count,
        "changed_loops": changed,
        "old_loops": [loop.to_dict() for loop in old_boundary.loops],
        "new_loops": [loop.to_dict() for loop in new_boundary.loops],
    }
