from __future__ import annotations

import heapq
from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any

from .model import InvalidationReport, TerrainProject
from .util import canonical_json_bytes, sha256_bytes

DEPENDENCY_ROLES = ("seam_normal", "seam_material")


@dataclass(frozen=True)
class DependencyEdge:
    source: str
    target: str
    direction: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "from": self.source,
            "to": self.target,
            "direction": self.direction,
            "roles": list(DEPENDENCY_ROLES),
        }


class DependencyGraph:
    """Acyclic seam dependency graph.

    A tile owns its east and south seam samples and therefore depends on those
    neighbors. Edges point from the source tile to its consumer. East/south was
    chosen to make the graph deterministic and acyclic; west/north edges are
    the mirror side of the same undirected seam relation.
    """

    def __init__(self, project: TerrainProject) -> None:
        self.project = project
        self.tile_ids = tuple(project.tiles.keys())
        self.dependencies: dict[str, tuple[str, ...]] = {}
        self.dependents: dict[str, tuple[str, ...]] = {tile_id: () for tile_id in self.tile_ids}
        edges: list[DependencyEdge] = []
        dependent_lists: dict[str, list[str]] = {tile_id: [] for tile_id in self.tile_ids}
        for tile in project.tiles.values():
            deps: list[str] = []
            neighbor_map = project.neighbors(tile)
            for direction in ("east", "south"):
                neighbor = neighbor_map.get(direction)
                if neighbor is None:
                    continue
                deps.append(neighbor.tile_id)
                dependent_lists[neighbor.tile_id].append(tile.tile_id)
                edges.append(
                    DependencyEdge(
                        source=neighbor.tile_id,
                        target=tile.tile_id,
                        direction=direction,
                    )
                )
            self.dependencies[tile.tile_id] = tuple(deps)
        for tile_id, values in dependent_lists.items():
            self.dependents[tile_id] = tuple(sorted(values))
        self.edges = tuple(sorted(edges, key=lambda edge: (edge.source, edge.target)))
        self._topological_order = self._compute_topological_order()
        self._topological_index = {
            tile_id: index for index, tile_id in enumerate(self._topological_order)
        }
        self._levels = self._compute_levels()

    def _compute_topological_order(self) -> tuple[str, ...]:
        indegree = {tile_id: len(self.dependencies[tile_id]) for tile_id in self.tile_ids}
        ready = [tile_id for tile_id, degree in indegree.items() if degree == 0]
        heapq.heapify(ready)
        result: list[str] = []
        while ready:
            tile_id = heapq.heappop(ready)
            result.append(tile_id)
            for dependent in self.dependents[tile_id]:
                indegree[dependent] -= 1
                if indegree[dependent] == 0:
                    heapq.heappush(ready, dependent)
        if len(result) != len(self.tile_ids):
            raise RuntimeError("terrain dependency graph contains a cycle")
        return tuple(result)

    def _compute_levels(self) -> dict[str, int]:
        levels: dict[str, int] = {}
        for tile_id in self._topological_order:
            deps = self.dependencies[tile_id]
            levels[tile_id] = 0 if not deps else 1 + max(levels[dep] for dep in deps)
        return levels

    @property
    def topological_order(self) -> tuple[str, ...]:
        return self._topological_order

    def level(self, tile_id: str) -> int:
        return self._levels[tile_id]

    def wave_order(self, tile_ids: Iterable[str]) -> list[list[str]]:
        selected = set(tile_ids)
        grouped: dict[int, list[str]] = {}
        for tile_id in selected:
            grouped.setdefault(self._levels[tile_id], []).append(tile_id)
        return [sorted(grouped[level]) for level in sorted(grouped)]

    def dependency_payload(self, tile_id: str) -> list[dict[str, Any]]:
        tile = self.project.tiles[tile_id]
        neighbor_map = self.project.neighbors(tile)
        result: list[dict[str, Any]] = []
        for direction in ("east", "south"):
            neighbor = neighbor_map.get(direction)
            if neighbor is not None:
                result.append(
                    {
                        "tile_id": neighbor.tile_id,
                        "direction": direction,
                        "roles": list(DEPENDENCY_ROLES),
                        "content_hash": neighbor.content_hash,
                    }
                )
        return result

    def invalidate(
        self,
        changed_tiles: Iterable[str],
        *,
        trigger: list[dict[str, Any]],
        mode: str,
        startup_reason: str | None = None,
        force_all: bool = False,
    ) -> InvalidationReport:
        changed = sorted(set(changed_tiles))
        for tile_id in changed:
            if tile_id not in self.dependencies:
                raise KeyError(tile_id)
        if force_all:
            affected_set = set(self.tile_ids)
            reasons = {
                tile_id: [
                    {
                        "source": "full_rebuild",
                        "from": "",
                        "edge": "all",
                    }
                ]
                for tile_id in self.tile_ids
            }
        else:
            affected_set = set(changed)
            reasons: dict[str, list[dict[str, str]]] = {
                tile_id: [{"source": tile_id, "from": "", "edge": "self"}] for tile_id in changed
            }
            queue: list[tuple[str, str, str]] = [(tile_id, tile_id, "self") for tile_id in changed]
            while queue:
                source, current, parent_edge = queue.pop(0)
                for dependent in self.dependents.get(current, ()):
                    record = {
                        "source": source,
                        "from": current,
                        "edge": parent_edge
                        if parent_edge != "self"
                        else "seam_normal+seam_material",
                    }
                    if dependent not in affected_set:
                        affected_set.add(dependent)
                        reasons[dependent] = []
                        queue.append((source, dependent, "seam_normal+seam_material"))
                    if record not in reasons[dependent]:
                        reasons[dependent].append(record)
            for tile_id in affected_set:
                for item in reasons[tile_id]:
                    item["from"] = item.get("from", "")
                    item["edge"] = item.get("edge", "")

        affected = sorted(affected_set, key=lambda tile_id: self._topological_index[tile_id])
        reused = [tile_id for tile_id in self._topological_order if tile_id not in affected_set]
        for tile_id in changed:
            reasons.setdefault(tile_id, [])
        return InvalidationReport(
            trigger=list(trigger),
            affected=affected,
            reused=reused,
            changed=changed,
            graph_edges=[edge.to_dict() for edge in self.edges],
            reasons={
                tile_id: sorted(
                    reasons.get(tile_id, []),
                    key=lambda r: (r["source"], r["from"], r["edge"]),
                )
                for tile_id in affected
            },
            topological_order=list(self._topological_order),
            mode=mode,
            startup_reason=startup_reason,
        )


class CacheKeys:
    """Recursive content/dependency hashes used as cache keys."""

    def __init__(self, project: TerrainProject, graph: DependencyGraph, bake_version: str) -> None:
        self.project = project
        self.graph = graph
        self.bake_version = bake_version
        self.keys: dict[str, str] = {}
        self.dependency_maps: dict[str, tuple[dict[str, Any], ...]] = {}
        self._compute()

    def _compute(self) -> None:
        for tile_id in self.graph.topological_order:
            dependencies = self.graph.dependency_payload(tile_id)
            for dependency in dependencies:
                dependency["cache_key"] = self.keys[dependency["tile_id"]]
            payload = {
                "format": "terrain-block-cache-key/1",
                "tile_id": tile_id,
                "content_hash": self.project.tiles[tile_id].content_hash,
                "bake_version": self.bake_version,
                "dependencies": dependencies,
            }
            digest = sha256_bytes(canonical_json_bytes(payload))
            self.keys[tile_id] = digest
            self.dependency_maps[tile_id] = tuple(dependencies)

    def expected_map(self) -> dict[str, str]:
        return dict(sorted(self.keys.items()))
