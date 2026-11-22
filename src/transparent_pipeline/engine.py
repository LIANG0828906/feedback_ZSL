"""Concurrent staged rendering engine with checkpoints and invalidation."""

from __future__ import annotations

import copy
import json
import math
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .checkpoint import CheckpointStore, Snapshot
from .dependencies import DependencyGraph
from .errors import PipelineError, parameter_error, schema_error
from .fixedpoint import LENGTH_SCALE, OPTICAL_SCALE
from .renderer import (
    ALGORITHM_VERSION,
    BlockResult,
    BlockWindow,
    ReferenceResult,
    RenderConfig,
    assemble_blocks,
    block_windows,
    conservative_preview_bound,
    render_block,
    render_stats,
)
from .resources import ResourceGovernor, ResourceLimits
from .scene import Scene, validate_scene_dict
from .state import BlockStateMachine, BlockStatus, STATUS_LABELS
from .util import sha256_json, write_json_atomic


class SimulatedCrash(RuntimeError):
    def __init__(self, committed: int) -> None:
        super().__init__(f"simulated crash after {committed} committed block(s)")
        self.committed = committed


@dataclass
class RenderOutcome:
    transmission: np.ndarray
    scattering: np.ndarray
    path_units: np.ndarray
    optical_units: np.ndarray
    hit_mask: np.ndarray
    stats: dict[str, Any]
    statuses: dict[int, str]
    status_labels: dict[int, str]
    progress: dict[str, int]
    commit_order: list[int]
    complete: bool
    failed_blocks: dict[int, str]
    mode: str
    actual_step: float
    preview_error_bound: float
    algorithm_version: str
    config_hash: str
    scene_hash: str
    media_versions: dict[str, int]
    light_versions: dict[str, int]
    media_dependencies: dict[int, tuple[str, ...]]
    light_dependencies: dict[int, tuple[str, ...]]
    dependency_graph: dict[str, Any]
    invalidation_manifest: list[dict[str, Any]]
    block_windows: list[BlockWindow]
    resource_stats: dict[str, int]
    resume: dict[str, Any]
    quantization: dict[str, Any]
    block_locks: dict[int, dict[str, Any]]
    paused: bool = False

    @property
    def progress_text(self) -> str:
        return f"{self.progress['committed']}/{self.progress['total']}"

    def to_dict(self, include_arrays: bool = False, include_heatmap: bool = False) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": self.complete and not self.failed_blocks,
            "complete": self.complete,
            "mode": self.mode,
            "paused": self.paused,
            "progress": self.progress,
            "progress_text": self.progress_text,
            "statuses": {str(key): value for key, value in sorted(self.statuses.items())},
            "status_labels": {str(key): value for key, value in sorted(self.status_labels.items())},
            "commit_order": self.commit_order,
            "failed_blocks": {str(key): value for key, value in sorted(self.failed_blocks.items())},
            "actual_step": self.actual_step,
            "preview_error_bound": self.preview_error_bound,
            "algorithm_version": self.algorithm_version,
            "config_hash": self.config_hash,
            "scene_hash": self.scene_hash,
            "media_versions": self.media_versions,
            "light_versions": self.light_versions,
            "media_dependencies": {str(key): list(value) for key, value in sorted(self.media_dependencies.items())},
            "light_dependencies": {str(key): list(value) for key, value in sorted(self.light_dependencies.items())},
            "dependency_graph": self.dependency_graph,
            "invalidation_manifest": self.invalidation_manifest,
            "resource_stats": self.resource_stats,
            "resume": self.resume,
            "quantization": self.quantization,
            "block_locks": {str(key): value for key, value in sorted(self.block_locks.items())},
            "stats": self.stats,
            "blocks": [
                {
                    "id": window.index,
                    "row": window.row,
                    "col": window.col,
                    "x0": window.x0,
                    "y0": window.y0,
                    "x1": window.x1,
                    "y1": window.y1,
                    "status": self.statuses.get(window.index, "pending_intersect"),
                }
                for window in self.block_windows
            ],
        }
        if include_arrays:
            result["transmission"] = self.transmission.tolist()
            result["scattering"] = self.scattering.tolist()
            result["path_units"] = self.path_units.tolist()
            result["optical_units"] = self.optical_units.tolist()
            result["hit_mask"] = self.hit_mask.tolist()
        if include_heatmap:
            from .renderer import render_heatmap_rgb

            result["transmission_heatmap"] = render_heatmap_rgb(self.transmission, self.hit_mask)
            result["scattering_heatmap"] = render_heatmap_rgb(self.scattering, self.hit_mask)
        return result

    def export(self, directory: str | Path, *, include_arrays: bool = True) -> dict[str, str]:
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True)
        json_path = directory / "result.json"
        write_json_atomic(json_path, self.to_dict(include_arrays=include_arrays, include_heatmap=True))
        npz_path = directory / "buffers.npz"
        temp = npz_path.with_name(f".{npz_path.name}.tmp")
        with temp.open("wb") as handle:
            np.savez_compressed(
                handle,
                transmission=self.transmission,
                scattering=self.scattering,
                path_units=self.path_units,
                optical_units=self.optical_units,
                hit_mask=self.hit_mask,
            )
            handle.flush()
            import os

            os.fsync(handle.fileno())
        temp.replace(npz_path)
        return {"json": str(json_path), "npz": str(npz_path)}


