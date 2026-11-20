"""Deterministic reverse shortest-path fields and path reconstruction."""

from __future__ import annotations

import heapq
from collections import deque
from collections.abc import Iterable
from dataclasses import dataclass

import numpy as np

from .grid import INF
from .hashing import canonical_json_bytes, path_bytes, path_hash, sha256_bytes

NEIGHBOR_OFFSETS: tuple[tuple[int, int], ...] = (
    (-1, 0),
    (0, -1),
    (0, 1),
    (1, 0),
)


@dataclass(frozen=True)
class CostField:
    unit_id: int
    target: tuple[int, int]
    distance: np.ndarray
    field_hash: str

    @classmethod
    def create(
        cls, unit_id: int, target: tuple[int, int], distance: np.ndarray
    ) -> CostField:
        array = np.asarray(distance, dtype=np.int64).copy()
        array.flags.writeable = False
        digest = cost_field_hash(unit_id, target, array)
        return cls(int(unit_id), (int(target[0]), int(target[1])), array, digest)

    @property
    def generation_cells(self) -> int:
        return int(np.count_nonzero(self.distance != INF))

    def at(self, row: int, col: int) -> int:
        value = int(self.distance[row, col])
        return -1 if value == int(INF) else value

    def array_nbytes(self) -> int:
        return int(self.distance.nbytes)


def cost_field_hash(unit_id: int, target: tuple[int, int], distance: np.ndarray) -> str:
    array = np.asarray(distance, dtype=np.int64)
    header = canonical_json_bytes(
        {
            "format": "cost-field-v1",
            "unit": int(unit_id),
            "target": [int(target[0]), int(target[1])],
            "shape": [int(array.shape[0]), int(array.shape[1])],
            "dtype": "<i8",
        }
    )
    return sha256_bytes(header + array.astype("<i8", copy=False).tobytes(order="C"))


def compute_full_field(weights: np.ndarray, target: tuple[int, int]) -> np.ndarray:
    """Compute a reverse field with deterministic ``(distance, row, col)`` order."""

    costs = np.asarray(weights, dtype=np.int64)
    if costs.ndim != 2:
        raise ValueError("weights must be two-dimensional")
    height, width = costs.shape
    row, col = int(target[0]), int(target[1])
    if not (0 <= row < height and 0 <= col < width):
        raise ValueError("target outside grid")
    if int(costs[row, col]) == int(INF):
        raise ValueError("target cell is impassable")

    distance = np.full((height, width), INF, dtype=np.int64)
    distance[row, col] = 0
    heap: list[tuple[int, int, int]] = [(0, row, col)]
    while heap:
        current_distance, current_row, current_col = heapq.heappop(heap)
        if current_distance != int(distance[current_row, current_col]):
            continue
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = current_row + delta_row
            next_col = current_col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            cell_weight = int(costs[next_row, next_col])
            if cell_weight == int(INF):
                continue
            candidate = current_distance + cell_weight
            if candidate < int(distance[next_row, next_col]):
                distance[next_row, next_col] = np.int64(candidate)
                heapq.heappush(heap, (candidate, next_row, next_col))
    return distance


def make_cost_field(
    unit_id: int, target: tuple[int, int], weights: np.ndarray
) -> CostField:
    distance = compute_full_field(weights, target)
    return CostField.create(unit_id, target, distance)


