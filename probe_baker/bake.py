"""Transactional dense/sparse baking and dependency-aware incremental rebakes."""

from __future__ import annotations

import hashlib
import math
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable

import numpy as np

from .canonical import freeze_array, hash_arrays, hash_object
from .errors import BakeCancelled, ErrorCode, ProbeError
from .grid import GridBuildResult, ProbeGrid, build_probe_grid
from .interpolation import (
    AnchorStructure,
    apply_occlusion_compensation,
    interpolate_from_stencil,
    lattice_anchor_structure,
    occlusion_compensation_blend,
)
from .sampling import occluder_hit_masks, sample_probes, sky_radiance
from .scene import Occluder, Scene
from .sh import ProjectionResult, project_radiance, sh_coefficient_count
from .strategy import (
    STRATEGY_DENSE,
    STRATEGY_SPARSE,
    BakeConfig,
    StrategyDecision,
    choose_strategy,
)


ProgressCallback = Callable[[dict[str, Any]], None]
CheckpointCallback = Callable[[int, int], None]


def _hash_probe(
    raw_coefficient: np.ndarray,
    final_coefficient: np.ndarray,
    truncation_error: float,
    visibility_mean: float,
    sampled: bool,
    factor_version: str,
) -> str:
    digest = hashlib.sha256()
    for array in (raw_coefficient, final_coefficient):
        contiguous = np.ascontiguousarray(array)
        digest.update(str(contiguous.dtype).encode("ascii"))
        digest.update(str(contiguous.shape).encode("ascii"))
        digest.update(contiguous.tobytes())
    digest.update(np.asarray([truncation_error, visibility_mean], dtype="<f8").tobytes())
    digest.update(b"\x01" if sampled else b"\x00")
    digest.update(factor_version.encode("utf-8"))
    return digest.hexdigest()


def _factor_context(scene: Scene, config: BakeConfig, grid: ProbeGrid) -> dict[str, Any]:
    config_factors = config.factor_hashes()
    return {
        "grid": grid.structure_hash,
        "material": hash_object(scene.material.to_dict()),
        "sky": hash_object(scene.sky.to_dict()),
        "sh_order": config_factors["sh_order"],
        "directions": config_factors["directions"],
        "occluders": scene.occluder_hashes(),
    }


def _probe_factor_version(
    context: dict[str, Any],
    hit_ids: frozenset[str],
    compensation_strength: float | None,
) -> str:
    relevant = {
        occluder_id: context["occluders"][occluder_id]
        for occluder_id in sorted(hit_ids)
        if occluder_id in context["occluders"]
    }
    return hash_object(
        {
            "grid": context["grid"],
            "material": context["material"],
            "sky": context["sky"],
            "sh_order": context["sh_order"],
            "directions": context["directions"],
            "occluders": relevant,
            "compensation_strength": compensation_strength,
        }
    )


def _hit_ids(hits: np.ndarray, occluder_ids: tuple[str, ...], probe_id: int) -> frozenset[str]:
    return frozenset(
        occluder_ids[index]
        for index in np.flatnonzero(hits[int(probe_id)])
    )


@dataclass(frozen=True)
class BakeArtifacts:
    raw_coefficients: np.ndarray
    coefficients: np.ndarray
    truncation_error: np.ndarray
    visibility_mean: np.ndarray
    sampled_mask: np.ndarray
    occluder_ids: tuple[str, ...]
    occluder_hits: np.ndarray
    factor_versions: tuple[str, ...]
    probe_hashes: tuple[str, ...]
    content_hash: str

    @property
    def probe_count(self) -> int:
        return int(self.coefficients.shape[0])


@dataclass(frozen=True)
class BakeState:
    scene: Scene
    config: BakeConfig
    grid: ProbeGrid
    decision: StrategyDecision
    artifacts: BakeArtifacts

    @property
    def content_hash(self) -> str:
        return self.artifacts.content_hash


