"""Incremental scheduling, persistence, recovery and consistency auditing."""

from __future__ import annotations

import threading
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping

import numpy as np

from .cache import (
    CacheEntry,
    StageCache,
    build_cache_entry,
    make_cache_key,
)
from .diagnostics import ComparisonReport, RunDiagnostics, StageDiagnostic
from .errors import PipelineCancelled, WorkbenchError
from .filters import STAGE_FUNCTIONS
from .imaging import DecodedImage, decode_image, first_mismatch
from .snapshot import SnapshotData, SnapshotStore
from .stages import (
    DEFAULT_PARAMS,
    GRAPH,
    STAGE_IDS,
    STAGE_SPECS,
    STAGE_VERSIONS,
    changed_stages,
    normalize_pipeline_params,
    normalize_stage_params,
    params_fingerprint,
)


StageRunner = Callable[
    [str, np.ndarray, Mapping[str, int | float], Callable[[], None]], np.ndarray
]
EventListener = Callable[[dict[str, Any]], None]


@dataclass(frozen=True)
class ResourceLimits:
    max_buffer_bytes: int = 256 * 1024 * 1024
    max_parallel: int = 1
    max_dimension: int = 16384
    working_set_factor: int = 12

    def __post_init__(self) -> None:
        if self.max_buffer_bytes < 1024:
            raise ValueError("max_buffer_bytes 必须至少为 1024")
        if self.max_dimension < 1:
            raise ValueError("max_dimension 必须为正数")
        if self.max_parallel < 1:
            raise ValueError("max_parallel 必须至少为 1")
        # The chain is ordered and shares one cache. One worker is the only
        # configuration that can guarantee commit order by construction.
        object.__setattr__(self, "max_parallel", 1)


class RunControl:
    """Cooperative pause/cancel token checked between channels and stages."""

    def __init__(self) -> None:
        self._cancel = threading.Event()
        self._pause = threading.Event()

    def request_pause(self) -> None:
        self._pause.set()

    def resume(self) -> None:
        self._pause.clear()

    def cancel(self) -> None:
        self._cancel.set()
        self._pause.clear()

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()

    @property
    def paused(self) -> bool:
        return self._pause.is_set() and not self._cancel.is_set()

    def checkpoint(self) -> None:
        while self._pause.is_set() and not self._cancel.is_set():
            time.sleep(0.02)
        if self._cancel.is_set():
            raise PipelineCancelled()


@dataclass(frozen=True)
class RestoreResult:
    status: str
    reason: str
    loaded_stages: tuple[str, ...] = ()
    snapshot_complete: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "reason": self.reason,
            "loaded_stages": list(self.loaded_stages),
            "snapshot_complete": self.snapshot_complete,
        }


@dataclass
class PipelineRun:
    status: str
    diagnostics: RunDiagnostics
    output: np.ndarray | None = None
    error: dict[str, Any] | None = None
    completed_stages: tuple[str, ...] = ()

    @property
    def ok(self) -> bool:
        return self.status == "complete"

    def to_dict(self, *, include_output: bool = False) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "status": self.status,
            "error": self.error,
            "completed_stages": list(self.completed_stages),
            "diagnostics": self.diagnostics.to_dict(),
        }
        if include_output and self.output is not None:
            payload["output"] = {
                "shape": list(self.output.shape),
                "dtype": str(self.output.dtype),
            }
        return payload


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


