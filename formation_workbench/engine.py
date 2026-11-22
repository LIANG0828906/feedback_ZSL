"""Cached formation pipeline, dependency invalidation and consistency checks."""

from __future__ import annotations

import copy
import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping

from .checkpoint import CheckpointRead, CheckpointStore
from .errors import ErrorCode, WorkbenchError
from .executor import BoundedExecutor
from .fixed import MAX_CELL_COST
from .models import (
    ALGORITHM_VERSION,
    FIXED_POINT_VERSION,
    SCHEMA_VERSION,
    Coord,
    Scene,
    SearchConfig,
    SimulationConfig,
)
from .search import RawPath, plan_member
from .simulation import (
    MemberTrace,
    SimulationInterval,
    interval_digest,
    simulate_member,
    split_intervals,
)
from .smoothing import SmoothedPath, smooth_member, traversed_cells

CHECKPOINT_FORMAT_VERSION = "formation-checkpoint-1"


@dataclass
class UnitState:
    member_id: str
    path_cached: bool = False
    smooth_cached: bool = False
    simulation_cached: bool = False
    path_cache_hit: bool = False
    smooth_cache_hit: bool = False
    simulation_cache_hit: bool = False
    path_ms: float = 0.0
    smooth_ms: float = 0.0
    simulation_ms: float = 0.0
    path_error: dict[str, Any] | None = None
    smooth_error: dict[str, Any] | None = None
    simulation_error: dict[str, Any] | None = None
    revision: int = 0

    @property
    def status(self) -> str:
        if self.path_error or self.smooth_error or self.simulation_error:
            return "failed"
        if self.path_cached and self.smooth_cached and self.simulation_cached:
            return "ok"
        return "pending"

    def to_dict(self) -> dict[str, Any]:
        return {
            "member_id": self.member_id,
            "status": self.status,
            "path_cached": self.path_cached,
            "smooth_cached": self.smooth_cached,
            "simulation_cached": self.simulation_cached,
            "path_cache_hit": self.path_cache_hit,
            "smooth_cache_hit": self.smooth_cache_hit,
            "simulation_cache_hit": self.simulation_cache_hit,
            "path_ms": round(self.path_ms, 3),
            "smooth_ms": round(self.smooth_ms, 3),
            "simulation_ms": round(self.simulation_ms, 3),
            "path_error": self.path_error,
            "smooth_error": self.smooth_error,
            "simulation_error": self.simulation_error,
            "revision": self.revision,
        }


@dataclass(frozen=True)
class InvalidationManifest:
    revision: int
    edit: dict[str, Any]
    changed_cells: tuple[Coord, ...]
    direct_path_members: tuple[str, ...]
    path_recomputed: tuple[str, ...]
    smooth_recomputed: tuple[str, ...]
    simulation_recomputed: tuple[str, ...]
    reused_paths: tuple[str, ...]
    reused_simulations: tuple[str, ...]
    invalidated_intervals: tuple[SimulationInterval, ...]
    reused_intervals: tuple[SimulationInterval, ...]
    reasons: tuple[dict[str, Any], ...]
    elapsed_ms: float
    status: str
    failures: tuple[dict[str, Any], ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "revision": self.revision,
            "edit": self.edit,
            "changed_cells": [list(cell) for cell in self.changed_cells],
            "direct_path_members": list(self.direct_path_members),
            "path_recomputed": list(self.path_recomputed),
            "smooth_recomputed": list(self.smooth_recomputed),
            "simulation_recomputed": list(self.simulation_recomputed),
            "reused_paths": list(self.reused_paths),
            "reused_simulations": list(self.reused_simulations),
            "invalidated_intervals": [
                interval.to_dict() for interval in self.invalidated_intervals
            ],
            "reused_intervals": [
                interval.to_dict() for interval in self.reused_intervals
            ],
            "reasons": list(self.reasons),
            "elapsed_ms": round(self.elapsed_ms, 3),
            "status": self.status,
            "failures": list(self.failures),
        }


@dataclass(frozen=True)
class ComparisonResult:
    ok: bool
    checked_members: tuple[str, ...]
    checked_ticks: int
    path_cells: int
    waypoints: int
    elapsed_ms: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "checked_members": list(self.checked_members),
            "checked_ticks": self.checked_ticks,
            "path_cells": self.path_cells,
            "waypoints": self.waypoints,
            "elapsed_ms": round(self.elapsed_ms, 3),
        }


@dataclass(frozen=True)
class RestoreResult:
    restored: bool
    reason: str
    digest: str | None = None
    full_rebuild_required: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "restored": self.restored,
            "reason": self.reason,
            "digest": self.digest,
            "full_rebuild_required": self.full_rebuild_required,
        }


def _error_dict(exc: BaseException) -> dict[str, Any]:
    if isinstance(exc, WorkbenchError):
        return exc.to_dict()
    return {
        "code": ErrorCode.E_STATE.value,
        "message": f"{type(exc).__name__}: {exc}",
    }


def _sorted_members(scene: Scene) -> tuple[str, ...]:
    return tuple(sorted(member.member_id for member in scene.members))


