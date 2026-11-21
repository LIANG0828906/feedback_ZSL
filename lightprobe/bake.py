from __future__ import annotations

import os
import time
import uuid
from collections import defaultdict, deque
from dataclasses import dataclass, replace
from pathlib import Path
from threading import Event
from typing import Any, Callable, Mapping

import numpy as np

from .bake_types import (
    BakeConfig,
    BakeReport,
    InfluenceReport,
    StrategyPlan,
)
from .directions import DirectionSet, make_directions
from .errors import ProbeError, interp_failed, recovery_invalid
from .geometry import occlusion_digest
from .grid import ProbeGrid, build_grid, grid_change
from .influence import common_factor_version, expected_influence_version, record_reusable
from .planner import plan_incremental
from .record import ProbeRecord
from .sampling import (
    AuditProbe,
    SampledProbe,
    audit_probe,
    leak_score_from_audit,
    sample_probe,
)
from .scene import Scene
from .sh import coefficient_count, evaluate_irradiance, negativity_probe_normals, radiance_to_irradiance
from .snapshot import RecoveredSnapshot, SnapshotWriter
from .state import BakeState, factor_versions
from .strategy import (
    InterpolationIndex,
    choose_strategy,
    interpolate_coefficients,
)


@dataclass(frozen=True)
class BakeResult:
    state: BakeState
    report: BakeReport