class PipelineSession:
    """One image + one parameter state + one atomic snapshot."""

    def __init__(
        self,
        snapshot_path: str | Path,
        *,
        params: Mapping[str, Mapping[str, Any]] | None = None,
        limits: ResourceLimits | None = None,
        stage_runner: StageRunner | None = None,
    ) -> None:
        self.limits = limits or ResourceLimits()
        self.snapshot = SnapshotStore(
            snapshot_path,
            max_bytes=max(self.limits.max_buffer_bytes * 2, 1024 * 1024),
        )
        self.graph = GRAPH
        self.params = normalize_pipeline_params(params)
        self.cache = StageCache()
        self._image: DecodedImage | None = None
        self._statuses: dict[str, str] = {stage_id: "empty" for stage_id in STAGE_IDS}
        self._status_errors: dict[str, dict[str, Any] | None] = {
            stage_id: None for stage_id in STAGE_IDS
        }
        self._run_lock = threading.Lock()
        self._commit_lock = threading.RLock()
        self._stage_runner = stage_runner or self._default_stage_runner
        self._restore = RestoreResult("none", "尚未关联输入图像")
        self._restored_from_snapshot = False
        self._previous_snapshot_complete: bool | None = None
        self._invalidated_last: set[str] = set()
        self._last_run: PipelineRun | None = None
        self._snapshot_writes = 0
        self._current_hash: str | None = None

    @staticmethod
    def _default_stage_runner(
        stage_id: str,
        array: np.ndarray,
        params: Mapping[str, int | float],
        checkpoint: Callable[[], None],
    ) -> np.ndarray:
        return STAGE_FUNCTIONS[stage_id](array, params, checkpoint)

    @property
    def image(self) -> DecodedImage | None:
        return self._image

    @property
    def restore_result(self) -> RestoreResult:
        return self._restore

    @property
    def last_run(self) -> PipelineRun | None:
        return self._last_run

    @property
    def stage_statuses(self) -> dict[str, str]:
        return dict(self._statuses)

    @property
    def stage_errors(self) -> dict[str, dict[str, Any] | None]:
        return {stage: dict(error) if error else None for stage, error in self._status_errors.items()}

    @property
    def is_complete(self) -> bool:
        return len(self.cache) == len(STAGE_IDS) and all(
            self._statuses[stage_id] in {"cached", "computed", "valid"}
            for stage_id in STAGE_IDS
        )

    def set_image(self, path: str | Path) -> RestoreResult:
        decoded = decode_image(path)
        self._check_resources("<input>", decoded.array)
        changed = self._image is not None and decoded.content_hash != self._current_hash
        first_image = self._image is None
        if changed:
            self._invalidate_everything("input_changed")
            self.snapshot.discard()
            self._restore = RestoreResult("none", "输入图像内容已变化，旧快照整体作废")
            self._restored_from_snapshot = False
            self._previous_snapshot_complete = None
        self._image = decoded
        self._current_hash = decoded.content_hash
        if first_image or (not changed and not self.cache):
            self._restore = self._restore_snapshot()
        return self._restore

    def set_pipeline_params(
        self,
        params: Mapping[str, Mapping[str, Any]],
    ) -> tuple[str, ...]:
        normalized = normalize_pipeline_params(params)
        changes = changed_stages(self.params, normalized)
        if not changes:
            return ()
        first_changed = next(stage for stage in STAGE_IDS if stage in changes)
        affected = self.cache.invalidate_from(first_changed)
        for stage_id in affected:
            self._statuses[stage_id] = "invalid"
            self._status_errors[stage_id] = None
        self._invalidated_last = set(affected)
        self.params = normalized
        self._restored_from_snapshot = False
        self._emit(None, {"type": "invalidated", "stages": list(affected)})
        return affected

    def set_stage_params(
        self,
        stage_id: str,
        params: Mapping[str, Any],
    ) -> tuple[str, ...]:
        normalized = normalize_stage_params(stage_id, params)
        candidate = {
            stage: dict(stage_params) for stage, stage_params in self.params.items()
        }
        candidate[stage_id] = normalized
        return self.set_pipeline_params(candidate)

    def retry(self, **kwargs: Any) -> PipelineRun:
        return self.run(**kwargs)

    def run(
        self,
        *,
        control: RunControl | None = None,
        listener: EventListener | None = None,
        force_full: bool = False,
    ) -> PipelineRun:
        control = control or RunControl()
        if not self._run_lock.acquire(blocking=False):
            error = WorkbenchError("E_BUSY", "已有处理任务正在运行")
            diagnostics = self._base_diagnostics(
                path_type="incremental",
                source="busy",
                status="failed",
                started_at=_utc_now(),
                started_value=time.perf_counter(),
                control=control,
                invalidated=set(),
                error=error.to_dict(),
            )
            run = PipelineRun("failed", diagnostics, error=error.to_dict())
            self._last_run = run
            return run

        try:
            if self._image is None:
                raise WorkbenchError("E_INPUT", "尚未加载输入图像")
            started_at = _utc_now()
            started = time.perf_counter()
            if force_full:
                self.cache.clear()
                for stage_id in STAGE_IDS:
                    self._statuses[stage_id] = "empty"
                    self._status_errors[stage_id] = None
                self._invalidated_last = set(STAGE_IDS)
                self.snapshot.discard()
            initial_cache_entries = len(self.cache)
            path_type = "full" if force_full or initial_cache_entries == 0 else "incremental"
            source = self._source_label(force_full or initial_cache_entries == 0)
            invalidated = set(self._invalidated_last)
            self._invalidated_last.clear()
            stats = {
                stage_id: StageDiagnostic(
                    stage=stage_id,
                    display_name=STAGE_SPECS[stage_id].display_name,
                    status=self._statuses[stage_id],
                    invalidated=stage_id in invalidated,
                )
                for stage_id in STAGE_IDS
            }
            self._emit(
                listener,
                {
                    "type": "run_start",
                    "path_type": path_type,
                    "source": source,
                    "invalidated": list(invalidated),
                },
            )

            completed: list[str] = []
            for stage_id in STAGE_IDS:
                control.checkpoint()
                input_array, upstream_key = self._stage_input(stage_id)
                expected_key = make_cache_key(
                    input_hash=self._current_hash or "",
                    stage_id=stage_id,
                    params=self.params[stage_id],
                    upstream_key=upstream_key,
                    stage_version=STAGE_VERSIONS[stage_id],
                )
                existing = self.cache.get(stage_id)
                if existing is not None and existing.cache_key == expected_key:
                    self._statuses[stage_id] = "cached"
                    self._status_errors[stage_id] = None
                    stats[stage_id].hits = 1
                    stats[stage_id].status = "cached"
                    stats[stage_id].cache_key = existing.cache_key
                    completed.append(stage_id)
                    self._emit(
                        listener,
                        {
                            "type": "cache_hit",
                            "stage": stage_id,
                            "cache_key": existing.cache_key,
                        },
                    )
                    continue

                if existing is not None:
                    self.cache.invalidate_from(stage_id)
                self._statuses[stage_id] = "recomputing"
                self._status_errors[stage_id] = None
                stats[stage_id].status = "recomputing"
                self._emit(
                    listener,
                    {
                        "type": "stage_start",
                        "stage": stage_id,
                        "upstream_key": upstream_key,
                    },
                )
                stage_started = time.perf_counter()
                try:
                    self._check_resources(stage_id, input_array)
                    output = self._stage_runner(
                        stage_id,
                        input_array,
                        self.params[stage_id],
                        control.checkpoint,
                    )
                    self._validate_stage_output(stage_id, input_array, output)
                    elapsed = time.perf_counter() - stage_started
                    entry = build_cache_entry(
                        stage_id=stage_id,
                        input_hash=self._current_hash or "",
                        params=self.params[stage_id],
                        upstream_key=upstream_key,
                        stage_version=STAGE_VERSIONS[stage_id],
                        output=output,
                        elapsed_sec=elapsed,
                    )
                    with self._commit_lock:
                        self.cache.put(entry)
                        self._statuses[stage_id] = "computed"
                        self._status_errors[stage_id] = None
                        stats[stage_id].recomputes = 1
                        stats[stage_id].elapsed_sec = elapsed
                        stats[stage_id].status = "computed"
                        stats[stage_id].cache_key = entry.cache_key
                        try:
                            self._save_snapshot(
                                chain_complete=False,
                                last_run=None,
                                last_error=None,
                            )
                        except Exception:
                            self.cache.invalidate_from(stage_id)
                            raise
                    completed.append(stage_id)
                    self._emit(
                        listener,
                        {
                            "type": "stage_done",
                            "stage": stage_id,
                            "elapsed_sec": elapsed,
                            "cache_key": entry.cache_key,
                        },
                    )
                except PipelineCancelled:
                    self._statuses[stage_id] = "cancelled"
                    stats[stage_id].status = "cancelled"
                    raise
                except Exception as exc:
                    error = self._exception_payload(stage_id, exc)
                    self._statuses[stage_id] = "failed"
                    self._status_errors[stage_id] = error
                    stats[stage_id].status = "failed"
                    stats[stage_id].error = error
                    for downstream in self.graph.downstream(stage_id):
                        self._statuses[downstream] = "blocked"
                        stats[downstream].status = "blocked"
                    self._save_snapshot(
                        chain_complete=False,
                        last_run=None,
                        last_error=error,
                    )
                    diagnostics = self._finish_diagnostics(
                        path_type=path_type,
                        source=source,
                        status="failed",
                        started_at=started_at,
                        started=started,
                        stats=stats,
                        control=control,
                        error=error,
                    )
                    diagnostics.snapshot_writes = self._snapshot_writes
                    run = PipelineRun(
                        "failed",
                        diagnostics,
                        error=error,
                        completed_stages=tuple(completed),
                    )
                    self._last_run = run
                    self._emit(listener, {"type": "run_failed", "error": error})
                    return run

            control.checkpoint()
            diagnostics = self._finish_diagnostics(
                path_type=path_type,
                source=source,
                status="complete",
                started_at=started_at,
                started=started,
                stats=stats,
                control=control,
                error=None,
            )
            self._save_snapshot(
                chain_complete=True,
                last_run=diagnostics.to_dict(),
                last_error=None,
            )
            diagnostics.snapshot_writes = self._snapshot_writes
            final_entry = self.cache.get(STAGE_IDS[-1])
            run = PipelineRun(
                "complete",
                diagnostics,
                output=final_entry.output if final_entry else None,
                completed_stages=tuple(completed),
            )
            self._last_run = run
            self._emit(
                listener,
                {"type": "run_complete", "diagnostics": diagnostics.to_dict()},
            )
            return run
        except PipelineCancelled:
            completed_names = tuple(
                stage_id
                for stage_id in STAGE_IDS
                if self.cache.get(stage_id) is not None
            )
            self._save_snapshot(
                chain_complete=False,
                last_run=None,
                last_error={"code": "E_CANCELLED", "message": "用户取消了处理"},
            )
            diagnostics = self._finish_diagnostics(
                path_type="incremental" if self.cache else "full",
                source=self._source_label(False),
                status="cancelled",
                started_at=_utc_now(),
                started=time.perf_counter(),
                stats=self._empty_stats(),
                control=control,
                error={"code": "E_CANCELLED", "message": "用户取消了处理"},
            )
            run = PipelineRun(
                "cancelled",
                diagnostics,
                error={"code": "E_CANCELLED", "message": "用户取消了处理"},
                completed_stages=completed_names,
            )
            self._last_run = run
            self._emit(listener, {"type": "run_cancelled", "completed": list(completed_names)})
            return run
        except WorkbenchError as exc:
            diagnostics = self._base_diagnostics(
                path_type="incremental" if self.cache else "full",
                source=self._source_label(False),
                status="failed",
                started_at=_utc_now(),
                started_value=time.perf_counter(),
                control=control,
                invalidated=set(),
                error=exc.to_dict(),
            )
            run = PipelineRun("failed", diagnostics, error=exc.to_dict())
            self._last_run = run
            self._emit(listener, {"type": "run_failed", "error": exc.to_dict()})
            return run
        except Exception as exc:
            error = {
                "code": "E_STORAGE",
                "message": f"{type(exc).__name__}: {exc}",
            }
            diagnostics = self._base_diagnostics(
                path_type="incremental" if self.cache else "full",
                source=self._source_label(False),
                status="failed",
                started_at=_utc_now(),
                started_value=time.perf_counter(),
                control=control,
                invalidated=set(),
                error=error,
            )
            run = PipelineRun("failed", diagnostics, error=error)
            self._last_run = run
            self._emit(listener, {"type": "run_failed", "error": error})
            return run
        finally:
            self._run_lock.release()

    def compare_with_full(
        self,
        *,
        control: RunControl | None = None,
    ) -> ComparisonReport:
        control = control or RunControl()
        if not self._run_lock.acquire(blocking=False):
            return ComparisonReport(
                equal=False,
                code="E_BUSY",
                message="已有处理任务正在运行，无法启动全量验算",
            )
        try:
            if self._image is None:
                return ComparisonReport(
                    equal=False, code="E_INPUT", message="尚未加载输入图像"
                )
            if not self.is_complete:
                return ComparisonReport(
                    equal=False,
                    code="E_INCOMPLETE",
                    message="增量链尚未完整完成，无法执行全量比对",
                    incremental_cache_entries=len(self.cache),
                    full_stage_count=len(STAGE_IDS),
                )

            started_at = _utc_now()
            started = time.perf_counter()
            full_stats = {
                stage_id: StageDiagnostic(
                    stage=stage_id,
                    display_name=STAGE_SPECS[stage_id].display_name,
                    status="pending",
                )
                for stage_id in STAGE_IDS
            }
            outputs: dict[str, np.ndarray] = {}
            current = self._image.array
            try:
                for stage_id in STAGE_IDS:
                    control.checkpoint()
                    full_stats[stage_id].status = "recomputing"
                    stage_started = time.perf_counter()
                    self._check_resources(stage_id, current)
                    output = self._stage_runner(
                        stage_id,
                        current,
                        self.params[stage_id],
                        control.checkpoint,
                    )
                    self._validate_stage_output(stage_id, current, output)
                    outputs[stage_id] = output
                    elapsed = time.perf_counter() - stage_started
                    full_stats[stage_id].recomputes = 1
                    full_stats[stage_id].elapsed_sec = elapsed
                    full_stats[stage_id].status = "computed"
                    current = output
            except PipelineCancelled:
                return ComparisonReport(
                    equal=False,
                    code="E_CANCELLED",
                    message="全量验算已取消",
                    incremental_cache_entries=len(self.cache),
                    full_stage_count=len(STAGE_IDS),
                )
            except Exception as exc:
                error = self._exception_payload("full_compare", exc)
                return ComparisonReport(
                    equal=False,
                    code=error["code"],
                    message=str(error.get("message", "全量验算失败")),
                    stage=error.get("stage"),
                    incremental_cache_entries=len(self.cache),
                    full_stage_count=len(STAGE_IDS),
                )

            full_diagnostics = self._finish_diagnostics(
                path_type="full",
                source="empty-cache-audit",
                status="complete",
                started_at=started_at,
                started=started,
                stats=full_stats,
                control=control,
                error=None,
                effective_cache_entries=len(STAGE_IDS),
                snapshot_writes=0,
                sync_from_cache=False,
            )
            compared: list[str] = []
            for stage_id in STAGE_IDS:
                before = self.cache.get(stage_id)
                if before is None:
                    return ComparisonReport(
                        equal=False,
                        code="E_INCOMPLETE",
                        message=f"增量缓存缺少阶段 {stage_id}",
                        stage=stage_id,
                        compared_stages=compared,
                        incremental_cache_entries=len(self.cache),
                        full_stage_count=len(STAGE_IDS),
                        full_diagnostics=full_diagnostics,
                    )
                compared.append(stage_id)
                mismatch = first_mismatch(before.output, outputs[stage_id])
                if mismatch is not None:
                    message = (
                        f"增量与全量结果首次不一致：阶段={stage_id} "
                        f"坐标=({mismatch['x']},{mismatch['y']}) "
                        f"通道={mismatch['channel']}"
                    )
                    return ComparisonReport(
                        equal=False,
                        code="E_DIFF",
                        message=message,
                        stage=stage_id,
                        x=mismatch["x"],
                        y=mismatch["y"],
                        channel=mismatch["channel"],
                        compared_stages=compared,
                        incremental_cache_entries=len(self.cache),
                        full_stage_count=len(STAGE_IDS),
                        full_diagnostics=full_diagnostics,
                    )
            return ComparisonReport(
                equal=True,
                code=None,
                message="增量路径与空缓存全量路径逐像素一致",
                compared_stages=compared,
                incremental_cache_entries=len(self.cache),
                full_stage_count=len(STAGE_IDS),
                full_diagnostics=full_diagnostics,
            )
        finally:
            self._run_lock.release()

    def assert_consistent(self, *, control: RunControl | None = None) -> None:
        report = self.compare_with_full(control=control)
        if not report.equal:
            raise WorkbenchError(
                report.code or "E_DIFF",
                report.message,
                stage=report.stage,
                x=report.x,
                y=report.y,
                channel=report.channel,
            )

    def cache_rows(self) -> list[dict[str, Any]]:
        rows = []
        for stage_id in STAGE_IDS:
            entry = self.cache.get(stage_id)
            rows.append(
                {
                    "stage": stage_id,
                    "display_name": STAGE_SPECS[stage_id].display_name,
                    "status": self._statuses[stage_id],
                    "cache_key": entry.cache_key if entry else None,
                    "upstream_key": entry.upstream_key if entry else None,
                    "elapsed_sec": entry.elapsed_sec if entry else None,
                    "valid": entry is not None,
                    "error": self._status_errors[stage_id],
                }
            )
        return rows

    def snapshot_summary(self) -> dict[str, Any]:
        manifest = self.snapshot.peek_manifest()
        return {
            "path": str(self.snapshot.path),
            "exists": self.snapshot.exists(),
            "last_write_ok": self.snapshot.last_write_ok,
            "writes_this_session": self._snapshot_writes,
            "restore": self._restore.to_dict(),
            "manifest_chain_complete": (
                manifest.get("chain_complete") if manifest else None
            ),
            "manifest_updated_at": manifest.get("updated_at") if manifest else None,
        }

    def _stage_input(self, stage_id: str) -> tuple[np.ndarray, str]:
        spec = STAGE_SPECS[stage_id]
        if not spec.dependencies:
            assert self._image is not None
            return self._image.array, f"input:{self._current_hash}"
        upstream_id = spec.dependencies[0]
        upstream = self.cache.get(upstream_id)
        if upstream is None:
            raise WorkbenchError(
                "E_STAGE",
                f"阶段 {stage_id} 的上游 {upstream_id} 没有有效缓存",
                stage=stage_id,
            )
        return upstream.output, upstream.cache_key

    def _validate_stage_output(
        self,
        stage_id: str,
        input_array: np.ndarray,
        output: np.ndarray,
    ) -> None:
        if not isinstance(output, np.ndarray):
            raise WorkbenchError(
                "E_STAGE", "阶段输出不是 numpy.ndarray", stage=stage_id
            )
        if output.shape != input_array.shape:
            raise WorkbenchError(
                "E_STAGE",
                f"阶段输出尺寸/通道改变：{input_array.shape} -> {output.shape}",
                stage=stage_id,
            )
        if output.dtype != np.uint8:
            raise WorkbenchError(
                "E_STAGE",
                f"阶段输出 dtype 必须为 uint8，实际 {output.dtype}",
                stage=stage_id,
            )
        self._check_resources(stage_id, output)

    def _check_resources(self, stage_id: str, array: np.ndarray) -> None:
        if max(array.shape[:2]) > self.limits.max_dimension:
            raise WorkbenchError(
                "E_MEMORY",
                f"图像边长超过限制 {self.limits.max_dimension}",
                stage=stage_id,
            )
        estimated = array.nbytes * self.limits.working_set_factor
        if estimated > self.limits.max_buffer_bytes:
            raise WorkbenchError(
                "E_MEMORY",
                (
                    f"阶段预计工作集 {estimated} 字节超过上限 "
                    f"{self.limits.max_buffer_bytes} 字节"
                ),
                stage=stage_id,
                estimated_bytes=estimated,
                limit_bytes=self.limits.max_buffer_bytes,
            )

    def _save_snapshot(
        self,
        *,
        chain_complete: bool,
        last_run: Mapping[str, Any] | None,
        last_error: Mapping[str, Any] | None,
    ) -> None:
        if self._image is None:
            return
        entries = self.cache.valid_prefix()
        if not entries:
            return
        self.snapshot.save(
            image_metadata=self._image.metadata(),
            params=self.params,
            params_fingerprint=params_fingerprint(self.params),
            stage_versions=STAGE_VERSIONS,
            entries=entries,
            chain_complete=chain_complete,
            last_run=last_run,
            last_error=last_error,
        )
        self._snapshot_writes += 1

    def _restore_snapshot(self) -> RestoreResult:
        if self._image is None:
            return RestoreResult("none", "尚未加载输入图像")
        if not self.snapshot.exists():
            return RestoreResult("none", "没有持久化快照")
        data = self.snapshot.load()
        if data is None:
            reason = f"快照无法解析，整体作废: {self.snapshot.last_error}"
            self.snapshot.discard()
            self._invalidate_everything("snapshot_unreadable")
            return RestoreResult("discarded", reason)
        valid, reason, entries = self._validate_snapshot(data)
        if not valid:
            self.snapshot.discard()
            self._invalidate_everything("snapshot_mismatch")
            self._restored_from_snapshot = False
            self._previous_snapshot_complete = None
            return RestoreResult("discarded", reason)

        self.cache.replace_prefix(entries)
        for stage_id in STAGE_IDS:
            if any(entry.stage == stage_id for entry in entries):
                self._statuses[stage_id] = "cached"
                self._status_errors[stage_id] = None
            else:
                self._statuses[stage_id] = "empty"
        self._restored_from_snapshot = True
        self._previous_snapshot_complete = data.complete
        loaded = tuple(entry.stage for entry in entries)
        if data.complete:
            return RestoreResult(
                "restored",
                "已加载完整且版本匹配的快照",
                loaded_stages=loaded,
                snapshot_complete=True,
            )
        note = "已恢复部分快照；上次运行未完整结束，将从缺失阶段继续"
        if data.manifest.get("last_error"):
            note += f"；上次错误={data.manifest['last_error'].get('code')}"
        return RestoreResult(
            "partial",
            note,
            loaded_stages=loaded,
            snapshot_complete=False,
        )

    def _validate_snapshot(
        self,
        data: SnapshotData,
    ) -> tuple[bool, str, list[CacheEntry]]:
        assert self._image is not None
        manifest = data.manifest
        if not self.snapshot.validate_outputs(data):
            return False, "快照输出哈希、尺寸或 dtype 校验失败，整体作废", []
        image_meta = manifest.get("image", {})
        if image_meta.get("content_hash") != self._image.content_hash:
            return False, "输入图像内容哈希不匹配，整体快照作废", []
        if image_meta.get("mode") != self._image.mode:
            return False, "输入图像模式不匹配，整体快照作废", []
        if (
            image_meta.get("width") != self._image.width
            or image_meta.get("height") != self._image.height
            or image_meta.get("channels") != self._image.channels
            or image_meta.get("dtype") != self._image.dtype
        ):
            return False, "输入图像布局元数据不匹配，整体快照作废", []
        expected_fingerprint = params_fingerprint(self.params)
        if manifest.get("params_fingerprint") != expected_fingerprint:
            return False, "阶段参数版本/参数集不匹配，整体快照作废", []
        if manifest.get("params_version") != expected_fingerprint:
            return False, "参数版本不匹配，整体快照作废", []
        if manifest.get("stage_versions") != STAGE_VERSIONS:
            return False, "阶段算法版本不匹配，整体快照作废", []

        completed = list(manifest.get("completed_stages", []))
        if completed != list(STAGE_IDS[: len(completed)]):
            return False, "快照阶段不是合法前缀，整体快照作废", []
        if manifest.get("chain_complete") and len(completed) != len(STAGE_IDS):
            return False, "快照声称完成但缺少阶段，整体作废", []
        if set(data.outputs) != set(completed):
            return False, "快照输出集合与清单不一致，整体作废", []

        entries: list[CacheEntry] = []
        expected_upstream = f"input:{self._image.content_hash}"
        for stage_id in completed:
            metadata = manifest.get("entries", {}).get(stage_id)
            if not isinstance(metadata, dict):
                return False, f"阶段 {stage_id} 元数据缺失，整体作废", []
            if metadata.get("stage") != stage_id:
                return False, f"阶段 {stage_id} 标识不匹配，整体作废", []
            if metadata.get("stage_version") != STAGE_VERSIONS[stage_id]:
                return False, f"阶段 {stage_id} 版本不匹配，整体作废", []
            if metadata.get("input_hash") != self._image.content_hash:
                return False, f"阶段 {stage_id} 输入哈希不匹配，整体作废", []
            if metadata.get("params") != self.params[stage_id]:
                return False, f"阶段 {stage_id} 参数不匹配，整体作废", []
            if metadata.get("upstream_key") != expected_upstream:
                return False, f"阶段 {stage_id} 上游键不匹配，整体作废", []
            expected_key = make_cache_key(
                input_hash=self._image.content_hash,
                stage_id=stage_id,
                params=self.params[stage_id],
                upstream_key=expected_upstream,
                stage_version=STAGE_VERSIONS[stage_id],
            )
            if metadata.get("cache_key") != expected_key:
                return False, f"阶段 {stage_id} 缓存键校验失败，整体作废", []
            output = data.outputs[stage_id]
            if output.shape != self._image.array.shape or output.dtype != np.uint8:
                return False, f"阶段 {stage_id} 输出布局不匹配，整体作废", []
            output.setflags(write=False)
            entry = CacheEntry(
                stage=stage_id,
                cache_key=expected_key,
                upstream_key=expected_upstream,
                input_hash=self._image.content_hash,
                stage_version=STAGE_VERSIONS[stage_id],
                params=dict(self.params[stage_id]),
                output=output,
                elapsed_sec=float(metadata.get("elapsed_sec", 0.0)),
                output_hash=str(metadata.get("output_hash", "")),
                committed_at=str(metadata.get("committed_at", "")),
            )
            entries.append(entry)
            expected_upstream = expected_key
        return True, "快照校验通过", entries

    def _invalidate_everything(self, reason: str) -> None:
        self.cache.clear()
        for stage_id in STAGE_IDS:
            self._statuses[stage_id] = "empty"
            self._status_errors[stage_id] = None
        if reason:
            self._invalidated_last = set(STAGE_IDS)

    def _source_label(self, force_full: bool) -> str:
        if force_full:
            return "empty-cache"
        if self._restored_from_snapshot:
            if self._previous_snapshot_complete:
                return "snapshot"
            return "partial-snapshot"
        if self._invalidated_last:
            return "in-session-cache"
        return "in-session-cache"

    def _finish_diagnostics(
        self,
        *,
        path_type: str,
        source: str,
        status: str,
        started_at: str,
        started: float,
        stats: dict[str, StageDiagnostic],
        control: RunControl,
        error: dict[str, Any] | None,
        effective_cache_entries: int | None = None,
        snapshot_writes: int | None = None,
        sync_from_cache: bool = True,
    ) -> RunDiagnostics:
        if sync_from_cache:
            for stage_id in STAGE_IDS:
                stats[stage_id].status = self._statuses[stage_id]
                entry = self.cache.get(stage_id)
                if entry is not None:
                    stats[stage_id].cache_key = entry.cache_key
        notes: list[str] = []
        if self._restored_from_snapshot and self._previous_snapshot_complete is False:
            notes.append(
                "恢复自未完成快照；不对强制终止前的完整报告作任何声明。"
            )
        if status == "cancelled":
            notes.append("取消前已完成的阶段输出已按阶段原子持久化。")
        return RunDiagnostics(
            run_id=uuid.uuid4().hex[:12],
            path_type=path_type,
            source=source,
            status=status,
            stages=[stats[stage_id] for stage_id in STAGE_IDS],
            started_at=started_at,
            finished_at=_utc_now(),
            total_elapsed_sec=time.perf_counter() - started,
            effective_cache_entries=(
                len(self.cache)
                if effective_cache_entries is None
                else effective_cache_entries
            ),
            full_stage_count=len(STAGE_IDS),
            cache_hit_count=sum(stage.hits for stage in stats.values()),
            recompute_count=sum(stage.recomputes for stage in stats.values()),
            snapshot_path=str(self.snapshot.path),
            snapshot_writes=(
                self._snapshot_writes
                if snapshot_writes is None
                else snapshot_writes
            ),
            chain_complete=status == "complete" and self.is_complete,
            restored_from_snapshot=self._restored_from_snapshot,
            previous_snapshot_complete=self._previous_snapshot_complete,
            max_parallelism=self.limits.max_parallel,
            error=error,
            notes=notes,
        )

    def _base_diagnostics(
        self,
        *,
        path_type: str,
        source: str,
        status: str,
        started_at: str,
        started_value: float,
        control: RunControl,
        invalidated: set[str],
        error: dict[str, Any] | None,
    ) -> RunDiagnostics:
        stats = {
            stage_id: StageDiagnostic(
                stage=stage_id,
                display_name=STAGE_SPECS[stage_id].display_name,
                status=self._statuses[stage_id],
                invalidated=stage_id in invalidated,
            )
            for stage_id in STAGE_IDS
        }
        return self._finish_diagnostics(
            path_type=path_type,
            source=source,
            status=status,
            started_at=started_at,
            started=started_value,
            stats=stats,
            control=control,
            error=error,
        )

    def _empty_stats(self) -> dict[str, StageDiagnostic]:
        return {
            stage_id: StageDiagnostic(
                stage=stage_id,
                display_name=STAGE_SPECS[stage_id].display_name,
                status=self._statuses[stage_id],
            )
            for stage_id in STAGE_IDS
        }

    @staticmethod
    def _exception_payload(stage_id: str, exc: Exception) -> dict[str, Any]:
        if isinstance(exc, WorkbenchError):
            payload = exc.to_dict()
            payload.setdefault("stage", stage_id)
            return payload
        return {
            "code": "E_STAGE",
            "message": f"{type(exc).__name__}: {exc}",
            "stage": stage_id,
        }

    @staticmethod
    def _emit(listener: EventListener | None, event: dict[str, Any]) -> None:
        if listener is None:
            return
        try:
            listener(event)
        except Exception:
            pass


__all__ = [
    "PipelineRun",
    "PipelineSession",
    "ResourceLimits",
    "RestoreResult",
    "RunControl",
]
