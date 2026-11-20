"""Versioned tactical path service with isolated recalculation and publishing."""

from __future__ import annotations

import concurrent.futures
import threading
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from typing import Self

import numpy as np

from .cache import CacheKey, PathCacheEntry, path_index_dependencies
from .changes import Change, apply_changes, validate_changes
from .costfield import (
    CostField,
    compute_full_field,
    incremental_update,
    reconstruct_path,
)
from .errors import (
    E_BATCH_FAILED,
    E_BUSY,
    E_RECOVERY_INVALID,
    E_VERSION_MISMATCH,
    ServiceError,
)
from .grid import INF, BattleGrid
from .hashing import path_hash
from .persistence import SnapshotStore
from .snapshot import PublishedSnapshot, initial_chain_hash


@dataclass(frozen=True)
class PathResult:
    unit_id: int
    target: tuple[int, int]
    source: tuple[int, int]
    version: int
    version_hash: str
    field_hash: str
    path: tuple[tuple[int, int], ...]
    path_hash: str
    cost: int
    reachable: bool
    cache_hit: bool


@dataclass(frozen=True)
class BatchStats:
    batch_seq: int
    change_count: int
    previous_version: int
    version: int
    previous_version_hash: str
    version_hash: str
    field_hashes_before: dict[int, str]
    field_hashes_after: dict[int, str]
    generation_cells_after: dict[int, int]
    impact_sizes: dict[int, int]
    raw_merged_impact_sizes: dict[int, int]
    closure_impact_sizes: dict[int, int]
    merged_impact_size: int
    raw_merged_impact_size: int
    closure_impact_size: int
    closure_equals_merged: bool
    closure_extends_raw: bool
    field_replay_equals_merged: bool
    recomputed_cells: int
    local_cost_changed_cells: int
    cache_invalidated: int
    cache_hits: int
    cache_recomputed: int
    cache_entries_after: int
    snapshot_array_bytes: int
    cache_dependency_count: int

    def trace_record(self) -> dict[str, object]:
        return {
            "record": "batch",
            "batch": self.batch_seq,
            "change_count": self.change_count,
            "previous_version": self.previous_version,
            "version": self.version,
            "previous_version_hash": self.previous_version_hash,
            "published_version_hash": self.version_hash,
            "field_hashes_before": {
                str(key): value
                for key, value in sorted(self.field_hashes_before.items())
            },
            "field_hashes_after": {
                str(key): value
                for key, value in sorted(self.field_hashes_after.items())
            },
            "generation_cells_after": {
                str(key): value
                for key, value in sorted(self.generation_cells_after.items())
            },
            "impact_sizes": {
                str(key): value for key, value in sorted(self.impact_sizes.items())
            },
            "raw_merged_impact_sizes": {
                str(key): value
                for key, value in sorted(self.raw_merged_impact_sizes.items())
            },
            "closure_impact_sizes": {
                str(key): value
                for key, value in sorted(self.closure_impact_sizes.items())
            },
            "merged_impact_size": self.merged_impact_size,
            "raw_merged_impact_size": self.raw_merged_impact_size,
            "closure_impact_size": self.closure_impact_size,
            "closure_equals_merged": self.closure_equals_merged,
            "closure_extends_raw": self.closure_extends_raw,
            "field_replay_equals_merged": self.field_replay_equals_merged,
            "recomputed_cells": self.recomputed_cells,
            "local_cost_changed_cells": self.local_cost_changed_cells,
            "cache_invalidated": self.cache_invalidated,
            "cache_hits": self.cache_hits,
            "cache_recomputed": self.cache_recomputed,
            "cache_entries_after": self.cache_entries_after,
            "snapshot_array_bytes": self.snapshot_array_bytes,
            "cache_dependency_count": self.cache_dependency_count,
        }


@dataclass(frozen=True)
class PreparedBatch:
    base_version: int
    base_version_hash: str
    snapshot: PublishedSnapshot
    stats: BatchStats


@dataclass(frozen=True)
class PathTask:
    key: tuple[int, tuple[int, int]]
    future: concurrent.futures.Future[PathResult]


