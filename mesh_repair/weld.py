from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import BusinessError, E_MESH_INVALID, SUB_INVALID_PARAMETER
from .mesh import Mesh


@dataclass
class WeldResult:
    mesh: Mesh
    tolerance: float
    old_to_new: np.ndarray
    groups: list[list[int]]
    dropped_faces: list[int]

    @property
    def duplicate_count(self) -> int:
        return int(sum(max(0, len(group) - 1) for group in self.groups))

    def summary(self) -> dict[str, Any]:
        return {
            "tolerance": self.tolerance,
            "input_vertex_count": int(self.old_to_new.size),
            "output_vertex_count": self.mesh.vertex_count,
            "duplicate_count": self.duplicate_count,
            "merged_group_count": sum(1 for group in self.groups if len(group) > 1),
            "dropped_degenerate_faces": list(self.dropped_faces),
        }


class _UnionFind:
    def __init__(self, size: int) -> None:
        self.parent = list(range(size))
        self.rank = [0] * size

    def find(self, value: int) -> int:
        parent = self.parent
        while parent[value] != value:
            parent[value] = parent[parent[value]]
            value = parent[value]
        return value

    def union(self, left: int, right: int) -> None:
        a, b = self.find(left), self.find(right)
        if a == b:
            return
        if self.rank[a] < self.rank[b]:
            a, b = b, a
        self.parent[b] = a
        if self.rank[a] == self.rank[b]:
            self.rank[a] += 1


def validate_tolerance(tolerance: float) -> float:
    try:
        value = float(tolerance)
    except (TypeError, ValueError) as exc:
        raise BusinessError(
            E_MESH_INVALID,
            "weld tolerance must be a finite non-negative number",
            subcode=SUB_INVALID_PARAMETER,
            location={"field": "tolerance"},
        ) from exc
    if not math.isfinite(value) or value < 0:
        raise BusinessError(
            E_MESH_INVALID,
            "weld tolerance must be a finite non-negative number",
            subcode=SUB_INVALID_PARAMETER,
            location={"field": "tolerance"},
            context={"value": tolerance},
        )
    return value


def weld_vertices(mesh: Mesh, tolerance: float = 1e-6) -> WeldResult:
    """Merge coincident vertices with deterministic spatial hashing.

    Connected components are built first, then representatives are selected by
    the lowest original vertex index. This avoids order-dependent transitive
    welding and makes block-boundary merge maps reproducible.
    """

    tolerance = validate_tolerance(tolerance)
    count = mesh.vertex_count
    if count == 0:
        return WeldResult(mesh.copy(), tolerance, np.empty(0, np.int64), [], [])

    uf = _UnionFind(count)
    if tolerance == 0.0:
        exact: dict[bytes, int] = {}
        normalized = mesh.vertices.copy()
        normalized[normalized == 0.0] = 0.0
        for vertex_id, row in enumerate(normalized):
            key = row.tobytes()
            previous = exact.get(key)
            if previous is None:
                exact[key] = vertex_id
            else:
                uf.union(previous, vertex_id)
    else:
        cells: dict[tuple[int, int, int], list[int]] = {}
        for vertex_id, point in enumerate(mesh.vertices):
            cell = tuple(np.floor(point / tolerance).astype(np.int64).tolist())
            for dx in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    for dz in (-1, 0, 1):
                        candidates = cells.get((cell[0] + dx, cell[1] + dy, cell[2] + dz))
                        if not candidates:
                            continue
                        for candidate in candidates:
                            if float(np.linalg.norm(mesh.vertices[candidate] - point)) <= tolerance:
                                uf.union(candidate, vertex_id)
            cells.setdefault(cell, []).append(vertex_id)

    grouped: dict[int, list[int]] = {}
    for vertex_id in range(count):
        grouped.setdefault(uf.find(vertex_id), []).append(vertex_id)
    groups = sorted(grouped.values(), key=lambda group: group[0])

    old_to_new = np.full(count, -1, dtype=np.int64)
    new_vertices = np.empty((len(groups), 3), dtype=np.float64)
    for new_id, group in enumerate(groups):
        representative = min(group)
        old_to_new[group] = new_id
        new_vertices[new_id] = mesh.vertices[representative]

    remapped = old_to_new[mesh.faces]
    repeated = (
        (remapped[:, 0] == remapped[:, 1])
        | (remapped[:, 1] == remapped[:, 2])
        | (remapped[:, 2] == remapped[:, 0])
    )
    dropped_faces = np.flatnonzero(repeated).astype(int).tolist()
    keep = ~repeated
    welded = Mesh(new_vertices, remapped[keep])
    return WeldResult(
        mesh=welded,
        tolerance=tolerance,
        old_to_new=old_to_new,
        groups=groups,
        dropped_faces=dropped_faces,
    )


def make_cross_block_weld_map(
    *,
    block_id: int,
    raw_vertex_ids: list[int] | np.ndarray,
    old_to_new: np.ndarray,
    blocks_of_vertex: dict[int, set[int]],
) -> dict[str, Any]:
    """Describe a block's canonical vertex map and cross-block merge pairs."""

    raw_ids = sorted(int(value) for value in np.asarray(raw_vertex_ids).tolist())
    local_to_global = [
        {
            "local": local,
            "source_vertex": raw_id,
            "welded_vertex": int(old_to_new[raw_id]),
        }
        for local, raw_id in enumerate(raw_ids)
    ]
    by_canonical: dict[int, list[int]] = {}
    for raw_id in raw_ids:
        by_canonical.setdefault(int(old_to_new[raw_id]), []).append(raw_id)
    cross_boundary: list[dict[str, Any]] = []
    for welded, members in sorted(by_canonical.items()):
        other_blocks = sorted(
            {
                peer
                for member in members
                for peer in blocks_of_vertex.get(member, {block_id})
                if peer != block_id
            }
        )
        if other_blocks:
            cross_boundary.append(
                {
                    "welded_vertex": welded,
                    "local_vertices": members,
                    "peer_blocks": other_blocks,
                }
            )
    return {
        "block_id": block_id,
        "local_to_global": local_to_global,
        "cross_boundary": cross_boundary,
        "map_hash": _map_hash(local_to_global),
    }


def _map_hash(mapping: list[dict[str, int]]) -> str:
    import hashlib
    import json

    canonical = json.dumps(mapping, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()