class Baker:
    """Deterministic local baker with incremental reuse and snapshot recovery."""

    def bake(
        self,
        scene: Scene,
        config: BakeConfig | None = None,
        *,
        base_state: BakeState | None = None,
        recovered: RecoveredSnapshot | None = None,
        snapshot_dir: str | Path | None = None,
        target_version: int | None = None,
        run_id: str | None = None,
        cancel_event: Event | None = None,
        crash_after_chunks: int | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> BakeResult:
        started = time.monotonic()
        config = (config or BakeConfig.from_scene(scene)).validated()
        self._check_cancel(cancel_event)
        grid = build_grid(scene, config.spacing)
        direction_set = make_directions(config.direction_count, config.direction_seed)
        audit_directions = make_directions(
            config.audit_direction_count, config.direction_seed + 15485863
        )
        change = grid_change(base_state.grid if base_state is not None else None, grid)
        plan, interpolation = choose_strategy(scene, grid, direction_set, config)
        self._check_cancel(cancel_event)
        versions = factor_versions(
            scene, grid, direction_set, audit_directions, config, plan.name
        )

        if recovered is not None:
            target_version = target_version or int(recovered.metadata["target_version"])
            clean_records, dirty_ids, influence_report = self._recover_state(
                recovered,
                scene=scene,
                config=config,
                grid=grid,
                plan=plan,
                directions=direction_set,
                audit_directions=audit_directions,
                versions=versions,
            )
        else:
            target_version = target_version or 1
            influence_report = plan_incremental(
                base_state,
                scene=scene,
                grid=grid,
                directions=direction_set,
                audit_directions=audit_directions,
                config=config,
                strategy=plan.name,
            )
            clean_records = {}
            if base_state is not None:
                clean_records = {
                    probe_id: base_state.records[probe_id]
                    for probe_id in grid.probe_ids
                    if probe_id not in set(influence_report.affected_probe_ids)
                }
            dirty_ids = [
                probe_id
                for probe_id in grid.probe_ids
                if probe_id not in clean_records
            ]
            dirty_ids, extra_affected, extra_reasons = self._apply_reuse_safety_check(
                dirty_ids,
                clean_records,
                scene=scene,
                config=config,
                grid=grid,
                directions=direction_set,
                audit_directions=audit_directions,
                plan=plan,
            )
            if extra_affected:
                influence_report = InfluenceReport(
                    changed_factors=tuple(
                        dict.fromkeys(
                            tuple(influence_report.changed_factors)
                            + ("reuse_proof",)
                        )
                    ),
                    seed_probe_ids=tuple(
                        dict.fromkeys(
                            tuple(influence_report.seed_probe_ids)
                            + tuple(extra_affected)
                        )
                    ),
                    affected_probe_ids=tuple(
                        dict.fromkeys(
                            tuple(influence_report.affected_probe_ids)
                            + tuple(extra_affected)
                        )
                    ),
                    reasons={**influence_report.reasons, **extra_reasons},
                    conservative=True,
                    conservative_reason=(
                        "one or more probes failed the version+content-hash reuse proof"
                    ),
                )

        progress and progress(
            {
                "phase": "planned",
                "probe_count": grid.probe_count,
                "reused": len(clean_records),
                "recomputed": len(dirty_ids),
                "strategy": plan.name,
            }
        )
        self._check_cancel(cancel_event)
        records, max_deviation, deviation_probe_count, max_truncation = self._bake_records(
            scene=scene,
            config=config,
            grid=grid,
            directions=direction_set,
            audit_directions=audit_directions,
            plan=plan,
            interpolation=interpolation,
            clean_records=clean_records,
            dirty_ids=dirty_ids,
            base_state=base_state,
            cancel_event=cancel_event,
            progress=progress,
        )
        state = BakeState(
            scene=scene,
            grid=grid,
            directions=direction_set,
            audit_directions=audit_directions,
            config=config,
            strategy=plan,
            interpolation=interpolation,
            records=records,
            factor_versions=versions,
        )
        self._check_cancel(cancel_event)

        snapshot_report: dict[str, Any] = {}
        if snapshot_dir is not None:
            snapshot_report = self._write_snapshot(
                snapshot_dir,
                state=state,
                plan=plan,
                target_version=target_version,
                run_id=run_id,
                cancel_event=cancel_event,
                crash_after_chunks=crash_after_chunks,
            )

        report = BakeReport(
            scene_id=scene.scene_id,
            version=target_version,
            strategy=plan.name,
            probe_count=grid.probe_count,
            reused_count=len(clean_records),
            recomputed_count=len(dirty_ids),
            max_coefficient_deviation=max_deviation,
            coefficient_deviation_probe_count=deviation_probe_count,
            max_recomputed_truncation_error=max_truncation,
            grid_change=change.to_dict(),
            influence=influence_report.to_dict(),
            strategy_audit=plan.audit.to_dict(),
            grid_manifest={
                "probe_ids": list(grid.probe_ids),
                "coordinate_hashes": list(grid.coordinate_hashes),
            },
            snapshot=snapshot_report,
            elapsed_seconds=float(time.monotonic() - started),
        )
        return BakeResult(state=state, report=report)

    def _apply_reuse_safety_check(
        self,
        dirty_ids: list[str],
        clean_records: dict[str, ProbeRecord],
        *,
        scene: Scene,
        config: BakeConfig,
        grid: ProbeGrid,
        directions: DirectionSet,
        audit_directions: DirectionSet,
        plan: StrategyPlan,
    ) -> tuple[list[str], list[str], dict[str, str]]:
        original_dirty = set(dirty_ids)
        if not clean_records:
            return dirty_ids, [], {}
        dirty = set(dirty_ids)
        extra_reasons: dict[str, str] = {}
        changed = True
        while changed:
            changed = False
            for probe_id, record in clean_records.items():
                if probe_id in dirty:
                    continue
                reusable, reason = record_reusable(
                    record,
                    scene=scene,
                    grid=grid,
                    directions=directions,
                    audit_directions=audit_directions,
                    config=config,
                    strategy=plan.name,
                    records=clean_records,
                )
                if reusable:
                    continue
                dirty.add(probe_id)
                extra_reasons[probe_id] = f"conservative_reuse_rejected:{reason}"
                # Any probe whose final/raw data depends on this one must also be rebuilt.
                queue = deque([probe_id])
                reverse: dict[str, set[str]] = defaultdict(set)
                for other_id, other in clean_records.items():
                    dependencies = set(other.source_ids)
                    if other.leak_score > 1.0e-12:
                        dependencies |= set(other.comp_source_ids)
                    for dependency in dependencies:
                        reverse[dependency].add(other_id)
                while queue:
                    current = queue.popleft()
                    for dependent in reverse.get(current, ()):
                        if dependent not in dirty:
                            dirty.add(dependent)
                            extra_reasons[dependent] = (
                                f"dependency_of_rejected_probe:{current}"
                            )
                            queue.append(dependent)
                changed = True
        extra_affected = [
            probe_id
            for probe_id in grid.probe_ids
            if probe_id in dirty and probe_id not in original_dirty
        ]
        return (
            [probe_id for probe_id in grid.probe_ids if probe_id in dirty],
            extra_affected,
            extra_reasons,
        )

    def _recover_state(
        self,
        recovered: RecoveredSnapshot,
        *,
        scene: Scene,
        config: BakeConfig,
        grid: ProbeGrid,
        plan: StrategyPlan,
        directions: DirectionSet,
        audit_directions: DirectionSet,
        versions: Mapping[str, str],
    ) -> tuple[dict[str, ProbeRecord], list[str], InfluenceReport]:
        metadata = recovered.metadata
        if metadata.get("scene_id") != scene.scene_id:
            raise recovery_invalid(
                str(recovered.directory), "snapshot scene id does not match the requested scene"
            )
        if metadata.get("grid_hash") != grid.content_hash:
            raise recovery_invalid(
                str(recovered.directory), "snapshot grid hash does not match the current grid"
            )
        if metadata.get("strategy") != plan.name:
            raise recovery_invalid(
                str(recovered.directory), "snapshot strategy does not match the current strategy"
            )
        if metadata.get("config") != config.fingerprint():
            raise recovery_invalid(
                str(recovered.directory), "snapshot bake configuration does not match"
            )
        if int(metadata.get("probe_count", -1)) != grid.probe_count:
            raise recovery_invalid(
                str(recovered.directory),
                "snapshot probe_count does not match the current grid",
            )
        if metadata.get("factor_versions") != dict(versions):
            raise recovery_invalid(
                str(recovered.directory),
                "snapshot influence-factor versions do not match current versions",
            )
        expected_coefficients = coefficient_count(config.sh_order)
        for probe_id, record in recovered.records.items():
            if probe_id not in grid.index_by_id:
                raise recovery_invalid(
                    str(recovered.directory), f"snapshot contains unknown probe '{probe_id}'"
                )
            index = grid.index_by_id[probe_id]
            if record.coordinate_hash != grid.coordinate_hashes[index]:
                raise recovery_invalid(
                    str(recovered.directory),
                    f"snapshot coordinate hash mismatch for probe '{probe_id}'",
                )
            if record.coefficients.shape != (expected_coefficients, 3):
                raise recovery_invalid(
                    str(recovered.directory),
                    f"snapshot coefficient shape mismatch for probe '{probe_id}'",
                )
        missing = [
            probe_id for probe_id in grid.probe_ids if probe_id not in recovered.records
        ]
        report = InfluenceReport(
            changed_factors=("recovery",),
            seed_probe_ids=tuple(missing),
            affected_probe_ids=tuple(missing),
            reasons={probe_id: "uncommitted_snapshot_probe" for probe_id in missing},
            conservative=False,
            conservative_reason=None,
        )
        return dict(recovered.records), missing, report

    def _bake_records(
        self,
        *,
        scene: Scene,
        config: BakeConfig,
        grid: ProbeGrid,
        directions: DirectionSet,
        audit_directions: DirectionSet,
        plan: StrategyPlan,
        interpolation: InterpolationIndex,
        clean_records: Mapping[str, ProbeRecord],
        dirty_ids: list[str],
        base_state: BakeState | None,
        cancel_event: Event | None,
        progress: Callable[[dict[str, Any]], None] | None,
    ) -> tuple[dict[str, ProbeRecord], float, int, float]:
        records: dict[str, ProbeRecord] = dict(clean_records)
        dirty_set = set(dirty_ids)
        if dirty_ids:
            source_set = set(plan.source_indices)
            source_values: dict[int, np.ndarray] = {}
            source_truncations: dict[int, float] = {}
            source_raw_means: dict[int, tuple[float, float, float]] = {}
            for probe_id, record in clean_records.items():
                index = grid.index_by_id[probe_id]
                if index in source_set:
                    source_values[index] = record.raw_radiance_coefficients
                    source_truncations[index] = record.truncation_error
                    source_raw_means[index] = record.raw_mean_radiance

            audit_by_id: dict[str, AuditProbe] = {}
            direct_by_id: dict[str, SampledProbe] = {}
            for position, probe_id in enumerate(dirty_ids):
                if position % 33 == 0:
                    self._check_cancel(cancel_event)
                audit_by_id[probe_id] = audit_probe(
                    scene, grid.coordinates[grid.index_by_id[probe_id]], audit_directions.vectors
                )
                if plan.name == "dense" or grid.index_by_id[probe_id] in source_set:
                    sampled = sample_probe(
                        scene,
                        grid.coordinates[grid.index_by_id[probe_id]],
                        directions.vectors,
                        config.sh_order,
                    )
                    direct_by_id[probe_id] = sampled
                    index = grid.index_by_id[probe_id]
                    source_values[index] = sampled.radiance_coefficients
                    source_truncations[index] = sampled.truncation_error
                    source_raw_means[index] = sampled.raw_mean_radiance
                progress and progress(
                    {
                        "phase": "sampling",
                        "completed": position + 1,
                        "total": len(dirty_ids),
                    }
                )

            raw_by_index: dict[int, np.ndarray] = {}
            raw_mean_by_index: dict[int, tuple[float, float, float]] = {}
            truncation_by_index: dict[int, float] = {}
            source_ids_by_id: dict[str, tuple[str, ...]] = {}
            sampled_by_id: dict[str, bool] = {}
            for probe_id, record in clean_records.items():
                index = grid.index_by_id[probe_id]
                raw_by_index[index] = record.raw_radiance_coefficients
                raw_mean_by_index[index] = record.raw_mean_radiance
                truncation_by_index[index] = record.truncation_error
                source_ids_by_id[probe_id] = record.source_ids
                sampled_by_id[probe_id] = record.sampled

            for probe_id, sampled in direct_by_id.items():
                index = grid.index_by_id[probe_id]
                raw_by_index[index] = sampled.radiance_coefficients
                raw_mean_by_index[index] = sampled.raw_mean_radiance
                truncation_by_index[index] = sampled.truncation_error
                source_ids_by_id[probe_id] = (probe_id,)
                sampled_by_id[probe_id] = True

            if plan.name == "sparse":
                for probe_id in dirty_ids:
                    if probe_id in direct_by_id:
                        continue
                    index = grid.index_by_id[probe_id]
                    interpolated = interpolate_coefficients(
                        interpolation, index, source_values
                    )
                    weighted_mean = np.zeros(3, dtype=np.float64)
                    weighted_truncation = 0.0
                    source_ids: list[str] = []
                    trunc_values: list[float] = []
                    for source_index, weight in interpolation.target_weights[index]:
                        source_id = grid.probe_ids[source_index]
                        source_ids.append(source_id)
                        weighted_mean += np.asarray(source_raw_means[source_index]) * weight
                        weighted_truncation += source_truncations[source_index] * weight
                        trunc_values.append(source_truncations[source_index])
                    spread = (
                        max(trunc_values) - min(trunc_values) if trunc_values else 0.0
                    )
                    raw_by_index[index] = interpolated
                    raw_mean_by_index[index] = tuple(float(item) for item in weighted_mean)
                    truncation_by_index[index] = float(weighted_truncation + 0.25 * spread)
                    source_ids_by_id[probe_id] = tuple(source_ids)
                    sampled_by_id[probe_id] = False

            temp: dict[str, ProbeRecord] = {}
            negative_ids: list[str] = []
            normals = negativity_probe_normals()
            for probe_id in dirty_ids:
                index = grid.index_by_id[probe_id]
                audit = audit_by_id[probe_id]
                raw = raw_by_index[index]
                raw_mean = raw_mean_by_index[index]
                leak = leak_score_from_audit(
                    raw_mean,
                    audit.mean_radiance,
                    audit.visibility_fraction,
                    audit.sky_mean,
                )
                if sampled_by_id[probe_id]:
                    # Directly sampled probes already carry their own occlusion
                    # signal; leakage correction is only valid for interpolated data.
                    leak = 0.0
                raw_irradiance = radiance_to_irradiance(raw, config.sh_order)
                neighbor_ids = grid.neighbors[probe_id]
                if leak > 1.0e-12 and neighbor_ids:
                    neighbor_index = min(
                        (grid.index_by_id[item] for item in neighbor_ids),
                        key=lambda candidate: float(
                            np.mean(raw_mean_by_index[candidate])
                        ),
                    )
                    target = radiance_to_irradiance(
                        raw_by_index[neighbor_index], config.sh_order
                    )
                    blend = min(
                        1.0,
                        max(0.0, config.compensation_strength * leak * 0.85),
                    )
                    coefficients = raw_irradiance * (1.0 - blend) + target * blend
                else:
                    coefficients = raw_irradiance
                coefficients = np.asarray(coefficients, dtype=np.float64)
                coefficients.setflags(write=False)
                raw_copy = np.array(raw, dtype=np.float64, copy=True)
                raw_copy.setflags(write=False)
                evaluated = evaluate_irradiance(coefficients, normals, config.sh_order)
                if np.any(evaluated < -1.0e-7):
                    negative_ids.append(probe_id)
                temp[probe_id] = ProbeRecord(
                    probe_id=probe_id,
                    coordinate_hash=grid.coordinate_hashes[index],
                    raw_radiance_coefficients=raw_copy,
                    coefficients=coefficients,
                    visibility_fraction=audit.visibility_fraction,
                    leak_score=leak,
                    truncation_error=float(truncation_by_index[index]),
                    audit_mean_radiance=audit.mean_radiance,
                    raw_mean_radiance=raw_mean,
                    audit_sky_mean=audit.sky_mean,
                    first_hit_ids=audit.first_hit_ids,
                    first_hit_distances=audit.first_hit_distances,
                    source_ids=source_ids_by_id[probe_id],
                    comp_source_ids=tuple(neighbor_ids),
                    sampled=sampled_by_id[probe_id],
                )

            if negative_ids:
                raise interp_failed(
                    "compensated irradiance became negative at one or more probes",
                    negative_ids,
                )

            payload_records: dict[str, ProbeRecord] = {}
            for probe_id, record in records.items():
                payload_records[probe_id] = record
            for probe_id, record in temp.items():
                payload_records[probe_id] = replace(
                    record, payload_hash=record.compute_payload_hash()
                )
            common = common_factor_version(
                scene, grid, directions, audit_directions, config, plan.name
            )
            for probe_id, record in temp.items():
                influence_version = expected_influence_version(
                    record,
                    scene=scene,
                    grid=grid,
                    directions=directions,
                    audit_directions=audit_directions,
                    config=config,
                    strategy=plan.name,
                    records=payload_records,
                    common_version=common,
                )
                records[probe_id] = record.with_hashes(influence_version)

        max_deviation = 0.0
        deviation_probe_count = 0
        max_truncation = 0.0
        if base_state is not None:
            for probe_id in dirty_ids:
                max_truncation = max(
                    max_truncation, float(records[probe_id].truncation_error)
                )
                previous = base_state.records.get(probe_id)
                if previous is None:
                    continue
                if previous.coordinate_hash != records[probe_id].coordinate_hash:
                    continue
                deviation_probe_count += 1
                current = records[probe_id].coefficients
                old = previous.coefficients
                width = max(current.shape[0], old.shape[0])
                current_padded = np.zeros((width, 3), dtype=np.float64)
                old_padded = np.zeros((width, 3), dtype=np.float64)
                current_padded[: current.shape[0]] = current
                old_padded[: old.shape[0]] = old
                max_deviation = max(
                    max_deviation,
                    float(np.max(np.abs(current_padded - old_padded))),
                )
        elif dirty_ids:
            max_truncation = max(
                float(records[probe_id].truncation_error) for probe_id in dirty_ids
            )
        return records, max_deviation, deviation_probe_count, max_truncation

    def _write_snapshot(
        self,
        snapshot_dir: str | Path,
        *,
        state: BakeState,
        plan: StrategyPlan,
        target_version: int,
        run_id: str | None,
        cancel_event: Event | None,
        crash_after_chunks: int | None,
    ) -> dict[str, Any]:
        writer = _RecordingWriter(
            snapshot_dir,
            metadata={
                "format": "local-lightprobe-chunked-v1",
                "run_id": run_id or uuid.uuid4().hex,
                "scene_id": state.scene.scene_id,
                "target_version": target_version,
                "grid_hash": state.grid.content_hash,
                "factor_versions": dict(state.factor_versions),
                "config": state.config.fingerprint(),
                "strategy": plan.name,
                "chunk_size": state.config.chunk_size,
                "probe_count": state.grid.probe_count,
            },
            cancel_event=cancel_event,
            crash_after_chunks=crash_after_chunks,
        )
        source_ids = [state.grid.probe_ids[index] for index in plan.source_indices]
        source_set = set(source_ids)
        ordered_ids = source_ids + [
            probe_id for probe_id in state.grid.probe_ids if probe_id not in source_set
        ]
        report = writer.commit_all([state.records[probe_id] for probe_id in ordered_ids])
        report["complete"] = True
        return report

    @staticmethod
    def _check_cancel(cancel_event: Event | None) -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise ProbeError(
                "E_CANCELLED",
                "bake cancelled at a chunk checkpoint; the last published version remains readable",
            )


class _RecordingWriter(SnapshotWriter):
    def __init__(
        self,
        directory: str | Path,
        *,
        metadata: dict[str, Any],
        cancel_event: Event | None,
        crash_after_chunks: int | None,
    ) -> None:
        self._cancel_event = cancel_event
        self._crash_after_chunks = crash_after_chunks
        super().__init__(directory, metadata, checkpoint=self._checkpoint)

    def _checkpoint(self, chunk_number: int, _record_count: int) -> None:
        Baker._check_cancel(self._cancel_event)
        if (
            self._crash_after_chunks is not None
            and chunk_number >= self._crash_after_chunks
        ):
            os._exit(137)