class RenderEngine:
    def __init__(
        self,
        scene: Scene,
        config: RenderConfig | None = None,
        *,
        checkpoint_directory: str | Path | None = None,
        limits: ResourceLimits | None = None,
        sample_index: int = 0,
    ) -> None:
        self.scene = scene.copy()
        self.config = config or RenderConfig()
        self.sample_index = int(sample_index)
        if not 0 <= self.sample_index < len(self.scene.camera["trajectory"]):
            raise parameter_error("camera.sample_index", "camera trajectory sample index is out of range")
        self.limits = limits or ResourceLimits()
        self.store = CheckpointStore(checkpoint_directory) if checkpoint_directory is not None else None
        self._windows = block_windows(
            int(self.scene.camera["width"]),
            int(self.scene.camera["height"]),
            self.config.block_width,
            self.config.block_height,
        )
        self._lock = threading.RLock()
        self._pause_event = threading.Event()
        self._states = BlockStateMachine([window.index for window in self._windows])
        self._results: dict[int, BlockResult] = {}
        self._block_records: dict[int, dict[str, Any]] = {}
        self._graph = DependencyGraph()
        self._commit_order: list[int] = []
        self._invalidation_manifest: list[dict[str, Any]] = []
        self._resume_info: dict[str, Any] = {"action": "not_started", "reasons": []}
        self._media_versions = {medium["id"]: 1 for medium in self.scene.media}
        self._light_versions = {light["id"]: 1 for light in self.scene.lights}
        if self.store is not None:
            try:
                version_sidecar = self.store.load_resource_versions()
            except PipelineError:
                version_sidecar = None
            if version_sidecar and version_sidecar.get("scene_hash") == self.scene.content_hash:
                for medium in self.scene.media:
                    medium_id = medium["id"]
                    if medium_id in version_sidecar.get("media_versions", {}):
                        self._media_versions[medium_id] = int(version_sidecar["media_versions"][medium_id])
                for light in self.scene.lights:
                    light_id = light["id"]
                    if light_id in version_sidecar.get("light_versions", {}):
                        self._light_versions[light_id] = int(version_sidecar["light_versions"][light_id])
        self._actual_step = self.config.step_size
        self._preview_bound = 0.0
        self._last_outcome: RenderOutcome | None = None
        self._resources = ResourceGovernor(self.limits)
        self._reset_arrays()

    @property
    def media_versions(self) -> dict[str, int]:
        return dict(self._media_versions)

    @property
    def light_versions(self) -> dict[str, int]:
        return dict(self._light_versions)

    @property
    def block_windows(self) -> list[BlockWindow]:
        return list(self._windows)

    def _reset_arrays(self) -> None:
        width = int(self.scene.camera["width"])
        height = int(self.scene.camera["height"])
        bands = self.scene.band_count
        self._transmission = np.zeros((height, width, bands), dtype=np.float64)
        self._scattering = np.zeros((height, width, bands), dtype=np.float64)
        self._path_units = np.zeros((height, width, bands), dtype=np.int64)
        self._optical_units = np.zeros((height, width, bands), dtype=np.int64)
        self._hit_mask = np.zeros((height, width), dtype=np.bool_)

    def _reset_run(self) -> None:
        self._states = BlockStateMachine([window.index for window in self._windows])
        self._results.clear()
        self._block_records.clear()
        self._graph = DependencyGraph()
        self._commit_order.clear()
        self._invalidation_manifest.clear()
        self._reset_arrays()

    def pause(self) -> None:
        self._pause_event.set()

    def clear_pause(self) -> None:
        self._pause_event.clear()

    def statuses(self) -> dict[int, str]:
        return self._states.statuses()

    def progress(self) -> dict[str, int]:
        return self._states.progress()

    def committed_ids(self) -> list[int]:
        return self._states.committed_ids()

    def retry_failed(self) -> list[int]:
        retried = self._states.failed_ids()
        for block_id in retried:
            self._states.retry(block_id)
        return retried

    def _snapshot(self, *, complete: bool, mode: str) -> Snapshot:
        records = [self._block_records[block_id] for block_id in self._commit_order if block_id in self._block_records]
        return Snapshot(
            sequence=0,
            scene_hash=self.scene.content_hash,
            media_versions=self.media_versions,
            light_versions=self.light_versions,
            algorithm_version=ALGORITHM_VERSION,
            config_hash=self.config.config_hash,
            actual_step=self._actual_step,
            preview_error_bound=self._preview_bound,
            transmission=self._transmission.copy(),
            scattering=self._scattering.copy(),
            path_units=self._path_units.copy(),
            optical_units=self._optical_units.copy(),
            hit_mask=self._hit_mask.copy(),
            committed_mask=np.asarray(
                [self._states.status(window.index) == BlockStatus.COMMITTED.value for window in self._windows],
                dtype=np.bool_,
            ),
            block_records=records,
            commit_order=list(self._commit_order),
            complete=complete,
            width=int(self.scene.camera["width"]),
            height=int(self.scene.camera["height"]),
            band_count=self.scene.band_count,
            block_width=self.config.block_width,
            block_height=self.config.block_height,
            render_mode=mode,
        )

    def _persist(
        self,
        *,
        mode: str,
        commit_event: dict[str, Any] | None = None,
        force_complete: bool = False,
    ) -> None:
        if self.store is None:
            return
        complete = force_complete or len(self._states.committed_ids()) == len(self._windows)
        snapshot = self._snapshot(complete=complete, mode=mode)
        self.store.write(snapshot, commit_event=commit_event)

    def _validate_snapshot_blocks(self, snapshot: Snapshot) -> list[str]:
        reasons: list[str] = []
        committed_ids = [index for index, committed in enumerate(snapshot.committed_mask.tolist()) if committed]
        record_ids = sorted(int(record["block_id"]) for record in snapshot.block_records)
        if committed_ids != record_ids:
            reasons.append("block_records")
            return reasons
        for record in snapshot.block_records:
            block_id = int(record["block_id"])
            window = self._windows[block_id]
            checksum = _buffer_checksum(
                snapshot.transmission[window.y0 : window.y1, window.x0 : window.x1, :],
                snapshot.scattering[window.y0 : window.y1, window.x0 : window.x1, :],
                snapshot.path_units[window.y0 : window.y1, window.x0 : window.x1, :],
                snapshot.optical_units[window.y0 : window.y1, window.x0 : window.x1, :],
            )
            if checksum != record.get("buffer_checksum"):
                reasons.append(f"block_checksum[{block_id}]")
        return reasons

    def _restore_snapshot(self, snapshot: Snapshot) -> list[str]:
        reasons = self._validate_snapshot_blocks(snapshot)
        if reasons:
            return reasons
        self._reset_run()
        self._transmission = snapshot.transmission.copy()
        self._scattering = snapshot.scattering.copy()
        self._path_units = snapshot.path_units.copy()
        self._optical_units = snapshot.optical_units.copy()
        self._hit_mask = snapshot.hit_mask.copy()
        self._commit_order = sorted(int(value) for value in snapshot.commit_order)
        self._actual_step = float(snapshot.actual_step)
        self._preview_bound = float(snapshot.preview_error_bound)
        for record in snapshot.block_records:
            block_id = int(record["block_id"])
            self._block_records[block_id] = copy.deepcopy(record)
            self._graph.set_block(
                block_id,
                record.get("media_dependencies", []),
                record.get("light_dependencies", []),
            )
            self._states.transition(block_id, BlockStatus.TRANSMISSION_ACCUMULATING)
            self._states.transition(block_id, BlockStatus.SCATTERING_ACCUMULATING)
            self._states.transition(
                block_id,
                BlockStatus.COMMITTED,
                reason="resume",
                media_versions={str(k): int(v) for k, v in record.get("media_versions", {}).items()},
                light_versions={str(k): int(v) for k, v in record.get("light_versions", {}).items()},
                buffer_checksum=record.get("buffer_checksum"),
            )
        return []

    def _compatibility(self, snapshot: Snapshot) -> list[str]:
        if self.store is None:
            return ["no_checkpoint_store"]
        reasons = self.store.compatibility_reasons(
            snapshot,
            scene_hash=self.scene.content_hash,
            media_versions=self.media_versions,
            light_versions=self.light_versions,
            algorithm_version=ALGORITHM_VERSION,
            config_hash=self.config.config_hash,
            width=int(self.scene.camera["width"]),
            height=int(self.scene.camera["height"]),
            band_count=self.scene.band_count,
            block_width=self.config.block_width,
            block_height=self.config.block_height,
            block_count=len(self._windows),
        )
        expected_step = self.config.effective_step(snapshot.render_mode)
        if abs(snapshot.actual_step - expected_step) > 1.0e-15:
            reasons.append("actual_step")
        reasons.extend(self._validate_snapshot_blocks(snapshot))
        return reasons

    def _load_for_resume(self) -> bool:
        if self.store is None:
            self._reset_run()
            self._resume_info = {"action": "fresh_full_recompute", "reasons": ["no_checkpoint_store"]}
            return False
        try:
            snapshot = self.store.load()
        except PipelineError as exc:
            self.store.discard([str(exc.code)])
            self._reset_run()
            self._resume_info = {"action": "discard_full_recompute", "reasons": [exc.code, exc.field]}
            return False
        if snapshot is None:
            self._reset_run()
            self._resume_info = {"action": "fresh_full_recompute", "reasons": ["checkpoint_missing"]}
            self.store.write_decision(
                {"action": "fresh_full_recompute", "reasons": ["checkpoint_missing"], "at_unix": time.time()}
            )
            return False
        reasons = self._compatibility(snapshot)
        if reasons:
            self.store.discard(reasons)
            self._reset_run()
            self._resume_info = {"action": "discard_full_recompute", "reasons": reasons}
            return False
        restore_reasons = self._restore_snapshot(snapshot)
        if restore_reasons:
            self.store.discard(restore_reasons)
            self._reset_run()
            self._resume_info = {"action": "discard_full_recompute", "reasons": restore_reasons}
            return False
        self._resume_info = {
            "action": "resume",
            "reasons": [],
            "sequence": snapshot.sequence,
            "committed": snapshot.committed_count,
            "complete": snapshot.complete,
            "render_mode": snapshot.render_mode,
        }
        self.store.write_decision(
            {
                "action": "resume",
                "reasons": [],
                "sequence": snapshot.sequence,
                "committed": snapshot.committed_count,
                "at_unix": time.time(),
            }
        )
        return True

    def _prepare_mode(self, mode: str) -> None:
        if mode not in {"full", "preview", "resume", "incremental"}:
            raise parameter_error("render.mode", "mode must be full, preview, resume, or incremental")
        if mode in {"full", "preview"}:
            self._reset_run()
            self._resume_info = {
                "action": "full_recompute" if mode == "full" else "preview_recompute",
                "reasons": [],
            }
            if self.store is not None:
                self.store.write_decision(
                    {"action": self._resume_info["action"], "reasons": [], "at_unix": time.time()}
                )
        elif mode == "resume":
            self._load_for_resume()
        # incremental keeps the current in-memory or restored state.

    def _block_versions(self) -> tuple[dict[str, int], dict[str, int]]:
        return self.media_versions, self.light_versions

    def _fail_block(self, block_id: int, message: str) -> None:
        current = self._states.status(block_id)
        if current == BlockStatus.FAILED.value or current == BlockStatus.COMMITTED.value:
            return
        try:
            self._states.transition(block_id, BlockStatus.FAILED, reason="block_exception", failure=message)
        except PipelineError:
            pass

    def _worker(
        self,
        block_id: int,
        mode: str,
        failure_injector: Callable[[int], None] | None,
        governor: ResourceGovernor,
    ) -> BlockResult:
        window = self._windows[block_id]
        try:
            with governor.thread_slot():
                self._states.transition(block_id, BlockStatus.TRANSMISSION_ACCUMULATING, reason=mode)
                if failure_injector is not None:
                    failure_injector(block_id)

                def on_transmission_complete() -> None:
                    self._states.transition(block_id, BlockStatus.SCATTERING_ACCUMULATING, reason=mode)

                result = render_block(
                    self.scene,
                    self.config,
                    window,
                    sample_index=self.sample_index,
                    mode=mode,
                    transmission_callback=on_transmission_complete,
                    governor=governor,
                )
                return result
        except Exception as exc:
            self._fail_block(block_id, f"{type(exc).__name__}: {exc}")
            raise

    def _commit_block(self, block_id: int, result: BlockResult, mode: str) -> None:
        media_versions, light_versions = self._block_versions()
        checksum = result.buffer_checksum
        self._states.transition(
            block_id,
            BlockStatus.COMMITTED,
            reason=mode,
            media_versions=media_versions,
            light_versions=light_versions,
            buffer_checksum=checksum,
        )
        window = result.window
        self._transmission[window.y0 : window.y1, window.x0 : window.x1, :] = result.transmission
        self._scattering[window.y0 : window.y1, window.x0 : window.x1, :] = result.scattering
        self._path_units[window.y0 : window.y1, window.x0 : window.x1, :] = result.path_units
        self._optical_units[window.y0 : window.y1, window.x0 : window.x1, :] = result.optical_units
        self._hit_mask[window.y0 : window.y1, window.x0 : window.x1] |= result.hit_mask
        self._results[block_id] = result
        self._graph.set_block(block_id, result.media_ids, result.light_ids)
        self._commit_order = sorted(self._states.committed_ids())
        self._block_records[block_id] = {
            "block_id": block_id,
            "row": window.row,
            "col": window.col,
            "media_versions": dict(media_versions),
            "light_versions": dict(light_versions),
            "media_dependencies": list(result.media_ids),
            "light_dependencies": list(result.light_ids),
            "media_field_dependencies": {
                media_id: sorted(fields)
                for media_id, fields in sorted(self._graph.block_media_fields.get(block_id, {}).items())
            },
            "light_field_dependencies": {
                light_id: sorted(fields)
                for light_id, fields in sorted(self._graph.block_light_fields.get(block_id, {}).items())
            },
            "buffer_checksum": checksum,
            "duration_ms": result.duration_ms,
            "max_path_quantization_error": result.max_path_quantization_error,
            "max_optical_quantization_error": result.max_optical_quantization_error,
            "tir_pixels": result.tir_pixels,
        }
        complete = len(self._states.committed_ids()) == len(self._windows)
        event = {
            "block_id": block_id,
            "scene_hash": self.scene.content_hash,
            "algorithm_version": ALGORITHM_VERSION,
            "config_hash": self.config.config_hash,
            "media_versions": dict(media_versions),
            "light_versions": dict(light_versions),
            "buffer_checksum": checksum,
            "committed_count": len(self._states.committed_ids()),
            "total_blocks": len(self._windows),
            "complete": complete,
        }
        self._persist(mode=mode, commit_event=event, force_complete=complete)

    def _pending_blocks(self) -> list[int]:
        pending: list[int] = []
        for window in self._windows:
            status = self._states.status(window.index)
            if status == BlockStatus.COMMITTED.value:
                continue
            if status == BlockStatus.FAILED.value:
                self._states.retry(window.index)
            elif status != BlockStatus.PENDING_INTERSECT.value:
                # A paused/crashed process may leave an active in-memory block.
                # Treat it as failed so that retry can restore the pending state.
                self._fail_block(window.index, "orphaned active state")
                self._states.retry(window.index)
            pending.append(window.index)
        return sorted(pending)

    def render(
        self,
        *,
        mode: str = "full",
        failure_injector: Callable[[int], None] | None = None,
        crash_after_commits: int | None = None,
    ) -> RenderOutcome:
        with self._lock:
            self._prepare_mode(mode)
            self._pause_event.clear()
            effective_mode = (
                mode
                if mode != "resume"
                else str(self._resume_info.get("render_mode", "full"))
            )
            if effective_mode not in {"full", "preview", "incremental"}:
                effective_mode = "full"
            # The snapshot knows whether the interrupted run was a preview.
            self._actual_step = self.config.effective_step(effective_mode)
            self._preview_bound = conservative_preview_bound(self.scene, self.config, effective_mode)
            pending = self._pending_blocks()
            governor = ResourceGovernor(self.limits)
            completed_since_start = 0
            paused = False
            window_size = min(self.limits.max_parallelism, self.limits.max_pending_blocks)
            if pending and window_size <= 0:
                raise parameter_error("resources.max_pending_blocks", "pending block budget must be positive")
            cursor = 0
            while cursor < len(pending):
                if self._pause_event.is_set():
                    paused = True
                    break
                batch = pending[cursor : cursor + window_size]
                futures: dict[Future[BlockResult], int] = {}
                with ThreadPoolExecutor(max_workers=max(1, min(window_size, len(batch)))) as executor:
                    for block_id in batch:
                        future = executor.submit(
                            self._worker,
                            block_id,
                            effective_mode,
                            failure_injector,
                            governor,
                        )
                        futures[future] = block_id
                    settled: dict[int, BlockResult | Exception] = {}
                    for future in futures:
                        block_id = futures[future]
                        try:
                            settled[block_id] = future.result()
                        except Exception as exc:
                            settled[block_id] = exc
                for block_id in batch:
                    result = settled[block_id]
                    if isinstance(result, Exception):
                        continue
                    self._commit_block(block_id, result, effective_mode)
                    completed_since_start += 1
                    if crash_after_commits is not None and completed_since_start >= crash_after_commits:
                        raise SimulatedCrash(completed_since_start)
                cursor += len(batch)
                if self._pause_event.is_set():
                    paused = True
                    break
            complete = len(self._states.committed_ids()) == len(self._windows)
            if complete and self.store is not None:
                self._persist(mode=effective_mode, force_complete=True)
                self.store.write_decision(
                    {
                        "action": "complete",
                        "reasons": [],
                        "committed": len(self._windows),
                        "at_unix": time.time(),
                    }
                )
            failed = {
                block_id: (self._states.state(block_id).failure or "unknown")
                for block_id in self._states.failed_ids()
            }
            outcome = RenderOutcome(
                transmission=self._transmission.copy(),
                scattering=self._scattering.copy(),
                path_units=self._path_units.copy(),
                optical_units=self._optical_units.copy(),
                hit_mask=self._hit_mask.copy(),
                stats=render_stats(self.scene, self._transmission, self._scattering, self._hit_mask),
                statuses=self._states.statuses(),
                status_labels=self._states.labels(),
                progress=self._states.progress(),
                commit_order=list(self._commit_order),
                complete=complete,
                failed_blocks=failed,
                mode=effective_mode,
                actual_step=self._actual_step,
                preview_error_bound=self._preview_bound,
                algorithm_version=ALGORITHM_VERSION,
                config_hash=self.config.config_hash,
                scene_hash=self.scene.content_hash,
                media_versions=self.media_versions,
                light_versions=self.light_versions,
                media_dependencies={
                    block_id: tuple(self._graph.block_media.get(block_id, set()))
                    for block_id in sorted(self._graph.block_media)
                },
                light_dependencies={
                    block_id: tuple(self._graph.block_lights.get(block_id, set()))
                    for block_id in sorted(self._graph.block_lights)
                },
                dependency_graph=self._graph.to_dict(),
                invalidation_manifest=copy.deepcopy(self._invalidation_manifest),
                block_windows=list(self._windows),
                resource_stats=governor.snapshot(),
                resume=copy.deepcopy(self._resume_info),
                quantization={
                    "length_scale": LENGTH_SCALE,
                    "optical_scale": OPTICAL_SCALE,
                    "max_path_quantization_error": max(
                        (record.get("max_path_quantization_error", 0.0) for record in self._block_records.values()),
                        default=0.0,
                    ),
                    "max_optical_quantization_error": max(
                        (record.get("max_optical_quantization_error", 0.0) for record in self._block_records.values()),
                        default=0.0,
                    ),
                },
                block_locks={
                    block_id: {
                        "media_versions": dict(record.get("media_versions", {})),
                        "light_versions": dict(record.get("light_versions", {})),
                        "buffer_checksum": record.get("buffer_checksum"),
                        "media_dependencies": list(record.get("media_dependencies", [])),
                        "light_dependencies": list(record.get("light_dependencies", [])),
                        "media_field_dependencies": record.get("media_field_dependencies", {}),
                        "light_field_dependencies": record.get("light_field_dependencies", {}),
                    }
                    for block_id, record in sorted(self._block_records.items())
                },
                paused=paused,
            )
            self._last_outcome = outcome
            return outcome

    @property
    def last_outcome(self) -> RenderOutcome | None:
        return self._last_outcome

    def _invalidate_blocks(
        self,
        block_ids: list[int],
        *,
        reason: str,
        preserve_transmission: bool = False,
    ) -> None:
        for block_id in sorted(set(block_ids)):
            if self._states.status(block_id) == BlockStatus.COMMITTED.value:
                self._states.invalidate(block_id, reason)
                window = self._windows[block_id]
                self._scattering[window.y0 : window.y1, window.x0 : window.x1, :] = 0.0
                if not preserve_transmission:
                    self._transmission[window.y0 : window.y1, window.x0 : window.x1, :] = 0.0
                    self._path_units[window.y0 : window.y1, window.x0 : window.x1, :] = 0
                    self._optical_units[window.y0 : window.y1, window.x0 : window.x1, :] = 0
                    self._hit_mask[window.y0 : window.y1, window.x0 : window.x1] = False
                self._results.pop(block_id, None)
                self._block_records.pop(block_id, None)
            elif self._states.status(block_id) == BlockStatus.FAILED.value:
                self._states.retry(block_id)
        self._commit_order = sorted(self._states.committed_ids())

    def _propagate(
        self,
        *,
        resource_kind: str,
        resource_id: str,
        field_names: list[str],
        stage: str,
        extra_blocks: list[int] | None = None,
        preserve_transmission: bool = False,
    ) -> list[dict[str, Any]]:
        if resource_kind == "medium":
            direct = self._graph.blocks_for_media(resource_id)
            # All blocks currently touching this resource are direct dependencies.
            # Their scattering stages are downstream of their own transmission stages.
            downstream: set[int] = set()
        else:
            direct = self._graph.blocks_for_light(resource_id)
            downstream = set()
        extra = set(extra_blocks or [])
        all_affected = sorted(set(direct) | downstream | extra)
        entries = self._graph.invalidation_manifest(
            resource_id=resource_id,
            resource_kind=resource_kind,
            field_names=field_names,
            stage=stage,
            direct_blocks=direct,
            downstream_blocks=sorted(downstream),
            extra_blocks=sorted(extra),
        )
        self._invalidate_blocks(
            all_affected,
            reason="invalidate",
            preserve_transmission=preserve_transmission,
        )
        self._invalidation_manifest.extend(entries)
        return entries

    def _projected_medium_blocks(self, medium: dict[str, Any]) -> list[int]:
        boundary = medium["boundary"]
        if boundary["type"] == "sphere":
            center = np.asarray(boundary["center"], dtype=np.float64)
            radius = float(boundary["radius"])
            lower = center - radius
            upper = center + radius
        elif boundary["type"] == "box":
            lower = np.asarray(boundary["min"], dtype=np.float64)
            upper = np.asarray(boundary["max"], dtype=np.float64)
        else:
            vertices = np.asarray(boundary["vertices"], dtype=np.float64)
            lower = np.min(vertices, axis=0)
            upper = np.max(vertices, axis=0)
        corners = np.asarray(
            [
                [x, y, z]
                for x in (lower[0], upper[0])
                for y in (lower[1], upper[1])
                for z in (lower[2], upper[2])
            ],
            dtype=np.float64,
        )
        sample = self.scene.camera["trajectory"][self.sample_index]
        eye = np.asarray(sample["eye"], dtype=np.float64)
        target = np.asarray(sample["target"], dtype=np.float64)
        up = np.asarray(sample["up"], dtype=np.float64)
        forward = target - eye
        forward /= np.linalg.norm(forward)
        right = np.cross(forward, up)
        right /= np.linalg.norm(right)
        true_up = np.cross(right, forward)
        relative = corners - eye
        depth = relative @ forward
        if np.any(depth <= float(self.scene.camera["near_far"][0])):
            return [window.index for window in self._windows]
        width = int(self.scene.camera["width"])
        height = int(self.scene.camera["height"])
        tangent = math.tan(math.radians(float(self.scene.camera["fov_y_deg"])) * 0.5)
        x_ndc = (relative @ right) / (depth * tangent * (width / height))
        y_ndc = (relative @ true_up) / (depth * tangent)
        pixel_x = (x_ndc + 1.0) * width * 0.5 - 0.5
        pixel_y = (1.0 - y_ndc) * height * 0.5 - 0.5
        x_min = max(0.0, float(np.min(pixel_x)) - 1.5)
        x_max = min(float(width), float(np.max(pixel_x)) + 1.5)
        y_min = max(0.0, float(np.min(pixel_y)) - 1.5)
        y_max = min(float(height), float(np.max(pixel_y)) + 1.5)
        return [
            window.index
            for window in self._windows
            if x_min < window.x1 and x_max >= window.x0 and y_min < window.y1 and y_max >= window.y0
        ]

    def _persist_invalidation(self) -> None:
        if self.store is None:
            return
        self._persist(mode="incremental", force_complete=False)
        self.store.write_decision(
            {
                "action": "incremental_invalidation",
                "reasons": [],
                "committed": len(self._states.committed_ids()),
                "at_unix": time.time(),
            }
        )

    def edit_media(self, medium_id: str, updates: dict[str, Any]) -> list[dict[str, Any]]:
        with self._lock:
            if medium_id not in self.scene.media_by_id:
                raise schema_error("scene.media.id", f"unknown medium {medium_id!r}")
            candidate = self.scene.copy()
            target = candidate.media_by_id[medium_id]
            unknown = set(updates) - {"boundary", "ior", "absorption", "scattering", "kind", "color"}
            if unknown:
                raise schema_error(f"scene.media[{medium_id}]", f"unknown editable fields: {sorted(unknown)}")
            target.update(copy.deepcopy(updates))
            validate_scene_dict(candidate.data)
            field_names = sorted(updates)
            extra_blocks: list[int] = []
            if {"boundary", "kind"} & set(updates):
                extra_blocks = sorted(
                    set(self._projected_medium_blocks(self.scene.media_by_id[medium_id]))
                    | set(self._projected_medium_blocks(target))
                )
            self.scene = candidate
            self._media_versions[medium_id] = self._media_versions.get(medium_id, 1) + 1
            scattering_only = set(updates) <= {"scattering", "color"}
            stage = "scattering" if scattering_only else "transmission+scattering"
            entries = self._propagate(
                resource_kind="medium",
                resource_id=medium_id,
                field_names=field_names,
                stage=stage,
                extra_blocks=extra_blocks,
                preserve_transmission=scattering_only,
            )
            self._persist_invalidation()
            return entries

    def edit_light(self, light_id: str, updates: dict[str, Any]) -> list[dict[str, Any]]:
        with self._lock:
            if light_id not in self.scene.light_by_id:
                raise schema_error("scene.lights.id", f"unknown light {light_id!r}")
            candidate = self.scene.copy()
            target = candidate.light_by_id[light_id]
            unknown = set(updates) - {"direction", "radiance", "anisotropy", "type"}
            if unknown:
                raise schema_error(f"scene.lights[{light_id}]", f"unknown editable fields: {sorted(unknown)}")
            target.update(copy.deepcopy(updates))
            validate_scene_dict(candidate.data)
            field_names = sorted(updates)
            self.scene = candidate
            self._light_versions[light_id] = self._light_versions.get(light_id, 1) + 1
            entries = self._propagate(
                resource_kind="light",
                resource_id=light_id,
                field_names=field_names,
                stage="scattering",
                preserve_transmission=True,
            )
            self._persist_invalidation()
            return entries

    def edit_camera(self, updates: dict[str, Any]) -> list[dict[str, Any]]:
        with self._lock:
            if {"width", "height"} & set(updates):
                raise schema_error(
                    "scene.camera",
                    "width and height are layout fields; create a new engine for resolution changes",
                )
            candidate = self.scene.copy()
            candidate.camera.update(copy.deepcopy(updates))
            validate_scene_dict(candidate.data)
            if not 0 <= self.sample_index < len(candidate.camera["trajectory"]):
                raise parameter_error(
                    "render.sample_index",
                    "camera edit removed the active trajectory sample",
                )
            self.scene = candidate
            all_blocks = [window.index for window in self._windows]
            self._invalidate_blocks(all_blocks, reason="invalidate")
            entries = [
                {
                    "block_id": block_id,
                    "resource_kind": "camera",
                    "resource_id": "camera",
                    "field": ",".join(sorted(updates)),
                    "stage": "transmission+scattering",
                    "reason": "camera_projection_changed",
                }
                for block_id in all_blocks
            ]
            self._invalidation_manifest.extend(entries)
            self._persist_invalidation()
            return entries


def _buffer_checksum(
    transmission: np.ndarray,
    scattering: np.ndarray,
    path_units: np.ndarray,
    optical_units: np.ndarray,
) -> str:
    import hashlib

    return hashlib.sha256(
        np.asarray(transmission, dtype=np.float64).tobytes()
        + np.asarray(scattering, dtype=np.float64).tobytes()
        + np.asarray(path_units, dtype=np.int64).tobytes()
        + np.asarray(optical_units, dtype=np.int64).tobytes()
    ).hexdigest()


__all__ = ["RenderEngine", "RenderOutcome", "SimulatedCrash"]