@dataclass(frozen=True)
class InvalidationPlan:
    reasons: tuple[str, ...]
    direct_affected: np.ndarray
    resampled: np.ndarray
    interpolated: np.ndarray
    recomputed: np.ndarray
    dependency_closure: np.ndarray
    conservative: bool

    def __post_init__(self) -> None:
        for name in (
            "direct_affected",
            "resampled",
            "interpolated",
            "recomputed",
            "dependency_closure",
        ):
            value = np.asarray(getattr(self, name), dtype=np.int64)
            value.setflags(write=False)
            object.__setattr__(self, name, value)

    def to_dict(self) -> dict[str, Any]:
        return {
            "reasons": list(self.reasons),
            "direct_affected_count": int(self.direct_affected.size),
            "resampled_count": int(self.resampled.size),
            "interpolated_count": int(self.interpolated.size),
            "recomputed_count": int(self.recomputed.size),
            "dependency_closure_count": int(self.dependency_closure.size),
            "conservative": self.conservative,
            "direct_affected_probes": self.direct_affected.tolist(),
            "resampled_probes": self.resampled.tolist(),
            "interpolated_probes": self.interpolated.tolist(),
            "recomputed_probes": self.recomputed.tolist(),
        }


@dataclass(frozen=True)
class BakeOutcome:
    state: BakeState
    grid_result: GridBuildResult
    elapsed_seconds: float
    mean_truncation_error: float
    actual_error_upper_bound: float
    plan: InvalidationPlan | None = None
    error_before: float | None = None

    def to_dict(self, include_probe_lists: bool = False) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "scene_id": self.state.scene.scene_id,
            "probe_count": self.state.grid.count,
            "neighbor_edge_count": self.state.grid.edge_count,
            "grid_structure_hash": self.state.grid.structure_hash,
            "grid_rebuild": self.grid_result.to_dict(),
            "strategy": self.state.decision.to_dict(),
            "elapsed_seconds": self.elapsed_seconds,
            "mean_truncation_error": self.mean_truncation_error,
            "actual_error_upper_bound": self.actual_error_upper_bound,
            "content_hash": self.state.content_hash,
        }
        if self.plan is not None:
            plan = self.plan.to_dict()
            if not include_probe_lists:
                plan.pop("direct_affected_probes", None)
                plan.pop("resampled_probes", None)
                plan.pop("interpolated_probes", None)
                plan.pop("recomputed_probes", None)
            payload["invalidation"] = plan
        if self.error_before is not None:
            payload["error_before"] = self.error_before
            payload["error_delta"] = self.mean_truncation_error - self.error_before
        return payload


def _readonly(values: np.ndarray) -> np.ndarray:
    result = np.asarray(values)
    result.setflags(write=False)
    return result


def _unique_sorted(values: Iterable[int]) -> np.ndarray:
    array = np.asarray(sorted(set(int(value) for value in values)), dtype=np.int64)
    array.setflags(write=False)
    return array


def _neighbor_closure(
    grid: ProbeGrid,
    seeds: np.ndarray,
    active_mask: np.ndarray,
) -> np.ndarray:
    if seeds.size == 0:
        return _unique_sorted(())
    values: set[int] = set(int(value) for value in seeds)
    for seed in seeds:
        if active_mask[int(seed)]:
            values.update(int(value) for value in grid.adjacency[int(seed)] if active_mask[int(value)])
    return _unique_sorted(values)


