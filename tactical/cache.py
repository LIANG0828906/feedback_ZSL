"""Dependency-aware path cache entries."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

from .hashing import path_hash

CacheKey = tuple[int, tuple[int, int], tuple[int, int]]


@dataclass(frozen=True)
class PathCacheEntry:
    unit_id: int
    target: tuple[int, int]
    source: tuple[int, int]
    version: int
    path: tuple[tuple[int, int], ...]
    path_hash: str
    dependencies: frozenset[int]
    cost: int
    reachable: bool

    @property
    def key(self) -> CacheKey:
        return (self.unit_id, self.target, self.source)

    @classmethod
    def create(
        cls,
        unit_id: int,
        target: tuple[int, int],
        source: tuple[int, int],
        version: int,
        path: Iterable[tuple[int, int]],
        dependencies: Iterable[int],
        cost: int,
        reachable: bool,
    ) -> PathCacheEntry:
        points = tuple((int(row), int(col)) for row, col in path)
        return cls(
            int(unit_id),
            (int(target[0]), int(target[1])),
            (int(source[0]), int(source[1])),
            int(version),
            points,
            path_hash(points),
            frozenset(int(index) for index in dependencies),
            int(cost),
            bool(reachable),
        )

    def is_invalidated(self, current_version: int, affected: set[int]) -> bool:
        if self.version != current_version:
            return True
        if not self.reachable:
            return bool(affected)
        return not self.dependencies.isdisjoint(affected)


def path_index_dependencies(
    path: Iterable[tuple[int, int]], height: int, width: int
) -> frozenset[int]:
    """Include each path cell and all local alternatives used by tie-breaking."""

    dependencies: set[int] = set()
    for row, col in path:
        row = int(row)
        col = int(col)
        dependencies.add(row * int(width) + col)
        for delta_row, delta_col in ((-1, 0), (0, -1), (0, 1), (1, 0)):
            next_row = row + delta_row
            next_col = col + delta_col
            if 0 <= next_row < int(height) and 0 <= next_col < int(width):
                dependencies.add(next_row * int(width) + next_col)
    return frozenset(dependencies)