class FormationEngine:
    """A serialized engine with per-member caches and exact dependency closure."""

    def __init__(
        self,
        scene: Scene,
        *,
        checkpoint_path: str | Path | None = None,
        max_workers: int | None = None,
        per_worker_buffer: int | None = None,
    ) -> None:
        import threading

        self._scene = scene
        self._checkpoint_store = (
            CheckpointStore(checkpoint_path) if checkpoint_path is not None else None
        )
        self._max_workers = (
            scene.simulation.max_parallel_workers if max_workers is None else max_workers
        )
        self._per_worker_buffer = (
            scene.simulation.per_worker_buffer
            if per_worker_buffer is None
            else per_worker_buffer
        )
        self._executor = BoundedExecutor(self._max_workers, self._per_worker_buffer)
        self._lock = threading.RLock()
        self._operation_lock = threading.RLock()
        self._raw_paths: dict[str, RawPath] = {}
        self._smoothed_paths: dict[str, SmoothedPath] = {}
        self._traces: dict[str, MemberTrace] = {}
        self._intervals: dict[str, tuple[SimulationInterval, ...]] = {}
        self._states: dict[str, UnitState] = {
            member_id: UnitState(member_id=member_id)
            for member_id in _sorted_members(scene)
        }
        self._revision = 0
        self._last_manifest: InvalidationManifest | None = None
        self._last_build_ms = 0.0
        self._last_checkpoint_digest: str | None = None
        self._restore_result: RestoreResult | None = None
        self._blockade_history: dict[str, set[str]] = {}

    @property
    def scene(self) -> Scene:
        with self._lock:
            return self._scene

    @property
    def revision(self) -> int:
        with self._lock:
            return self._revision

    @property
    def last_manifest(self) -> InvalidationManifest | None:
        with self._lock:
            return self._last_manifest

    @property
    def checkpoint_path(self) -> Path | None:
        return self._checkpoint_store.path if self._checkpoint_store is not None else None

    def snapshot_caches(self) -> dict[str, Any]:
        with self._lock:
            return {
                "scene": self._scene,
                "revision": self._revision,
                "raw_paths": dict(self._raw_paths),
                "smoothed_paths": dict(self._smoothed_paths),
                "traces": dict(self._traces),
                "intervals": dict(self._intervals),
                "states": {key: copy.copy(value) for key, value in self._states.items()},
            }

    def member_states(self) -> dict[str, dict[str, Any]]:
        with self._lock:
            return {
                member_id: state.to_dict()
                for member_id, state in sorted(self._states.items())
            }

    def paths(self) -> dict[str, SmoothedPath]:
        with self._lock:
            return dict(self._smoothed_paths)

    def traces(self) -> dict[str, MemberTrace]:
        with self._lock:
            return dict(self._traces)

    def intervals(self) -> dict[str, tuple[SimulationInterval, ...]]:
        with self._lock:
            return dict(self._intervals)

    def dependency_graph_payload(self) -> dict[str, Any]:
        with self._lock:
            scene = self._scene
            raw_paths = dict(self._raw_paths)
            intervals = dict(self._intervals)
        path_dependencies = {
            member_id: path.dependencies.to_dict()
            for member_id, path in sorted(raw_paths.items())
        }
        interval_edges: list[dict[str, Any]] = []
        for child_id in scene.formation.topo_order():
            parent_id = scene.formation.parents.get(child_id)
            if parent_id is None:
                continue
            parent_intervals = intervals.get(parent_id, ())
            child_intervals = intervals.get(child_id, ())
            for parent_interval in parent_intervals:
                for child_interval in child_intervals:
                    start = max(parent_interval.tick_start, child_interval.tick_start)
                    end = min(parent_interval.tick_end, child_interval.tick_end)
                    if start > end:
                        continue
                    interval_edges.append(
                        {
                            "parent_member_id": parent_id,
                            "parent_interval_index": parent_interval.interval_index,
                            "child_member_id": child_id,
                            "child_interval_index": child_interval.interval_index,
                            "tick_start": start,
                            "tick_end": end,
                            "reason": "formation_parent_child_slot_coupling",
                        }
                    )
        return {
            "topology_order": list(scene.formation.topo_order()),
            "path_dependencies": path_dependencies,
            "simulation_interval_edges": interval_edges,
        }

    def _new_state_map(self) -> dict[str, UnitState]:
        return {
            member_id: UnitState(member_id=member_id, revision=self._revision + 1)
            for member_id in _sorted_members(self._scene)
        }

    def _run_simulation_waves(
        self,
        scene: Scene,
        smoothed_paths: Mapping[str, SmoothedPath],
        seed_traces: Mapping[str, MemberTrace],
        member_ids: set[str],
        *,
        parallel: bool = True,
    ) -> tuple[
        dict[str, MemberTrace],
        dict[str, dict[str, Any]],
        dict[str, float],
    ]:
        traces = dict(seed_traces)
        errors: dict[str, dict[str, Any]] = {}
        timings: dict[str, float] = {}
        pending = {
            member_id
            for member_id in member_ids
            if member_id in smoothed_paths
        }
        topology = scene.formation.topo_order()
        while pending:
            ready: list[str] = []
            for member_id in topology:
                if member_id not in pending:
                    continue
                parent_id = scene.formation.parents.get(member_id)
                if parent_id is None or parent_id in traces:
                    ready.append(member_id)
            if not ready:
                for member_id in sorted(pending):
                    parent_id = scene.formation.parents.get(member_id)
                    errors[member_id] = {
                        "code": ErrorCode.E_STATE.value,
                        "message": f"parent '{parent_id}' trace unavailable",
                        "member_id": member_id,
                    }
                pending.clear()
                break

            def compute_trace(member_id: str) -> tuple[MemberTrace, float]:
                begin = time.perf_counter()
                parent_id = scene.formation.parents.get(member_id)
                trace = simulate_member(
                    scene,
                    member_id,
                    smoothed_paths,
                    parent_trace=traces.get(parent_id) if parent_id is not None else None,
                )
                return trace, (time.perf_counter() - begin) * 1000.0

            if parallel and len(ready) > 1:
                outcomes = self._executor.run_ordered(ready, compute_trace)
            else:
                outcomes = []
                for member_id in ready:
                    begin = time.perf_counter()
                    try:
                        value = compute_trace(member_id)
                        outcomes.append(
                            type(
                                "_Outcome",
                                (),
                                {
                                    "item": member_id,
                                    "value": value,
                                    "error": None,
                                    "elapsed_ms": (time.perf_counter() - begin) * 1000.0,
                                },
                            )()
                        )
                    except BaseException as exc:
                        outcomes.append(
                            type(
                                "_Outcome",
                                (),
                                {
                                    "item": member_id,
                                    "value": None,
                                    "error": exc,
                                    "elapsed_ms": (time.perf_counter() - begin) * 1000.0,
                                },
                            )()
                        )
            for outcome in outcomes:
                if outcome.error is not None:
                    errors[outcome.item] = _error_dict(outcome.error)
                    continue
                trace, simulation_ms = outcome.value  # type: ignore[misc]
                traces[outcome.item] = trace
                timings[outcome.item] = simulation_ms
            pending.difference_update(ready)
        return traces, errors, timings

    def build_all(
        self,
        *,
        parallel: bool = True,
        save_checkpoint: bool = True,
    ) -> dict[str, Any]:
        with self._operation_lock:
            started = time.perf_counter()
            with self._lock:
                scene = self._scene
                self._revision += 1
                revision = self._revision
            member_ids = _sorted_members(scene)
            states = {
                member_id: UnitState(member_id=member_id, revision=revision)
                for member_id in member_ids
            }

            def compute_path(member_id: str) -> tuple[RawPath, float]:
                begin = time.perf_counter()
                path = plan_member(scene, member_id)
                return path, (time.perf_counter() - begin) * 1000.0

            if parallel and len(member_ids) > 1:
                outcomes = self._executor.run_ordered(member_ids, compute_path)
            else:
                outcomes = []
                for member_id in member_ids:
                    begin = time.perf_counter()
                    try:
                        value = compute_path(member_id)
                        outcomes.append(
                            type(
                                "_Outcome",
                                (),
                                {
                                    "item": member_id,
                                    "value": value,
                                    "error": None,
                                    "elapsed_ms": (time.perf_counter() - begin) * 1000.0,
                                },
                            )()
                        )
                    except BaseException as exc:
                        outcomes.append(
                            type(
                                "_Outcome",
                                (),
                                {
                                    "item": member_id,
                                    "value": None,
                                    "error": exc,
                                    "elapsed_ms": (time.perf_counter() - begin) * 1000.0,
                                },
                            )()
                        )

            raw_paths: dict[str, RawPath] = {}
            for outcome in outcomes:
                state = states[outcome.item]
                if outcome.error is not None:
                    state.path_error = _error_dict(outcome.error)
                    continue
                path, path_ms = outcome.value  # type: ignore[misc]
                raw_paths[outcome.item] = path
                state.path_cached = True
                state.path_ms = path_ms

            smoothed_paths: dict[str, SmoothedPath] = {}
            for member_id in scene.formation.topo_order():
                state = states[member_id]
                raw_path = raw_paths.get(member_id)
                if raw_path is None:
                    if state.path_error is None:
                        state.path_error = {
                            "code": ErrorCode.E_STATE.value,
                            "message": "path search did not produce a path",
                        }
                    continue
                parent_id = scene.formation.parents.get(member_id)
                if parent_id is not None and parent_id not in smoothed_paths:
                    state.smooth_error = {
                        "code": ErrorCode.E_STATE.value,
                        "message": f"parent '{parent_id}' smoothing failed; dependent member isolated",
                    }
                    continue
                begin = time.perf_counter()
                try:
                    smoothed_paths[member_id] = smooth_member(
                        scene,
                        raw_path,
                        smoothed_paths.get(parent_id) if parent_id is not None else None,
                    )
                    state.smooth_cached = True
                    state.smooth_ms = (time.perf_counter() - begin) * 1000.0
                except BaseException as exc:
                    state.smooth_error = _error_dict(exc)

            traces, trace_errors, trace_ms = self._run_simulation_waves(
                scene,
                smoothed_paths,
                {},
                set(smoothed_paths),
                parallel=parallel,
            )
            for member_id, error in trace_errors.items():
                states[member_id].simulation_error = error
            for member_id, simulation_ms in trace_ms.items():
                states[member_id].simulation_cached = True
                states[member_id].simulation_ms = simulation_ms

            intervals = {
                member_id: split_intervals(trace, scene.simulation.interval_ticks)
                for member_id, trace in traces.items()
            }
            complete = (
                len(raw_paths) == len(member_ids)
                and len(smoothed_paths) == len(member_ids)
                and len(traces) == len(member_ids)
            )
            with self._lock:
                self._raw_paths = raw_paths
                self._smoothed_paths = smoothed_paths
                self._traces = traces
                self._intervals = intervals
                self._states = states
                self._last_build_ms = (time.perf_counter() - started) * 1000.0
                self._last_manifest = None
                self._blockade_history = {}
            payload = {
                "status": "ok" if complete else "partial",
                "revision": revision,
                "elapsed_ms": round((time.perf_counter() - started) * 1000.0, 3),
                "complete": complete,
                "members": {
                    key: value.to_dict() for key, value in sorted(states.items())
                },
            }
            if complete and save_checkpoint:
                payload["checkpoint"] = self.save_checkpoint()
            return payload

    def _path_affected_by_cell(
        self, member_id: str, coord: Coord, *, opening: bool = False
    ) -> bool:
        smoothed = self._smoothed_paths.get(member_id)
        if smoothed is None:
            return False
        if coord in smoothed.raw.dependencies.cells:
            return True
        if coord in traversed_cells(smoothed):
            return True
        return opening and coord in smoothed.raw.dependencies.search_cells

    def _direct_path_members(
        self, changed_cells: set[Coord], *, opening: bool = False
    ) -> tuple[str, ...]:
        result = []
        for member_id in _sorted_members(self._scene):
            if any(
                self._path_affected_by_cell(member_id, coord, opening=opening)
                for coord in changed_cells
            ):
                result.append(member_id)
        return tuple(result)

    def apply_cell_cost(self, coord: Coord, cost: int) -> InvalidationManifest:
        with self._operation_lock:
            if (
                not isinstance(coord, tuple)
                or len(coord) != 2
                or not all(isinstance(value, int) for value in coord)
            ):
                raise WorkbenchError(
                    ErrorCode.E_SCHEMA,
                    "cell coordinate must be [x, y]",
                    field="map.costs",
                )
            x, y = coord
            with self._lock:
                scene = self._scene
                grid_map = scene.map
            if not grid_map.in_bounds(coord):
                raise WorkbenchError(
                    ErrorCode.E_RANGE,
                    "cell coordinate is outside map bounds",
                    field="map.costs",
                    coord=coord,
                )
            if not isinstance(cost, int) or isinstance(cost, bool):
                raise WorkbenchError(
                    ErrorCode.E_SCHEMA,
                    "cell cost must be an integer",
                    field=f"map.costs[{y}][{x}]",
                    coord=coord,
                )
            if cost < -1 or cost > MAX_CELL_COST:
                raise WorkbenchError(
                    ErrorCode.E_RANGE,
                    f"cell cost must be -1 or in [0, {MAX_CELL_COST}]",
                    field=f"map.costs[{y}][{x}]",
                    coord=coord,
                )
            old_cost = grid_map.cost_at(coord)
            old_terrain = grid_map.terrain_at(coord)
            if old_cost == cost:
                with self._lock:
                    cached_paths = tuple(sorted(self._raw_paths))
                    cached_traces = tuple(sorted(self._traces))
                    failed_ids = tuple(
                        member_id
                        for member_id, state in sorted(self._states.items())
                        if state.status == "failed"
                    )
                return InvalidationManifest(
                    revision=self.revision,
                    edit={"kind": "cell_cost", "coord": list(coord), "old": old_cost, "new": cost},
                    changed_cells=(),
                    direct_path_members=(),
                    path_recomputed=(),
                    smooth_recomputed=(),
                    simulation_recomputed=(),
                    reused_paths=cached_paths,
                    reused_simulations=cached_traces,
                    invalidated_intervals=(),
                    reused_intervals=tuple(
                        interval
                        for member_id in _sorted_members(scene)
                        for interval in self._intervals.get(member_id, ())
                    ),
                    reasons=tuple(
                        {
                            "member_id": member_id,
                            "scope": "cache",
                            "reason": "previous_failure_requires_retry",
                            "cache_reused": False,
                        }
                        for member_id in failed_ids
                    ),
                    elapsed_ms=0.0,
                    status="partial" if failed_ids else "no-op",
                )
            new_map = grid_map.with_cost(coord, cost)
            new_scene = scene.with_map(new_map)
            opening = old_cost < 0 or cost < old_cost
            direct = self._direct_path_members({coord}, opening=opening)
            edit = {
                "kind": "cell_cost",
                "coord": list(coord),
                "old": old_cost,
                "new": cost,
                "old_terrain": old_terrain,
                "new_terrain": new_map.terrain_at(coord),
            }
            return self._run_incremental(new_scene, direct, edit, {coord})

    def apply_blockade(self, blockade_id: str, active: bool) -> InvalidationManifest:
        with self._operation_lock:
            with self._lock:
                scene = self._scene
            blockade = next(
                (item for item in scene.map.blockades if item.blockade_id == blockade_id),
                None,
            )
            if blockade is None:
                raise WorkbenchError(
                    ErrorCode.E_REF,
                    f"blockade reference '{blockade_id}' does not exist",
                    field="map.blockades",
                )
            if blockade.active == active:
                with self._lock:
                    cached_paths = tuple(sorted(self._raw_paths))
                    cached_traces = tuple(sorted(self._traces))
                    failed_ids = tuple(
                        member_id
                        for member_id, state in sorted(self._states.items())
                        if state.status == "failed"
                    )
                return InvalidationManifest(
                    revision=self.revision,
                    edit={
                        "kind": "blockade",
                        "blockade_id": blockade_id,
                        "old": blockade.active,
                        "new": active,
                    },
                    changed_cells=(),
                    direct_path_members=(),
                    path_recomputed=(),
                    smooth_recomputed=(),
                    simulation_recomputed=(),
                    reused_paths=cached_paths,
                    reused_simulations=cached_traces,
                    invalidated_intervals=(),
                    reused_intervals=tuple(
                        interval
                        for member_id in _sorted_members(scene)
                        for interval in self._intervals.get(member_id, ())
                    ),
                    reasons=tuple(
                        {
                            "member_id": member_id,
                            "scope": "cache",
                            "reason": "previous_failure_requires_retry",
                            "cache_reused": False,
                        }
                        for member_id in failed_ids
                    ),
                    elapsed_ms=0.0,
                    status="partial" if failed_ids else "no-op",
                )
            new_map = scene.map.with_blockade_active(blockade_id, active)
            new_scene = scene.with_map(new_map)
            changed = set(blockade.cells)
            current_direct = set(self._direct_path_members(changed))
            if not active:
                current_direct.update(
                    self._direct_path_members(changed, opening=True)
                )
            with self._lock:
                historical = set(self._blockade_history.get(blockade_id, set()))
            affected = current_direct | historical
            direct = tuple(
                member_id
                for member_id in scene.formation.topo_order()
                if member_id in affected
            )
            with self._lock:
                self._blockade_history.setdefault(blockade_id, set()).update(affected)
            return self._run_incremental(
                new_scene,
                direct,
                {
                    "kind": "blockade",
                    "blockade_id": blockade_id,
                    "old": blockade.active,
                    "new": active,
                },
                changed,
            )

    def _run_incremental(
        self,
        new_scene: Scene,
        direct: tuple[str, ...],
        edit: dict[str, Any],
        changed_cells: set[Coord],
    ) -> InvalidationManifest:
        started = time.perf_counter()
        with self._lock:
            old_raw = dict(self._raw_paths)
            old_smoothed = dict(self._smoothed_paths)
            old_traces = dict(self._traces)
            old_intervals = dict(self._intervals)
            old_revision = self._revision
        closure = new_scene.formation.closure(set(direct))
        closure_set = set(closure)
        states = {
            member_id: UnitState(
                member_id=member_id, revision=old_revision + 1
            )
            for member_id in _sorted_members(new_scene)
        }

        if not direct:
            with self._lock:
                previous_states = {
                    key: copy.copy(value) for key, value in self._states.items()
                }
            invalidated_intervals: tuple[SimulationInterval, ...] = ()
            reused_intervals = tuple(
                interval
                for member_id in _sorted_members(new_scene)
                for interval in old_intervals.get(member_id, ())
            )
            with self._lock:
                self._revision += 1
                revision = self._revision
                self._scene = new_scene
                for state in states.values():
                    previous = previous_states.get(state.member_id)
                    state.path_cached = state.member_id in old_raw
                    state.smooth_cached = state.member_id in old_smoothed
                    state.simulation_cached = state.member_id in old_traces
                    state.path_cache_hit = state.path_cached
                    state.smooth_cache_hit = state.smooth_cached
                    state.simulation_cache_hit = state.simulation_cached
                    if previous is not None:
                        state.path_error = previous.path_error
                        state.smooth_error = previous.smooth_error
                        state.simulation_error = previous.simulation_error
                self._states = states
            manifest = InvalidationManifest(
                revision=revision,
                edit=edit,
                changed_cells=tuple(sorted(changed_cells)),
                direct_path_members=(),
                path_recomputed=(),
                smooth_recomputed=(),
                simulation_recomputed=(),
                reused_paths=tuple(sorted(old_raw)),
                reused_simulations=tuple(sorted(old_traces)),
                invalidated_intervals=invalidated_intervals,
                reused_intervals=reused_intervals,
                reasons=tuple(
                    {
                        "member_id": member_id,
                        "scope": "cache",
                        "reason": "previous_failure_requires_retry",
                        "cache_reused": False,
                    }
                    for member_id, state in sorted(states.items())
                    if state.status == "failed"
                ),
                elapsed_ms=(time.perf_counter() - started) * 1000.0,
                status=(
                    "partial"
                    if any(state.status == "failed" for state in states.values())
                    else "ok"
                ),
            )
            with self._lock:
                self._last_manifest = manifest
            if self._checkpoint_store is not None:
                payload = self.save_checkpoint()
                if payload.get("saved"):
                    with self._lock:
                        self._last_checkpoint_digest = payload.get("digest")
            return manifest

        def compute_path(member_id: str) -> tuple[RawPath, float]:
            begin = time.perf_counter()
            path = plan_member(new_scene, member_id)
            return path, (time.perf_counter() - begin) * 1000.0

        ordered_direct = tuple(
            member_id for member_id in new_scene.formation.topo_order() if member_id in direct
        )
        path_outcomes = self._executor.run_ordered(ordered_direct, compute_path)
        raw_paths = dict(old_raw)
        path_recomputed: list[str] = []
        failures: list[dict[str, Any]] = []
        for outcome in path_outcomes:
            state = states[outcome.item]
            if outcome.error is not None:
                raw_paths.pop(outcome.item, None)
                state.path_error = _error_dict(outcome.error)
                failures.append({"member_id": outcome.item, "stage": "path", **_error_dict(outcome.error)})
                continue
            path, path_ms = outcome.value  # type: ignore[misc]
            raw_paths[outcome.item] = path
            state.path_cached = True
            state.path_ms = path_ms
            path_recomputed.append(outcome.item)
        for member_id in closure:
            state = states[member_id]
            if member_id not in direct and member_id in raw_paths:
                state.path_cached = True
                state.path_cache_hit = True

        smoothed_paths = dict(old_smoothed)
        smooth_recomputed: list[str] = []
        for member_id in closure:
            smoothed_paths.pop(member_id, None)
        for member_id in new_scene.formation.topo_order():
            if member_id not in closure_set:
                state = states[member_id]
                if member_id in old_smoothed:
                    state.smooth_cached = True
                    state.smooth_cache_hit = True
                continue
            state = states[member_id]
            raw_path = raw_paths.get(member_id)
            if raw_path is None:
                state.smooth_error = {
                    "code": ErrorCode.E_STATE.value,
                    "message": "raw path unavailable; smoothing skipped",
                }
                failures.append(
                    {
                        "member_id": member_id,
                        "stage": "smooth",
                        **state.smooth_error,
                    }
                )
                continue
            parent_id = new_scene.formation.parents.get(member_id)
            if parent_id is not None and parent_id not in smoothed_paths:
                state.smooth_error = {
                    "code": ErrorCode.E_STATE.value,
                    "message": f"parent '{parent_id}' smoothing unavailable",
                }
                failures.append(
                    {
                        "member_id": member_id,
                        "stage": "smooth",
                        **state.smooth_error,
                    }
                )
                continue
            begin = time.perf_counter()
            try:
                smoothed_paths[member_id] = smooth_member(
                    new_scene,
                    raw_path,
                    smoothed_paths.get(parent_id) if parent_id is not None else None,
                )
                state.smooth_cached = True
                state.smooth_ms = (time.perf_counter() - begin) * 1000.0
                smooth_recomputed.append(member_id)
            except BaseException as exc:
                smoothed_paths.pop(member_id, None)
                state.smooth_error = _error_dict(exc)
                failures.append(
                    {
                        "member_id": member_id,
                        "stage": "smooth",
                        **_error_dict(exc),
                    }
                )

        seed_traces = dict(old_traces)
        for member_id in closure:
            seed_traces.pop(member_id, None)
        traces, trace_errors, trace_ms = self._run_simulation_waves(
            new_scene,
            smoothed_paths,
            seed_traces,
            set(closure_set),
            parallel=True,
        )
        simulation_recomputed: list[str] = []
        for member_id, error in trace_errors.items():
            states[member_id].simulation_error = error
            failures.append(
                {
                    "member_id": member_id,
                    "stage": "simulation",
                    **error,
                }
            )
        for member_id, simulation_ms in trace_ms.items():
            states[member_id].simulation_cached = True
            states[member_id].simulation_ms = simulation_ms
            simulation_recomputed.append(member_id)
        for member_id in _sorted_members(new_scene):
            state = states[member_id]
            if member_id not in closure_set:
                if member_id in old_traces:
                    state.simulation_cached = True
                    state.simulation_cache_hit = True
                if member_id in old_raw:
                    state.path_cached = True
                    state.path_cache_hit = True
                if member_id in old_smoothed:
                    state.smooth_cached = True
                    state.smooth_cache_hit = True

        intervals = dict(old_intervals)
        for member_id in closure:
            intervals.pop(member_id, None)
        for member_id in simulation_recomputed:
            intervals[member_id] = split_intervals(
                traces[member_id], new_scene.simulation.interval_ticks
            )
        invalidated_intervals = tuple(
            interval
            for member_id in sorted(closure_set)
            for interval in old_intervals.get(member_id, ())
        )
        reused_intervals = tuple(
            interval
            for member_id in _sorted_members(new_scene)
            if member_id not in closure_set
            for interval in old_intervals.get(member_id, ())
        )
        reasons: list[dict[str, Any]] = []
        for member_id in sorted(closure_set):
            if member_id in direct:
                reasons.append(
                    {
                        "member_id": member_id,
                        "scope": "path",
                        "reason": "path_traverses_changed_cell",
                        "cache_reused": False,
                    }
                )
            else:
                reasons.append(
                    {
                        "member_id": member_id,
                        "scope": "smooth_and_simulation",
                        "reason": "formation_descendant_coupling",
                        "cache_reused": member_id not in closure_set,
                    }
                )
        with self._lock:
            self._revision += 1
            revision = self._revision
            self._scene = new_scene
            self._raw_paths = raw_paths
            self._smoothed_paths = smoothed_paths
            self._traces = traces
            self._intervals = intervals
            self._states = states
        status = "partial" if failures else "ok"
        manifest = InvalidationManifest(
            revision=revision,
            edit=edit,
            changed_cells=tuple(sorted(changed_cells)),
            direct_path_members=direct,
            path_recomputed=tuple(sorted(path_recomputed)),
            smooth_recomputed=tuple(sorted(smooth_recomputed)),
            simulation_recomputed=tuple(sorted(simulation_recomputed)),
            reused_paths=tuple(
                member_id
                for member_id in _sorted_members(new_scene)
                if member_id not in direct and member_id in raw_paths
            ),
            reused_simulations=tuple(
                member_id
                for member_id in _sorted_members(new_scene)
                if member_id not in closure_set and member_id in traces
            ),
            invalidated_intervals=invalidated_intervals,
            reused_intervals=reused_intervals,
            reasons=tuple(reasons),
            elapsed_ms=(time.perf_counter() - started) * 1000.0,
            status=status,
            failures=tuple(failures),
        )
        with self._lock:
            self._last_manifest = manifest
        if self._checkpoint_store is not None and not failures:
            payload = self.save_checkpoint()
            if payload.get("saved"):
                with self._lock:
                    self._last_checkpoint_digest = payload.get("digest")
        return manifest

    def retry_member(self, member_id: str) -> InvalidationManifest:
        with self._operation_lock:
            with self._lock:
                scene = self._scene
                raw_paths = dict(self._raw_paths)
                smoothed = dict(self._smoothed_paths)
            if member_id not in _sorted_members(scene):
                raise WorkbenchError(
                    ErrorCode.E_REF,
                    f"member reference '{member_id}' does not exist",
                    field="members.member_id",
                    member_id=member_id,
                )
            direct = {member_id}
            current = scene.formation.parents.get(member_id)
            while current is not None:
                if current not in raw_paths or current not in smoothed:
                    direct.add(current)
                current = scene.formation.parents.get(current)
            return self._run_incremental(
                scene,
                tuple(
                    item
                    for item in scene.formation.topo_order()
                    if item in direct
                ),
                {"kind": "retry", "member_id": member_id},
                set(),
            )

    def retry_failed(self) -> InvalidationManifest:
        with self._lock:
            failed = {
                member_id
                for member_id, state in self._states.items()
                if state.status == "failed"
            }
            scene = self._scene
        if not failed:
            return InvalidationManifest(
                revision=self.revision,
                edit={"kind": "retry_failed"},
                changed_cells=(),
                direct_path_members=(),
                path_recomputed=(),
                smooth_recomputed=(),
                simulation_recomputed=(),
                reused_paths=_sorted_members(scene),
                reused_simulations=_sorted_members(scene),
                invalidated_intervals=(),
                reused_intervals=(),
                reasons=(),
                elapsed_ms=0.0,
                status="no-op",
            )
        return self._run_incremental(
            scene,
            tuple(item for item in scene.formation.topo_order() if item in failed),
            {"kind": "retry_failed", "members": sorted(failed)},
            set(),
        )

    def _checkpoint_payload(self) -> dict[str, Any]:
        with self._lock:
            scene = self._scene
            revision = self._revision
            commit_order = [
                member_id
                for member_id in _sorted_members(scene)
                if member_id in self._smoothed_paths and member_id in self._traces
            ]
            members: dict[str, Any] = {}
            for member_id in commit_order:
                path = self._smoothed_paths[member_id]
                trace = self._traces[member_id]
                members[member_id] = {
                    "path": path.to_dict(),
                    "trace": trace.to_dict(),
                    "intervals": [
                        item.to_dict()
                        for item in self._intervals.get(member_id, ())
                    ],
                }
            complete = len(commit_order) == len(_sorted_members(scene))
            return {
                "format_version": CHECKPOINT_FORMAT_VERSION,
                "schema_version": SCHEMA_VERSION,
                "revision": revision,
                "complete": complete,
                "commit_order": commit_order,
                "versions": {
                    "algorithm": ALGORITHM_VERSION,
                    "fixed_point": FIXED_POINT_VERSION,
                    "formation": scene.formation.version,
                    "formation_hash": scene.formation.version_hash(),
                    "search_hash": scene.search.version_hash(),
                    "simulation_hash": scene.simulation.version_hash(),
                },
                "map_hash": scene.map.content_hash(),
                "scene_hash": scene.content_hash(),
                "members": members,
                "dependency_graph": self.dependency_graph_payload(),
                "blockade_history": {
                    blockade_id: sorted(member_ids)
                    for blockade_id, member_ids in sorted(self._blockade_history.items())
                },
            }

    def save_checkpoint(self) -> dict[str, Any]:
        if self._checkpoint_store is None:
            return {"saved": False, "reason": "no checkpoint path configured"}
        payload = self._checkpoint_payload()
        if not payload["complete"]:
            return {
                "saved": False,
                "reason": "incomplete member caches are not persisted as a recoverable snapshot",
            }
        try:
            digest = self._checkpoint_store.save(payload)
        except OSError as exc:
            raise WorkbenchError(
                ErrorCode.E_IO,
                f"checkpoint write failed: {exc}",
                field="checkpoint",
            ) from exc
        with self._lock:
            self._last_checkpoint_digest = digest
        return {
            "saved": True,
            "digest": digest,
            "path": str(self._checkpoint_store.path),
            "commit_order": payload["commit_order"],
        }

    def try_restore_checkpoint(self) -> RestoreResult:
        if self._checkpoint_store is None:
            result = RestoreResult(False, "no checkpoint path configured", full_rebuild_required=True)
            self._restore_result = result
            return result
        read_result: CheckpointRead = self._checkpoint_store.read()
        if not read_result.ok or read_result.payload is None:
            result = RestoreResult(
                False,
                read_result.reason or "checkpoint unavailable",
                read_result.digest,
                full_rebuild_required=True,
            )
            self._restore_result = result
            return result
        payload = read_result.payload
        with self._lock:
            scene = self._scene
        expected_versions = {
            "algorithm": ALGORITHM_VERSION,
            "fixed_point": FIXED_POINT_VERSION,
            "formation": scene.formation.version,
            "formation_hash": scene.formation.version_hash(),
            "search_hash": scene.search.version_hash(),
            "simulation_hash": scene.simulation.version_hash(),
        }
        mismatch: str | None = None
        if payload.get("format_version") != CHECKPOINT_FORMAT_VERSION:
            mismatch = "checkpoint format version mismatch"
        elif payload.get("schema_version") != SCHEMA_VERSION:
            mismatch = "scene schema version mismatch"
        elif payload.get("map_hash") != scene.map.content_hash():
            mismatch = "map content hash mismatch"
        elif payload.get("scene_hash") != scene.content_hash():
            mismatch = "scene content hash mismatch"
        elif payload.get("versions") != expected_versions:
            mismatch = "formation, search, simulation, fixed-point or algorithm version mismatch"
        elif not payload.get("complete"):
            mismatch = "checkpoint is incomplete"
        elif payload.get("commit_order") != list(_sorted_members(scene)):
            mismatch = "stable member commit order mismatch"
        if mismatch is not None:
            result = RestoreResult(
                False,
                mismatch,
                read_result.digest,
                full_rebuild_required=True,
            )
            self._restore_result = result
            return result

        raw_paths: dict[str, RawPath] = {}
        smoothed_paths: dict[str, SmoothedPath] = {}
        traces: dict[str, MemberTrace] = {}
        intervals: dict[str, tuple[SimulationInterval, ...]] = {}
        try:
            members_payload = payload.get("members")
            if not isinstance(members_payload, dict):
                raise ValueError("members must be an object")
            for member_id in payload["commit_order"]:
                item = members_payload[member_id]
                path = SmoothedPath.from_dict(item["path"])
                trace = MemberTrace.from_dict(item["trace"])
                if path.member_id != member_id or trace.member_id != member_id:
                    raise ValueError(f"member id mismatch for {member_id}")
                interval_list = tuple(
                    SimulationInterval.from_dict(entry) for entry in item["intervals"]
                )
                if any(entry.member_id != member_id for entry in interval_list):
                    raise ValueError(f"interval member mismatch for {member_id}")
                for entry in interval_list:
                    if entry.trace_digest != interval_digest(
                        trace, entry.tick_start, entry.tick_end
                    ):
                        raise ValueError(
                            f"interval digest mismatch for {member_id} "
                            f"[{entry.tick_start}, {entry.tick_end}]"
                        )
                raw_paths[member_id] = path.raw
                smoothed_paths[member_id] = path
                traces[member_id] = trace
                intervals[member_id] = interval_list
        except (KeyError, TypeError, ValueError) as exc:
            result = RestoreResult(
                False,
                f"checkpoint payload is invalid: {exc}",
                read_result.digest,
                full_rebuild_required=True,
            )
            self._restore_result = result
            return result

        states = {
            member_id: UnitState(
                member_id=member_id,
                path_cached=True,
                smooth_cached=True,
                simulation_cached=True,
                path_cache_hit=True,
                smooth_cache_hit=True,
                simulation_cache_hit=True,
                revision=int(payload.get("revision", 0)),
            )
            for member_id in _sorted_members(scene)
        }
        with self._lock:
            self._revision = int(payload.get("revision", 0))
            self._raw_paths = raw_paths
            self._smoothed_paths = smoothed_paths
            self._traces = traces
            self._intervals = intervals
            self._states = states
            raw_history = payload.get("blockade_history", {})
            self._blockade_history = {
                str(blockade_id): {str(member_id) for member_id in member_ids}
                for blockade_id, member_ids in raw_history.items()
            }
            self._last_checkpoint_digest = read_result.digest
            self._restore_result = RestoreResult(
                True, "checkpoint restored", read_result.digest
            )
            return self._restore_result

    def load_or_rebuild(self) -> dict[str, Any]:
        result = self.try_restore_checkpoint()
        if result.restored:
            return {"restored": True, "restore": result.to_dict()}
        build = self.build_all(save_checkpoint=True)
        return {
            "restored": False,
            "restore": result.to_dict(),
            "full_rebuild": build,
        }

    def compare_with_full(self, *, parallel: bool = True) -> ComparisonResult:
        started = time.perf_counter()
        with self._lock:
            scene = self._scene
            incremental_raw = dict(self._raw_paths)
            incremental_smoothed = dict(self._smoothed_paths)
            incremental_traces = dict(self._traces)
        full_engine = FormationEngine(
            scene,
            max_workers=self._max_workers,
            per_worker_buffer=self._per_worker_buffer,
        )
        build = full_engine.build_all(parallel=parallel, save_checkpoint=False)
        if not build["complete"]:
            raise WorkbenchError(
                ErrorCode.E_DIVERGE,
                "full rebuild did not complete; cannot compare incrementally",
                item="full_rebuild",
                left="complete incremental cache",
                right=build,
            )
        with full_engine._lock:
            full_raw = dict(full_engine._raw_paths)
            full_smoothed = dict(full_engine._smoothed_paths)
            full_traces = dict(full_engine._traces)

        member_ids = _sorted_members(scene)
        path_cells = 0
        waypoint_count = 0
        checked_ticks = 0
        for member_id in member_ids:
            left_raw = incremental_raw.get(member_id)
            right_raw = full_raw.get(member_id)
            if left_raw is None or right_raw is None:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "cached path availability differs",
                    item="path_presence",
                    member_id=member_id,
                    left=left_raw is not None,
                    right=right_raw is not None,
                )
            if left_raw.cost != right_raw.cost:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "path accumulated cost differs",
                    item="path_cost",
                    member_id=member_id,
                    left=left_raw.cost,
                    right=right_raw.cost,
                )
            for index in range(max(len(left_raw.cells), len(right_raw.cells))):
                left_cell = left_raw.cells[index] if index < len(left_raw.cells) else None
                right_cell = right_raw.cells[index] if index < len(right_raw.cells) else None
                if left_cell != right_cell:
                    raise WorkbenchError(
                        ErrorCode.E_DIVERGE,
                        "first path cell divergence",
                        item="path_cells",
                        member_id=member_id,
                        coord=left_cell if left_cell is not None else right_cell,
                        left=list(left_cell) if left_cell is not None else None,
                        right=list(right_cell) if right_cell is not None else None,
                    )
            path_cells += len(left_raw.cells)

            left_path = incremental_smoothed.get(member_id)
            right_path = full_smoothed.get(member_id)
            if left_path is None or right_path is None:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "smoothed path availability differs",
                    item="smooth_presence",
                    member_id=member_id,
                    left=left_path is not None,
                    right=right_path is not None,
                )
            if left_path.cost != right_path.cost or left_path.baseline_cost != right_path.baseline_cost:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "smoothed cost baseline differs",
                    item="smooth_cost",
                    member_id=member_id,
                    left=[left_path.baseline_cost, left_path.cost],
                    right=[right_path.baseline_cost, right_path.cost],
                )
            for index in range(max(len(left_path.waypoints), len(right_path.waypoints))):
                left_wp = left_path.waypoints[index] if index < len(left_path.waypoints) else None
                right_wp = right_path.waypoints[index] if index < len(right_path.waypoints) else None
                if left_wp != right_wp:
                    raise WorkbenchError(
                        ErrorCode.E_DIVERGE,
                        "first waypoint divergence",
                        item="waypoints",
                        member_id=member_id,
                        coord=(
                            (left_wp.grid if left_wp is not None else right_wp.grid)
                            if (left_wp is not None or right_wp is not None)
                            else None
                        ),
                        left=left_wp.to_dict() if left_wp is not None else None,
                        right=right_wp.to_dict() if right_wp is not None else None,
                    )
            waypoint_count += len(left_path.waypoints)

            left_trace = incremental_traces.get(member_id)
            right_trace = full_traces.get(member_id)
            if left_trace is None or right_trace is None:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "simulation trace availability differs",
                    item="trace_presence",
                    member_id=member_id,
                    left=left_trace is not None,
                    right=right_trace is not None,
                )
            if left_trace.completed_tick != right_trace.completed_tick:
                raise WorkbenchError(
                    ErrorCode.E_DIVERGE,
                    "completion tick differs",
                    item="completed_tick",
                    member_id=member_id,
                    tick=min(left_trace.completed_tick, right_trace.completed_tick),
                    left=left_trace.completed_tick,
                    right=right_trace.completed_tick,
                )
            for tick_index in range(max(len(left_trace.ticks), len(right_trace.ticks))):
                left_tick = (
                    left_trace.ticks[tick_index]
                    if tick_index < len(left_trace.ticks)
                    else None
                )
                right_tick = (
                    right_trace.ticks[tick_index]
                    if tick_index < len(right_trace.ticks)
                    else None
                )
                if left_tick != right_tick:
                    field_names = (
                        "position",
                        "heading",
                        "segment_index",
                        "slot_deviation",
                        "parent_spacing",
                        "slowdown_reason",
                        "completed",
                    )
                    differing = "tick_record"
                    left_value: Any = left_tick.to_dict() if left_tick is not None else None
                    right_value: Any = right_tick.to_dict() if right_tick is not None else None
                    if left_tick is not None and right_tick is not None:
                        for field_name in field_names:
                            if getattr(left_tick, field_name) != getattr(right_tick, field_name):
                                differing = field_name
                                left_value = getattr(left_tick, field_name)
                                right_value = getattr(right_tick, field_name)
                                break
                    raise WorkbenchError(
                        ErrorCode.E_DIVERGE,
                        f"first tick divergence in {differing}",
                        item=differing,
                        member_id=member_id,
                        tick=tick_index,
                        coord=(
                            left_tick.position
                            if left_tick is not None
                            else right_tick.position if right_tick is not None else None
                        ),
                        left=left_value,
                        right=right_value,
                    )
            checked_ticks += len(left_trace.ticks)
        return ComparisonResult(
            ok=True,
            checked_members=member_ids,
            checked_ticks=checked_ticks,
            path_cells=path_cells,
            waypoints=waypoint_count,
            elapsed_ms=(time.perf_counter() - started) * 1000.0,
        )

    def export_payload(self) -> dict[str, Any]:
        with self._lock:
            scene = self._scene
            states = {
                member_id: state.to_dict()
                for member_id, state in sorted(self._states.items())
            }
            paths = {
                member_id: path.to_dict()
                for member_id, path in sorted(self._smoothed_paths.items())
            }
            traces = {
                member_id: trace.to_dict()
                for member_id, trace in sorted(self._traces.items())
            }
            manifest = (
                self._last_manifest.to_dict()
                if self._last_manifest is not None
                else None
            )
            return {
                "scene": {
                    "name": scene.name,
                    "seed": scene.seed,
                    "content_hash": scene.content_hash(),
                    "map_hash": scene.map.content_hash(),
                    "formation_version": scene.formation.version,
                },
                "revision": self._revision,
                "fixed_point": FIXED_POINT_VERSION,
                "algorithm": ALGORITHM_VERSION,
                "states": states,
                "paths": paths,
                "traces": traces,
                "dependency_graph": self.dependency_graph_payload(),
                "last_invalidation": manifest,
                "last_build_ms": round(self._last_build_ms, 3),
            }

    def export_json(self, path: str | Path) -> Path:
        destination = Path(path)
        destination.parent.mkdir(parents=True, exist_ok=True)
        payload = self.export_payload()
        canonical = json.dumps(
            payload, sort_keys=True, indent=2, ensure_ascii=True
        )
        temp = destination.with_suffix(destination.suffix + ".tmp")
        temp.write_text(canonical + "\n", encoding="utf-8")
        temp.replace(destination)
        return destination