class BakeEngine:
    """Pure state transitions: a failed or cancelled bake never mutates a published store."""

    def __init__(self, trace: Any | None = None) -> None:
        self.trace = trace

    def _checkpoint(
        self,
        index: int,
        total: int,
        cancel_event: threading.Event | None,
        progress: ProgressCallback | None,
    ) -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise BakeCancelled()
        if progress is not None:
            progress({"stage": "sampling", "chunk": index, "chunks": total})

    def _sample_compact(
        self,
        ids: np.ndarray,
        grid: ProbeGrid,
        scene: Scene,
        config: BakeConfig,
        directions: np.ndarray,
        cancel_event: threading.Event | None,
        progress: ProgressCallback | None,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        coefficient_count = sh_coefficient_count(config.sh_order)
        raw = np.zeros((ids.size, coefficient_count, 3), dtype=np.float64)
        truncation = np.zeros(ids.size, dtype=np.float64)
        visibility_mean = np.zeros(ids.size, dtype=np.float64)
        hits = np.zeros((ids.size, len(scene.occluders)), dtype=bool)
        active = scene.active_occluders()
        active_indices = [
            index
            for index, occluder in enumerate(scene.occluders)
            if occluder.occluder_id not in scene.dynamic_mask
        ]
        total = max(1, int(math.ceil(ids.size / config.chunk_size)))
        for chunk_index, start in enumerate(range(0, ids.size, config.chunk_size)):
            self._checkpoint(chunk_index, total, cancel_event, progress)
            stop = min(start + config.chunk_size, ids.size)
            local_ids = ids[start:stop]
            sample = sample_probes(
                grid.centers[local_ids],
                directions,
                scene,
                chunk_size=max(1, local_ids.size),
            )
            projection = project_radiance(
                sample.radiance, directions, config.sh_order, sample.visibility
            )
            raw[start:stop] = projection.coefficients
            truncation[start:stop] = projection.truncation_error
            visibility_mean[start:stop] = projection.visibility_mean
            for local_active_index, global_index in enumerate(active_indices):
                hits[start:stop, global_index] = sample.occluder_hits[:, local_active_index]
        return raw, truncation, visibility_mean, hits

    def _full_sample(
        self,
        grid: ProbeGrid,
        scene: Scene,
        config: BakeConfig,
        decision: StrategyDecision,
        directions: np.ndarray,
        cancel_event: threading.Event | None,
        progress: ProgressCallback | None,
    ) -> tuple[
        np.ndarray,
        np.ndarray,
        np.ndarray,
        np.ndarray,
        np.ndarray,
        AnchorStructure | None,
    ]:
        coefficient_count = sh_coefficient_count(config.sh_order)
        raw = np.zeros((grid.count, coefficient_count, 3), dtype=np.float64)
        truncation = np.zeros(grid.count, dtype=np.float64)
        visibility_mean = np.zeros(grid.count, dtype=np.float64)
        hits = np.zeros((grid.count, len(scene.occluders)), dtype=bool)
        sampled_mask = np.zeros(grid.count, dtype=bool)
        anchor: AnchorStructure | None = None
        if decision.strategy == STRATEGY_DENSE:
            sampled_ids = np.arange(grid.count, dtype=np.int64)
        else:
            anchor = lattice_anchor_structure(grid, config.sparse_anchor_stride)
            sampled_ids = anchor.anchor_ids
        compact_raw, compact_truncation, compact_visibility, compact_hits = self._sample_compact(
            sampled_ids,
            grid,
            scene,
            config,
            directions,
            cancel_event,
            progress,
        )
        raw[sampled_ids] = compact_raw
        truncation[sampled_ids] = compact_truncation
        visibility_mean[sampled_ids] = compact_visibility
        hits[sampled_ids] = compact_hits
        sampled_mask[sampled_ids] = True

        if anchor is not None and anchor.unsampled_ids.size:
            unsampled = anchor.unsampled_ids
            raw[unsampled] = interpolate_from_stencil(
                raw, anchor.stencil_probe_ids, anchor.stencil_weights, unsampled
            )
            visibility_mean[unsampled] = interpolate_from_stencil(
                visibility_mean,
                anchor.stencil_probe_ids,
                anchor.stencil_weights,
                unsampled,
            )
            truncation[unsampled] = interpolate_from_stencil(
                truncation,
                anchor.stencil_probe_ids,
                anchor.stencil_weights,
                unsampled,
            )
            stencil_ids = anchor.stencil_probe_ids[unsampled]
            hits[unsampled] = hits[stencil_ids].any(axis=1)
            if progress is not None:
                progress(
                    {
                        "stage": "sparse_interpolation",
                        "probes": int(unsampled.size),
                    }
                )
        return raw, truncation, visibility_mean, hits, sampled_mask, anchor

    def _assemble(
        self,
        scene: Scene,
        config: BakeConfig,
        grid: ProbeGrid,
        raw: np.ndarray,
        coefficients: np.ndarray,
        truncation: np.ndarray,
        visibility: np.ndarray,
        hits: np.ndarray,
        sampled_mask: np.ndarray,
        rehash_ids: np.ndarray,
        previous: BakeArtifacts | None = None,
    ) -> BakeArtifacts:
        context = _factor_context(scene, config, grid)
        occluder_ids = tuple(occluder.occluder_id for occluder in scene.occluders)
        factors = (
            [""] * grid.count
            if previous is None
            else list(previous.factor_versions)
        )
        hashes = (
            [""] * grid.count
            if previous is None
            else list(previous.probe_hashes)
        )
        for probe_id in rehash_ids:
            probe_id = int(probe_id)
            probe_blend = occlusion_compensation_blend(
                np.asarray([visibility[probe_id]], dtype=np.float64),
                config.compensation_strength,
            )[0]
            compensation_strength = (
                float(config.compensation_strength) if probe_blend > 1e-15 else None
            )
            factor = _probe_factor_version(
                context,
                _hit_ids(hits, occluder_ids, probe_id),
                compensation_strength,
            )
            factors[probe_id] = factor
            hashes[probe_id] = _hash_probe(
                raw[probe_id],
                coefficients[probe_id],
                float(truncation[probe_id]),
                float(visibility[probe_id]),
                bool(sampled_mask[probe_id]),
                factor,
            )
        missing = [index for index, value in enumerate(hashes) if not value]
        if missing:
            raise ProbeError(
                ErrorCode.INTERP_FAILED,
                "incremental cache has probes without a content hash",
                probe_id=missing[0],
            )
        content_hash = hash_object(
            {
                "schema": "probe-artifacts-v1",
                "grid": grid.structure_hash,
                "scene_factors": scene.factor_hashes(),
                "sh_order": config.sh_order,
                "directions": config.factor_hashes()["directions"],
                "strategy": config.strategy,
                "anchor_stride": config.sparse_anchor_stride,
                "probe_hashes": hashes,
            }
        )
        return BakeArtifacts(
            raw_coefficients=freeze_array(raw),
            coefficients=freeze_array(coefficients),
            truncation_error=freeze_array(truncation),
            visibility_mean=freeze_array(visibility),
            sampled_mask=freeze_array(sampled_mask),
            occluder_ids=occluder_ids,
            occluder_hits=freeze_array(hits),
            factor_versions=tuple(factors),
            probe_hashes=tuple(hashes),
            content_hash=content_hash,
        )

    def _finish_initial(
        self,
        scene: Scene,
        config: BakeConfig,
        grid: ProbeGrid,
        raw: np.ndarray,
        truncation: np.ndarray,
        visibility: np.ndarray,
        hits: np.ndarray,
        sampled_mask: np.ndarray,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        coefficients, blend, _ = apply_occlusion_compensation(
            raw, visibility, grid, config.compensation_strength
        )
        rehash = np.arange(grid.count, dtype=np.int64)
        return coefficients, blend, rehash

    def bake(
        self,
        scene: Scene,
        config: BakeConfig,
        *,
        previous: BakeState | None = None,
        cancel_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
    ) -> BakeOutcome:
        started = time.perf_counter()
        config.validate()
        directions = config.resolved_directions()
        spacing = config.resolved_spacing(scene)
        previous_grid = previous.grid if previous is not None else None
        grid_result = build_probe_grid(scene, spacing, previous_grid)
        decision = choose_strategy(
            grid_result.grid.count,
            config.sh_order,
            directions.shape[0],
            config.strategy,
            config.sparse_anchor_stride,
        )
        if self.trace is not None:
            self.trace.event(
                "grid_built",
                **grid_result.to_dict(),
                spacing=spacing,
            )
            self.trace.event("strategy_selected", **decision.to_dict())
        if previous is None:
            raw, truncation, visibility, hits, sampled_mask, _ = self._full_sample(
                grid_result.grid,
                scene,
                config,
                decision,
                directions,
                cancel_event,
                progress,
            )
            coefficients, _, rehash = self._finish_initial(
                scene,
                config,
                grid_result.grid,
                raw,
                truncation,
                visibility,
                hits,
                sampled_mask,
            )
            artifacts = self._assemble(
                scene,
                config,
                grid_result.grid,
                raw,
                coefficients,
                truncation,
                visibility,
                hits,
                sampled_mask,
                rehash,
            )
            mean_error = float(np.mean(truncation))
            plan = None
            error_before = None
        else:
            (
                artifacts,
                truncation,
                plan,
                error_before,
            ) = self._incremental(
                previous,
                scene,
                config,
                grid_result,
                decision,
                directions,
                cancel_event,
                progress,
            )
            mean_error = float(np.mean(truncation))
        state = BakeState(
            scene=scene,
            config=config,
            grid=grid_result.grid,
            decision=decision,
            artifacts=artifacts,
        )
        elapsed = time.perf_counter() - started
        actual_bound = min(
            1.0,
            float(decision.estimated_error_upper_bound) + mean_error,
        )
        if self.trace is not None:
            self.trace.event(
                "bake_completed",
                probe_count=grid_result.grid.count,
                neighbor_edges=grid_result.grid.edge_count,
                strategy=decision.strategy,
                elapsed_seconds=elapsed,
                mean_truncation_error=mean_error,
                content_hash=artifacts.content_hash,
                incremental=plan is not None,
                invalidation=None if plan is None else plan.to_dict(),
            )
        return BakeOutcome(
            state=state,
            grid_result=grid_result,
            elapsed_seconds=elapsed,
            mean_truncation_error=mean_error,
            actual_error_upper_bound=actual_bound,
            plan=plan,
            error_before=error_before,
        )

    def resume_from_recovery(
        self,
        report: Any,
        scene: Scene,
        config: BakeConfig,
        *,
        cancel_event: threading.Event | None = None,
        progress: ProgressCallback | None = None,
    ) -> BakeOutcome:
        """Finish a crash-interrupted snapshot by sampling only pending probes.

        Compensation neighbours of pending probes are recomputed as a dependency
        closure. Committed probe samples and their coefficients are never resampled.
        """

        started = time.perf_counter()
        config.validate()
        directions = config.resolved_directions()
        if report.arrays is None:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "recovery report has no probe arrays")
        grid_result = build_probe_grid(scene, config.resolved_spacing(scene))
        if grid_result.structure_hash != report.begin["grid_structure_hash"]:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                "current scene grid does not match the interrupted snapshot",
            )
        if int(report.begin["sh_order"]) != config.sh_order:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                "current SH order does not match the interrupted snapshot",
            )
        if hash_arrays([("directions", directions)]) != hash_arrays(
            [("directions", np.asarray(report.begin["directions"], dtype=np.float64))]
        ):
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                "current direction set does not match the interrupted snapshot",
            )
        decision = choose_strategy(
            grid_result.grid.count,
            config.sh_order,
            directions.shape[0],
            config.strategy,
            config.sparse_anchor_stride,
        )
        raw = np.array(report.arrays["raw_coefficients"], copy=True)
        coefficients = np.array(report.arrays["coefficients"], copy=True)
        truncation = np.array(report.arrays["truncation_error"], copy=True)
        visibility = np.array(report.arrays["visibility_mean"], copy=True)
        sampled_mask = np.array(report.arrays["sampled_mask"], copy=True)
        hits = np.array(report.arrays["occluder_hits"], copy=True)
        pending = np.asarray(report.pending_probe_ids, dtype=np.int64)
        if self.trace is not None:
            self.trace.event(
                "recovery_resume_started",
                snapshot_version=int(report.snapshot_version),
                committed_count=int(report.committed_count),
                pending_count=int(report.pending_count),
                pending_probe_ids=pending.tolist(),
                clean_exit=bool(report.clean_exit),
            )
        if pending.size:
            compact_raw, compact_truncation, compact_visibility, compact_hits = self._sample_compact(
                pending,
                grid_result.grid,
                scene,
                config,
                directions,
                cancel_event,
                progress,
            )
            raw[pending] = compact_raw
            truncation[pending] = compact_truncation
            visibility[pending] = compact_visibility
            hits[pending] = compact_hits
            sampled_mask[pending] = True

        comp_active = (
            occlusion_compensation_blend(visibility, config.compensation_strength)
            > 1e-15
        )
        target = _neighbor_closure(grid_result.grid, pending, comp_active)
        if target.size:
            neighbour_coeffs = _idw_for_ids(raw, grid_result.grid, target)
            blend = occlusion_compensation_blend(
                visibility[target], config.compensation_strength
            )
            coefficients[target] = (
                raw[target] * (1.0 - blend[:, None, None])
                + neighbour_coeffs * blend[:, None, None]
            )
        artifacts = self._assemble(
            scene,
            config,
            grid_result.grid,
            raw,
            coefficients,
            truncation,
            visibility,
            hits,
            sampled_mask,
            np.arange(grid_result.grid.count, dtype=np.int64),
        )
        plan = InvalidationPlan(
            reasons=("forced_termination_recovery",),
            direct_affected=pending,
            resampled=pending,
            interpolated=np.empty(0, dtype=np.int64),
            recomputed=target,
            dependency_closure=target,
            conservative=True,
        )
        elapsed = time.perf_counter() - started
        if self.trace is not None:
            self.trace.event(
                "recovery_resume_completed",
                sampled_probe_ids=pending.tolist(),
                dependency_recomputed_ids=target.tolist(),
                elapsed_seconds=elapsed,
                content_hash=artifacts.content_hash,
            )
        return BakeOutcome(
            state=BakeState(
                scene=scene,
                config=config,
                grid=grid_result.grid,
                decision=decision,
                artifacts=artifacts,
            ),
            grid_result=grid_result,
            elapsed_seconds=elapsed,
            mean_truncation_error=float(np.mean(truncation)),
            actual_error_upper_bound=min(
                1.0, decision.estimated_error_upper_bound + float(np.mean(truncation))
            ),
            plan=plan,
            error_before=None,
        )

    def _incremental(
        self,
        previous: BakeState,
        scene: Scene,
        config: BakeConfig,
        grid_result: GridBuildResult,
        decision: StrategyDecision,
        directions: np.ndarray,
        cancel_event: threading.Event | None,
        progress: ProgressCallback | None,
    ) -> tuple[BakeArtifacts, np.ndarray, InvalidationPlan, float]:
        old_artifacts = previous.artifacts
        grid = grid_result.grid
        reasons: list[str] = []
        all_ids = np.arange(grid.count, dtype=np.int64)

        grid_changed = grid_result.structure_hash != previous.grid.structure_hash
        old_config_factors = previous.config.factor_hashes()
        new_config_factors = config.factor_hashes()
        old_scene_factors = previous.scene.factor_hashes()
        new_scene_factors = scene.factor_hashes()
        global_reasons: list[str] = []
        if grid_changed:
            global_reasons.append("grid_structure")
        for factor in ("material", "sky"):
            if old_scene_factors[factor] != new_scene_factors[factor]:
                global_reasons.append(factor)
        if old_config_factors["sh_order"] != new_config_factors["sh_order"]:
            global_reasons.append("sh_order")
        if old_config_factors["directions"] != new_config_factors["directions"]:
            global_reasons.append("directions")
        if (
            previous.decision.strategy != decision.strategy
            or previous.config.sparse_anchor_stride != config.sparse_anchor_stride
        ):
            global_reasons.append("sampling_strategy")

        old_occluders = {item.occluder_id: item for item in previous.scene.occluders}
        new_occluders = {item.occluder_id: item for item in scene.occluders}
        old_hashes = previous.scene.occluder_hashes()
        new_hashes = scene.occluder_hashes()
        changed_occluders = sorted(
            occluder_id
            for occluder_id in set(old_hashes) | set(new_hashes)
            if old_hashes.get(occluder_id) != new_hashes.get(occluder_id)
        )

        if global_reasons:
            reasons.extend(global_reasons)
            direct = all_ids
        elif changed_occluders:
            reasons.append("occluders:" + ",".join(changed_occluders))
            affected: set[int] = set()
            for occluder_id in changed_occluders:
                old_index = (
                    old_artifacts.occluder_ids.index(occluder_id)
                    if occluder_id in old_artifacts.occluder_ids
                    else None
                )
                if old_index is not None:
                    affected.update(
                        np.flatnonzero(old_artifacts.occluder_hits[:, old_index]).tolist()
                    )
                new_occluder = new_occluders.get(occluder_id)
                if new_occluder is not None and occluder_id not in scene.dynamic_mask:
                    new_hits = occluder_hit_masks(
                        grid.centers, directions, (new_occluder,)
                    )[:, 0]
                    affected.update(np.flatnonzero(new_hits).tolist())
            direct = _unique_sorted(affected)
        else:
            direct = np.empty(0, dtype=np.int64)

        compensation_direct = np.empty(0, dtype=np.int64)
        if previous.config.compensation_strength != config.compensation_strength:
            reasons.append("compensation_strength")
            old_active = (
                occlusion_compensation_blend(
                    old_artifacts.visibility_mean,
                    previous.config.compensation_strength,
                )
                > 1e-15
            )
            new_active = (
                occlusion_compensation_blend(
                    old_artifacts.visibility_mean,
                    config.compensation_strength,
                )
                > 1e-15
            )
            compensation_direct = _unique_sorted(
                np.flatnonzero(old_active | new_active).tolist()
            )

        if global_reasons:
            raw, truncation, visibility, hits, sampled_mask, _ = self._full_sample(
                grid,
                scene,
                config,
                decision,
                directions,
                cancel_event,
                progress,
            )
            coefficients, _, _ = self._finish_initial(
                scene, config, grid, raw, truncation, visibility, hits, sampled_mask
            )
            artifacts = self._assemble(
                scene,
                config,
                grid,
                raw,
                coefficients,
                truncation,
                visibility,
                hits,
                sampled_mask,
                all_ids,
            )
            closure = all_ids
            plan = InvalidationPlan(
                reasons=tuple(reasons),
                direct_affected=all_ids,
                resampled=all_ids if decision.strategy == STRATEGY_DENSE else _unique_sorted(
                    lattice_anchor_structure(grid, config.sparse_anchor_stride).anchor_ids.tolist()
                ),
                interpolated=(
                    np.empty(0, dtype=np.int64)
                    if decision.strategy == STRATEGY_DENSE
                    else lattice_anchor_structure(
                        grid, config.sparse_anchor_stride
                    ).unsampled_ids
                ),
                recomputed=all_ids,
                dependency_closure=closure,
                conservative=True,
            )
            return artifacts, truncation, plan, float(np.mean(old_artifacts.truncation_error))

        raw = np.array(old_artifacts.raw_coefficients, copy=True)
        truncation = np.array(old_artifacts.truncation_error, copy=True)
        visibility = np.array(old_artifacts.visibility_mean, copy=True)
        hits = np.array(old_artifacts.occluder_hits, copy=True)
        sampled_mask = np.array(old_artifacts.sampled_mask, copy=True)
        anchor: AnchorStructure | None = None
        if decision.strategy == STRATEGY_SPARSE:
            anchor = lattice_anchor_structure(grid, config.sparse_anchor_stride)
            anchor_set = set(int(value) for value in anchor.anchor_ids)
            resample_set = set(int(value) for value in direct if int(value) in anchor_set)
            for probe_id in direct:
                if int(probe_id) not in anchor_set:
                    resample_set.update(
                        int(value)
                        for value in anchor.stencil_probe_ids[int(probe_id)]
                    )
            resampled = _unique_sorted(resample_set)
            if resampled.size:
                compact_raw, compact_truncation, compact_visibility, compact_hits = self._sample_compact(
                    resampled,
                    grid,
                    scene,
                    config,
                    directions,
                    cancel_event,
                    progress,
                )
                raw[resampled] = compact_raw
                truncation[resampled] = compact_truncation
                visibility[resampled] = compact_visibility
                hits[resampled] = compact_hits
            resampled_set = set(int(value) for value in resampled)
            affected_unsampled = [
                int(probe_id)
                for probe_id in anchor.unsampled_ids
                if any(
                    int(anchor_id) in resampled_set
                    for anchor_id in anchor.stencil_probe_ids[int(probe_id)]
                )
            ]
            extra_unsampled = [
                int(probe_id)
                for probe_id in direct
                if int(probe_id) not in anchor_set
            ]
            interpolated = _unique_sorted(
                set(affected_unsampled) | set(extra_unsampled)
            )
            if interpolated.size:
                raw[interpolated] = interpolate_from_stencil(
                    raw,
                    anchor.stencil_probe_ids,
                    anchor.stencil_weights,
                    interpolated,
                )
                truncation[interpolated] = interpolate_from_stencil(
                    truncation,
                    anchor.stencil_probe_ids,
                    anchor.stencil_weights,
                    interpolated,
                )
                visibility[interpolated] = interpolate_from_stencil(
                    visibility,
                    anchor.stencil_probe_ids,
                    anchor.stencil_weights,
                    interpolated,
                )
                stencil_ids = anchor.stencil_probe_ids[interpolated]
                hits[interpolated] = hits[stencil_ids].any(axis=1)
            raw_changed = _unique_sorted(
                set(int(value) for value in resampled)
                | set(int(value) for value in interpolated)
            )
        else:
            resampled = _unique_sorted(direct)
            interpolated = np.empty(0, dtype=np.int64)
            if resampled.size:
                compact_raw, compact_truncation, compact_visibility, compact_hits = self._sample_compact(
                    resampled,
                    grid,
                    scene,
                    config,
                    directions,
                    cancel_event,
                    progress,
                )
                raw[resampled] = compact_raw
                truncation[resampled] = compact_truncation
                visibility[resampled] = compact_visibility
                hits[resampled] = compact_hits
            raw_changed = resampled

        coefficients = np.array(old_artifacts.coefficients, copy=True)
        new_comp_active = (
            occlusion_compensation_blend(visibility, config.compensation_strength)
            > 1e-15
        )
        if raw_changed.size:
            neighbor_coeffs = _idw_for_ids(raw, grid, raw_changed)
            blend = occlusion_compensation_blend(
                visibility[raw_changed], config.compensation_strength
            )
            coefficients[raw_changed] = (
                raw[raw_changed] * (1.0 - blend[:, None, None])
                + neighbor_coeffs * blend[:, None, None]
            )
        if compensation_direct.size:
            targeted = _unique_sorted(
                set(int(value) for value in compensation_direct)
                | set(int(value) for value in raw_changed)
            )
            if targeted.size:
                neighbor_coeffs = _idw_for_ids(raw, grid, targeted)
                blend = occlusion_compensation_blend(
                    visibility[targeted], config.compensation_strength
                )
                coefficients[targeted] = (
                    raw[targeted] * (1.0 - blend[:, None, None])
                    + neighbor_coeffs * blend[:, None, None]
                )

        rehash_set = set(int(value) for value in raw_changed)
        rehash_set.update(int(value) for value in compensation_direct)
        if rehash_set:
            rehash = _unique_sorted(rehash_set)
            dependent = _neighbor_closure(grid, rehash, new_comp_active)
            rehash_set.update(int(value) for value in dependent)
        rehash = _unique_sorted(rehash_set)
        if rehash.size:
            targeted = rehash[
                new_comp_active[rehash]
            ]
            if targeted.size:
                neighbor_coeffs = _idw_for_ids(raw, grid, targeted)
                blend = occlusion_compensation_blend(
                    visibility[targeted], config.compensation_strength
                )
                coefficients[targeted] = (
                    raw[targeted] * (1.0 - blend[:, None, None])
                    + neighbor_coeffs * blend[:, None, None]
                )

        artifacts = self._assemble(
            scene,
            config,
            grid,
            raw,
            coefficients,
            truncation,
            visibility,
            hits,
            sampled_mask,
            rehash,
            previous=old_artifacts,
        )
        plan = InvalidationPlan(
            reasons=tuple(reasons) if reasons else ("no_effective_change",),
            direct_affected=direct,
            resampled=resampled,
            interpolated=interpolated,
            recomputed=rehash,
            dependency_closure=_neighbor_closure(
                grid, rehash, new_comp_active
            ),
            conservative=bool(global_reasons),
        )
        if self.trace is not None:
            self.trace.event(
                "invalidation_planned",
                **plan.to_dict(),
            )
        return artifacts, truncation, plan, float(np.mean(old_artifacts.truncation_error))


def _idw_for_ids(coeffs: np.ndarray, grid: ProbeGrid, ids: np.ndarray) -> np.ndarray:
    from .interpolation import inverse_distance_neighbour_coefficients

    return inverse_distance_neighbour_coefficients(coeffs, grid, ids)