def incremental_update(
    old_field: CostField,
    old_weights: np.ndarray,
    new_weights: np.ndarray,
) -> tuple[CostField, np.ndarray, np.ndarray]:
    """Update a field under node-weight changes.

    Returns ``(new_field, actual_changed_mask, local_weight_changed_mask)``.
    Only cells reached by the reverse label-correction worklist are evaluated.
    """

    old_costs = np.asarray(old_weights, dtype=np.int64)
    new_costs = np.asarray(new_weights, dtype=np.int64)
    if (
        old_costs.shape != old_field.distance.shape
        or new_costs.shape != old_costs.shape
    ):
        raise ValueError("weight and field shapes must match")
    target_row, target_col = old_field.target
    if int(new_costs[target_row, target_col]) == int(INF):
        raise ValueError("target cell is impassable")

    height, width = old_costs.shape
    target_index = target_row * width + target_col
    old_distance = old_field.distance
    distance = old_distance.copy()
    weight_changed = old_costs != new_costs
    seeds = [int(index) for index in np.flatnonzero(weight_changed)]

    # Every old predecessor of a changed node may lose its shortest path.
    # Positive node costs make this predecessor graph a DAG, so this closure
    # terminates and correctly includes a whole detached cycle.
    dirty = np.zeros(distance.size, dtype=np.bool_)
    queue: deque[int] = deque()
    for seed in seeds:
        if seed != target_index and not dirty[seed]:
            dirty[seed] = True
            queue.append(seed)
    while queue:
        index = queue.popleft()
        row, col = divmod(index, width)
        upstream_distance = int(old_distance[row, col])
        if upstream_distance == int(INF):
            continue
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = row + delta_row
            next_col = col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            next_index = next_row * width + next_col
            if next_index == target_index or dirty[next_index]:
                continue
            next_weight = int(old_costs[next_row, next_col])
            next_distance = int(old_distance[next_row, next_col])
            if (
                next_weight != int(INF)
                and next_distance != int(INF)
                and next_distance == next_weight + upstream_distance
            ):
                dirty[next_index] = True
                queue.append(next_index)

    # Rebuild only the dirty region. Distances just outside it are still
    # valid: otherwise they would have been in the predecessor closure.
    distance.reshape(-1)[dirty] = INF
    rebuild_heap: list[tuple[int, int, int]] = []
    for index in np.flatnonzero(dirty):
        row, col = divmod(int(index), width)
        weight = int(new_costs[row, col])
        if weight == int(INF):
            continue
        best = int(INF)
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = row + delta_row
            next_col = col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            next_index = next_row * width + next_col
            if dirty[next_index]:
                continue
            candidate = int(distance[next_row, next_col])
            best = min(best, candidate)
        if best != int(INF):
            candidate_distance = weight + best
            if candidate_distance < int(distance[row, col]):
                distance[row, col] = np.int64(candidate_distance)
                heapq.heappush(rebuild_heap, (candidate_distance, row, col))
    while rebuild_heap:
        current_distance, row, col = heapq.heappop(rebuild_heap)
        if current_distance != int(distance[row, col]):
            continue
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = row + delta_row
            next_col = col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            next_index = next_row * width + next_col
            if next_index == target_index or not dirty[next_index]:
                continue
            next_weight = int(new_costs[next_row, next_col])
            if next_weight == int(INF):
                continue
            candidate = current_distance + next_weight
            if candidate < int(distance[next_row, next_col]):
                distance[next_row, next_col] = np.int64(candidate)
                heapq.heappush(rebuild_heap, (candidate, next_row, next_col))

    # A weight decrease can improve nodes beyond the old predecessor closure.
    decrease_heap: list[tuple[int, int, int]] = []
    decreased = distance < old_distance
    for index in np.flatnonzero(decreased):
        row, col = divmod(int(index), width)
        heapq.heappush(decrease_heap, (int(distance[row, col]), row, col))
    while decrease_heap:
        current_distance, row, col = heapq.heappop(decrease_heap)
        if current_distance != int(distance[row, col]):
            continue
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = row + delta_row
            next_col = col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            next_index = next_row * width + next_col
            if next_index == target_index:
                continue
            next_weight = int(new_costs[next_row, next_col])
            if next_weight == int(INF):
                continue
            candidate = current_distance + next_weight
            if candidate < int(distance[next_row, next_col]):
                distance[next_row, next_col] = np.int64(candidate)
                heapq.heappush(decrease_heap, (candidate, next_row, next_col))

    actual_changed = distance != old_distance
    new_field = CostField.create(old_field.unit_id, old_field.target, distance)
    return new_field, actual_changed, weight_changed


def reconstruct_path(
    field: CostField, weights: np.ndarray, source: tuple[int, int]
) -> tuple[tuple[tuple[int, int], ...], int, bool]:
    """Return ``(path, cost, reachable)`` using fixed neighbor order."""

    costs = np.asarray(weights, dtype=np.int64)
    height, width = costs.shape
    start_row, start_col = int(source[0]), int(source[1])
    if not (0 <= start_row < height and 0 <= start_col < width):
        raise ValueError("source outside grid")
    start_cost = int(field.distance[start_row, start_col])
    if start_cost == int(INF):
        return (), -1, False
    if (start_row, start_col) == field.target:
        return ((start_row, start_col),), 0, True

    path: list[tuple[int, int]] = [(start_row, start_col)]
    row, col = start_row, start_col
    visited = {(row, col)}
    while (row, col) != field.target:
        cell_weight = int(costs[row, col])
        if cell_weight == int(INF):
            raise AssertionError("passable field cell has impassable weight")
        expected = int(field.distance[row, col]) - cell_weight
        chosen: tuple[int, int] | None = None
        for delta_row, delta_col in NEIGHBOR_OFFSETS:
            next_row = row + delta_row
            next_col = col + delta_col
            if not (0 <= next_row < height and 0 <= next_col < width):
                continue
            if int(field.distance[next_row, next_col]) == expected:
                chosen = (next_row, next_col)
                break
        if chosen is None:
            raise AssertionError("shortest-path field is not locally consistent")
        if chosen in visited:
            raise AssertionError("path reconstruction encountered a cycle")
        path.append(chosen)
        visited.add(chosen)
        row, col = chosen
    return tuple(path), start_cost, True


def dependency_indices(path: Iterable[tuple[int, int]], width: int) -> frozenset[int]:
    return frozenset(int(row) * int(width) + int(col) for row, col in path)


def path_record(path: Iterable[tuple[int, int]]) -> dict[str, object]:
    points = tuple(path)
    return {
        "path": [[int(row), int(col)] for row, col in points],
        "path_hash": path_hash(points),
        "path_bytes_hex": path_bytes(points).hex(),
    }