class _TaskRegistry:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active: set[tuple[int, tuple[int, int]]] = set()

    def claim(self, key: tuple[int, tuple[int, int]]) -> bool:
        with self._lock:
            if key in self._active:
                return False
            self._active.add(key)
            return True

    def release(self, key: tuple[int, tuple[int, int]]) -> None:
        with self._lock:
            self._active.discard(key)

    def active_keys(self) -> tuple[tuple[int, tuple[int, int]], ...]:
        with self._lock:
            return tuple(sorted(self._active))


class TacticalService:
    """Publishes complete snapshots; queries never lock onto a half-built field."""

    def __init__(
        self,
        grid: BattleGrid,
        targets: Mapping[int, tuple[int, int]],
        cache_queries: Iterable[
            tuple[int, tuple[int, int]] | tuple[int, tuple[int, int], tuple[int, int]]
        ] = (),
        store: SnapshotStore | str | None = None,
        persist_initial: bool = True,
    ) -> None:
        normalized_targets = self._normalize_targets(grid, targets)
        fields: dict[int, CostField] = {}
        for unit in range(grid.unit_count):
            weights = grid.cell_cost(unit)
            target = normalized_targets[unit]
            if int(weights[target[0], target[1]]) == int(INF):
                raise ValueError(f"target for unit {unit} is impassable")
            fields[unit] = CostField.create(
                unit, target, compute_full_field(weights, target)
            )
        self._grid = grid
        self._targets = normalized_targets
        self._store = (
            store
            if isinstance(store, SnapshotStore) or store is None
            else SnapshotStore(store)
        )
        if (
            self._store is not None
            and persist_initial
            and any(self._store.root.glob("snapshot-*.snap"))
        ):
            raise ValueError(
                "snapshot store already contains publishes; use TacticalService.recover"
            )
        self._commit_lock = threading.RLock()
        self._publish_lock = threading.Lock()
        self._task_registry = _TaskRegistry()
        self._executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=4, thread_name_prefix="path-recalc"
        )
        initial_cache = self._build_query_cache(grid, fields, 0, tuple(cache_queries))
        snapshot = PublishedSnapshot.create(
            0,
            0,
            grid,
            fields,
            initial_cache,
            initial_chain_hash(),
        )
        self._snapshot = snapshot
        if self._store is not None and persist_initial:
            self._store.save(snapshot)

    @staticmethod
    def _normalize_targets(
        grid: BattleGrid, targets: Mapping[int, tuple[int, int]]
    ) -> dict[int, tuple[int, int]]:
        normalized: dict[int, tuple[int, int]] = {}
        for unit in range(grid.unit_count):
            if unit not in targets:
                raise ValueError(f"missing target for unit {unit}")
            raw = targets[unit]
            target = (int(raw[0]), int(raw[1]))
            if not (0 <= target[0] < grid.height and 0 <= target[1] < grid.width):
                raise ValueError(f"target outside grid for unit {unit}")
            normalized[unit] = target
        extra = {int(unit) for unit in targets}.difference(range(grid.unit_count))
        if extra:
            raise ValueError(f"targets for unknown units: {sorted(extra)}")
        return normalized

    @classmethod
    def recover(
        cls, store: SnapshotStore | str, persist_initial: bool = False
    ) -> TacticalService:
        snapshot_store = (
            store if isinstance(store, SnapshotStore) else SnapshotStore(store)
        )
        snapshot = snapshot_store.recover_latest()
        service = cls.__new__(cls)
        service._grid = snapshot.grid
        service._targets = {
            unit: field.target for unit, field in snapshot.fields.items()
        }
        service._store = snapshot_store
        service._commit_lock = threading.RLock()
        service._publish_lock = threading.Lock()
        service._task_registry = _TaskRegistry()
        service._executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=4, thread_name_prefix="path-recalc"
        )
        service._snapshot = snapshot
        return service

    @property
    def snapshot(self) -> PublishedSnapshot:
        with self._publish_lock:
            return self._snapshot

    @property
    def version(self) -> int:
        return self.snapshot.version

    @property
    def version_hash(self) -> str:
        return self.snapshot.version_hash

    @property
    def targets(self) -> dict[int, tuple[int, int]]:
        return dict(self._targets)

    @property
    def store(self) -> SnapshotStore | None:
        return self._store

    def active_task_keys(self) -> tuple[tuple[int, tuple[int, int]], ...]:
        return self._task_registry.active_keys()

    def _check_query_version(
        self,
        snapshot: PublishedSnapshot,
        expected_version: int | None,
        expected_hash: str | None,
    ) -> None:
        if expected_version is not None and int(expected_version) != snapshot.version:
            raise ServiceError(
                E_VERSION_MISMATCH,
                f"expected version {expected_version}, current {snapshot.version}",
            )
        if expected_hash is not None and str(expected_hash) != snapshot.version_hash:
            raise ServiceError(
                E_VERSION_MISMATCH,
                f"expected hash {expected_hash}, current {snapshot.version_hash}",
            )

    def get_path(
        self,
        unit_id: int,
        source: tuple[int, int],
        target: tuple[int, int] | None = None,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> PathResult:
        snapshot = self.snapshot
        self._check_query_version(snapshot, expected_version, expected_hash)
        unit = int(unit_id)
        if unit not in snapshot.fields:
            raise ValueError(f"unknown unit: {unit_id}")
        primary = snapshot.fields[unit]
        resolved_target = (
            primary.target
            if target is None
            else (
                int(target[0]),
                int(target[1]),
            )
        )
        source_point = (int(source[0]), int(source[1]))
        if not (
            0 <= source_point[0] < snapshot.grid.height
            and 0 <= source_point[1] < snapshot.grid.width
        ):
            raise ValueError("source outside grid")
        if not (
            0 <= resolved_target[0] < snapshot.grid.height
            and 0 <= resolved_target[1] < snapshot.grid.width
        ):
            raise ValueError("target outside grid")

        key: CacheKey = (unit, resolved_target, source_point)
        entry = (
            snapshot.path_cache.get(key) if resolved_target == primary.target else None
        )
        if entry is not None and entry.version == snapshot.version:
            return PathResult(
                unit,
                resolved_target,
                source_point,
                snapshot.version,
                snapshot.version_hash,
                primary.field_hash,
                entry.path,
                entry.path_hash,
                entry.cost,
                entry.reachable,
                True,
            )

        weights = snapshot.grid.cell_cost(unit)
        if resolved_target == primary.target:
            field = primary
        else:
            field = CostField.create(
                unit,
                resolved_target,
                compute_full_field(weights, resolved_target),
            )
        path, cost, reachable = reconstruct_path(field, weights, source_point)
        return PathResult(
            unit,
            resolved_target,
            source_point,
            snapshot.version,
            snapshot.version_hash,
            field.field_hash,
            path,
            path_hash(path),
            cost,
            reachable,
            False,
        )

    def get_cost(
        self,
        unit_id: int,
        row: int,
        col: int,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> int:
        snapshot = self.snapshot
        self._check_query_version(snapshot, expected_version, expected_hash)
        field = snapshot.fields.get(int(unit_id))
        if field is None:
            raise ValueError(f"unknown unit: {unit_id}")
        if not (
            0 <= int(row) < snapshot.grid.height and 0 <= int(col) < snapshot.grid.width
        ):
            raise ValueError("cell outside grid")
        value = int(field.distance[int(row), int(col)])
        return -1 if value == int(INF) else value

    def prepare_batch(
        self, changes: Sequence[Change], verify_closure: bool = True
    ) -> PreparedBatch:
        with self._commit_lock:
            base = self._snapshot
        try:
            return self._build_batch(base, tuple(changes), verify_closure)
        except ServiceError:
            raise
        except Exception as exc:
            raise ServiceError(E_BATCH_FAILED, str(exc)) from exc

    def publish_prepared(
        self,
        prepared: PreparedBatch,
        expected_base_version: int | None = None,
        expected_base_hash: str | None = None,
    ) -> BatchStats:
        with self._commit_lock:
            base = self._snapshot
            if (
                expected_base_version is not None
                and base.version != expected_base_version
            ):
                raise ServiceError(
                    E_VERSION_MISMATCH,
                    f"prepared for version {expected_base_version}, current {base.version}",
                )
            if (
                expected_base_hash is not None
                and base.version_hash != expected_base_hash
            ):
                raise ServiceError(
                    E_VERSION_MISMATCH,
                    "prepared snapshot base hash is stale",
                )
            if (
                base.version != prepared.base_version
                or base.version_hash != prepared.base_version_hash
            ):
                raise ServiceError(
                    E_VERSION_MISMATCH,
                    "prepared snapshot is no longer based on the published version",
                )
            if self._store is not None:
                try:
                    self._store.save(prepared.snapshot)
                except Exception as exc:
                    raise ServiceError(
                        E_BATCH_FAILED, f"snapshot persistence failed: {exc}"
                    ) from exc
            with self._publish_lock:
                self._snapshot = prepared.snapshot
            return prepared.stats

    def commit_batch(
        self, changes: Sequence[Change], verify_closure: bool = True
    ) -> BatchStats:
        with self._commit_lock:
            base = self._snapshot
            try:
                prepared = self._build_batch(base, tuple(changes), verify_closure)
                if self._store is not None:
                    self._store.save(prepared.snapshot)
            except ServiceError as exc:
                if exc.code == E_BATCH_FAILED:
                    raise
                raise ServiceError(E_BATCH_FAILED, str(exc)) from exc
            except Exception as exc:
                raise ServiceError(E_BATCH_FAILED, str(exc)) from exc
            with self._publish_lock:
                self._snapshot = prepared.snapshot
            return prepared.stats

    def _build_batch(
        self,
        base: PublishedSnapshot,
        changes: tuple[Change, ...],
        verify_closure: bool,
    ) -> PreparedBatch:
        ordered = validate_changes(base.grid, changes)
        candidate_grid = apply_changes(base.grid, ordered)
        for unit, target in self._targets.items():
            if not candidate_grid.is_passable(unit, target[0], target[1]):
                raise ServiceError(
                    E_BATCH_FAILED,
                    f"batch blocks target for unit {unit}",
                )

        new_fields: dict[int, CostField] = {}
        impact_masks: dict[int, np.ndarray] = {}
        local_changed_total = 0
        for unit in range(base.grid.unit_count):
            old_weights = base.grid.cell_cost(unit)
            new_weights = candidate_grid.cell_cost(unit)
            new_field, actual_changed, weight_changed = incremental_update(
                base.fields[unit], old_weights, new_weights
            )
            new_fields[unit] = new_field
            impact_masks[unit] = actual_changed | weight_changed
            local_changed_total += int(np.count_nonzero(weight_changed))

        raw_impact_masks = {unit: mask.copy() for unit, mask in impact_masks.items()}
        if verify_closure:
            closure_fields, closure_masks = self._sequential_closure(base, ordered)
            field_replay_equals_merged = all(
                np.array_equal(new_fields[unit].distance, closure_fields[unit].distance)
                for unit in new_fields
            )
            if not field_replay_equals_merged:
                raise ServiceError(
                    E_BATCH_FAILED,
                    "merged field does not equal sequential change replay",
                )
            closure_extends_raw = any(
                not np.array_equal(raw_impact_masks[unit], closure_masks[unit])
                for unit in new_fields
            )
            impact_masks = {
                unit: raw_impact_masks[unit] | closure_masks[unit]
                for unit in new_fields
            }
            closure_equal = all(
                np.array_equal(impact_masks[unit], closure_masks[unit])
                for unit in new_fields
            )
            if not closure_equal:
                raise ServiceError(
                    E_BATCH_FAILED,
                    "sequential closure does not cover the merged impact",
                )
        else:
            closure_fields = new_fields
            closure_masks = impact_masks
            closure_equal = True
            closure_extends_raw = False
            field_replay_equals_merged = True

        next_version = base.version + 1
        next_batch_seq = base.batch_seq + 1
        new_cache: dict[CacheKey, PathCacheEntry] = {}
        invalidated = 0
        hits = 0
        recomputed = 0
        for key, entry in sorted(base.path_cache.items()):
            affected = impact_masks.get(entry.unit_id)
            invalid = entry.version != base.version
            if affected is not None:
                if not entry.reachable:
                    invalid = invalid or bool(np.any(affected))
                elif entry.dependencies:
                    dependency_indices = np.fromiter(
                        entry.dependencies,
                        dtype=np.int64,
                        count=len(entry.dependencies),
                    )
                    invalid = invalid or bool(
                        np.any(affected.reshape(-1)[dependency_indices])
                    )
            if invalid:
                invalidated += 1
                new_cache[key] = self._make_cache_entry(
                    entry.unit_id,
                    entry.target,
                    entry.source,
                    candidate_grid,
                    new_fields,
                    next_version,
                )
                recomputed += 1
            else:
                hits += 1
                new_cache[key] = PathCacheEntry.create(
                    entry.unit_id,
                    entry.target,
                    entry.source,
                    next_version,
                    entry.path,
                    entry.dependencies,
                    entry.cost,
                    entry.reachable,
                )

        snapshot = PublishedSnapshot.create(
            next_version,
            next_batch_seq,
            candidate_grid,
            new_fields,
            new_cache,
            base.version_hash,
        )
        impact_sizes = {
            unit: int(np.count_nonzero(mask))
            for unit, mask in sorted(impact_masks.items())
        }
        raw_merged_impact_sizes = {
            unit: int(np.count_nonzero(mask))
            for unit, mask in sorted(raw_impact_masks.items())
        }
        closure_impact_sizes = {
            unit: int(np.count_nonzero(mask))
            for unit, mask in sorted(closure_masks.items())
        }
        stats = BatchStats(
            batch_seq=next_batch_seq,
            change_count=len(ordered),
            previous_version=base.version,
            version=next_version,
            previous_version_hash=base.version_hash,
            version_hash=snapshot.version_hash,
            field_hashes_before=base.field_hashes(),
            field_hashes_after=snapshot.field_hashes(),
            generation_cells_after={
                unit: field.generation_cells
                for unit, field in sorted(new_fields.items())
            },
            impact_sizes=impact_sizes,
            raw_merged_impact_sizes=raw_merged_impact_sizes,
            closure_impact_sizes=closure_impact_sizes,
            merged_impact_size=sum(impact_sizes.values()),
            raw_merged_impact_size=sum(raw_merged_impact_sizes.values()),
            closure_impact_size=sum(closure_impact_sizes.values()),
            closure_equals_merged=closure_equal,
            closure_extends_raw=closure_extends_raw,
            field_replay_equals_merged=field_replay_equals_merged,
            recomputed_cells=sum(impact_sizes.values()),
            local_cost_changed_cells=local_changed_total,
            cache_invalidated=invalidated,
            cache_hits=hits,
            cache_recomputed=recomputed,
            cache_entries_after=len(new_cache),
            snapshot_array_bytes=snapshot.array_nbytes(),
            cache_dependency_count=snapshot.cache_dependency_count,
        )
        return PreparedBatch(
            base_version=base.version,
            base_version_hash=base.version_hash,
            snapshot=snapshot,
            stats=stats,
        )

    def _sequential_closure(
        self,
        base: PublishedSnapshot,
        changes: tuple[Change, ...],
    ) -> tuple[dict[int, CostField], dict[int, np.ndarray]]:
        current_grid = base.grid
        current_fields = dict(base.fields)
        impact_masks = {
            unit: np.zeros(base.grid.shape, dtype=np.bool_)
            for unit in range(base.grid.unit_count)
        }
        for change in changes:
            next_grid = apply_changes(current_grid, (change,))
            for unit in range(base.grid.unit_count):
                old_weights = current_grid.cell_cost(unit)
                new_weights = next_grid.cell_cost(unit)
                if np.array_equal(old_weights, new_weights):
                    continue
                field, actual_changed, weight_changed = incremental_update(
                    current_fields[unit], old_weights, new_weights
                )
                current_fields[unit] = field
                impact_masks[unit] |= actual_changed | weight_changed
            current_grid = next_grid
        return current_fields, impact_masks

    def _build_query_cache(
        self,
        grid: BattleGrid,
        fields: Mapping[int, CostField],
        version: int,
        queries: tuple[
            tuple[int, tuple[int, int]] | tuple[int, tuple[int, int], tuple[int, int]],
            ...,
        ],
    ) -> dict[CacheKey, PathCacheEntry]:
        cache: dict[CacheKey, PathCacheEntry] = {}
        for query in queries:
            if len(query) == 2:
                unit, source = query
                target = fields[int(unit)].target
            elif len(query) == 3:
                unit, target, source = query
            else:
                raise ValueError("cache query must have 2 or 3 elements")
            entry = self._make_cache_entry(
                int(unit),
                (int(target[0]), int(target[1])),
                (int(source[0]), int(source[1])),
                grid,
                fields,
                version,
            )
            cache[entry.key] = entry
        return cache

    @staticmethod
    def _make_cache_entry(
        unit: int,
        target: tuple[int, int],
        source: tuple[int, int],
        grid: BattleGrid,
        fields: Mapping[int, CostField],
        version: int,
    ) -> PathCacheEntry:
        if unit not in fields:
            raise ValueError(f"unknown unit: {unit}")
        weights = grid.cell_cost(unit)
        primary = fields[unit]
        if primary.target == target:
            field = primary
        else:
            field = CostField.create(unit, target, compute_full_field(weights, target))
        path, cost, reachable = reconstruct_path(field, weights, source)
        dependencies = path_index_dependencies(path, grid.height, grid.width)
        return PathCacheEntry.create(
            unit,
            target,
            source,
            version,
            path,
            dependencies,
            cost,
            reachable,
        )

    def submit_path_task(
        self,
        unit_id: int,
        source: tuple[int, int],
        target: tuple[int, int] | None = None,
    ) -> PathTask:
        snapshot = self.snapshot
        unit = int(unit_id)
        if unit not in snapshot.fields:
            raise ValueError(f"unknown unit: {unit_id}")
        resolved_target = (
            snapshot.fields[unit].target
            if target is None
            else (int(target[0]), int(target[1]))
        )
        key = (unit, resolved_target)
        if not self._task_registry.claim(key):
            raise ServiceError(
                E_BUSY,
                f"path task already active for unit {unit}, target {resolved_target}",
            )
        try:
            future = self._executor.submit(
                self._execute_path_task,
                key,
                snapshot,
                unit,
                resolved_target,
                (int(source[0]), int(source[1])),
            )
        except Exception:
            self._task_registry.release(key)
            raise
        return PathTask(key, future)

    def _execute_path_task(
        self,
        key: tuple[int, tuple[int, int]],
        snapshot: PublishedSnapshot,
        unit: int,
        target: tuple[int, int],
        source: tuple[int, int],
    ) -> PathResult:
        try:
            return self._run_path_task(snapshot, unit, target, source)
        finally:
            self._task_registry.release(key)

    @staticmethod
    def _run_path_task(
        snapshot: PublishedSnapshot,
        unit: int,
        target: tuple[int, int],
        source: tuple[int, int],
    ) -> PathResult:
        weights = snapshot.grid.cell_cost(unit)
        temporary_field = CostField.create(
            unit, target, compute_full_field(weights, target)
        )
        path, cost, reachable = reconstruct_path(temporary_field, weights, source)
        return PathResult(
            unit,
            target,
            source,
            snapshot.version,
            snapshot.version_hash,
            temporary_field.field_hash,
            path,
            path_hash(path),
            cost,
            reachable,
            False,
        )

    def close(self) -> None:
        self._executor.shutdown(wait=True, cancel_futures=False)

    def __enter__(self) -> Self:
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.close()


def recover_service(store: SnapshotStore | str) -> TacticalService:
    try:
        return TacticalService.recover(store)
    except ServiceError:
        raise
    except Exception as exc:
        raise ServiceError(E_RECOVERY_INVALID, str(exc)) from exc
