"""Transactional mixing, slicing, incremental updates and recovery."""

from __future__ import annotations

import hashlib
import os
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .errors import (
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CONFIG,
    E_IO,
    E_MIX_INPUT,
    E_RECOVERY_INVALID,
    E_SLICE_GAP,
    MixError,
)
from .index import (
    INDEX_FILENAME,
    atomic_write_json,
    genesis_chain,
    load_index,
    rebuild_chain,
    seal_index,
    slice_chain_hash,
    validate_index,
)
from .mixer import LoadedTrack, Mixer, plan_task_memory
from .models import (
    MixConfig,
    TrackSpec,
    changed_mute_intervals,
)
from .trace import TraceWriter
from .wavio import atomic_write_wav, content_sha256, wav_bytes


@dataclass
class ControlToken:
    """Cooperative pause/cancel control shared with the workbench thread."""

    cancel_event: threading.Event = field(default_factory=threading.Event)
    pause_event: threading.Event = field(default_factory=threading.Event)
    _paused_reported: bool = False

    def pause(self) -> None:
        self.pause_event.set()

    def resume(self) -> None:
        self.pause_event.clear()

    def cancel(self) -> None:
        self.cancel_event.set()
        self.pause_event.clear()

    @property
    def cancelled(self) -> bool:
        return self.cancel_event.is_set()

    def checkpoint(self) -> None:
        if self.cancel_event.is_set():
            raise _RunStopped("cancelled")
        while self.pause_event.is_set():
            if self.cancel_event.is_set():
                raise _RunStopped("cancelled")
            time.sleep(0.02)


class _RunStopped(Exception):
    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


@dataclass
class SliceTask:
    index: int
    start_sample: int
    end_sample: int
    priority: int = 0

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample


@dataclass
class TaskOutcome:
    task: SliceTask
    pcm: np.ndarray | None = None
    error: MixError | None = None
    block_samples: int = 0
    estimated_bytes: int = 0


@dataclass
class RunResult:
    job_id: str
    output_dir: Path
    index_path: Path
    trace_path: Path
    index: dict[str, Any] | None
    canceled: bool = False
    stopped_early: bool = False
    stop_reason: str | None = None

    @property
    def summary(self) -> dict[str, Any]:
        return summarize_index(self.index or {})

    @property
    def ok(self) -> bool:
        return bool(self.index and self.index.get("complete") and not self.canceled)


def summarize_index(index: dict[str, Any]) -> dict[str, Any]:
    slices = index.get("slices", [])
    return {
        "boundaries": [
            [int(item["start_sample"]), int(item["end_sample"]), int(item["sample_count"])]
            for item in slices
        ],
        "chain_head": index.get("chain_head"),
        "committed_slices": int(index.get("committed_slices", len(slices))),
        "complete": bool(index.get("complete", False)),
        "content_hashes": [str(item["content_sha256"]) for item in slices],
        "index_version": int(index.get("index_version", 0)),
        "total_samples": int(index.get("total_samples", 0)),
        "total_slices": int(index.get("total_slices", 0)),
    }


def _error_from_exception(exc: BaseException, slice_index: int | None = None) -> MixError:
    if isinstance(exc, MixError):
        if exc.slice_index is None and slice_index is not None:
            exc.slice_index = slice_index
        return exc
    return MixError(
        E_IO,
        "slice task failed",
        slice_index=slice_index,
        details={"exception_type": type(exc).__name__, "text": str(exc)},
    )


class MixEngine:
    """One configured mix job with a process-wide E_BUSY guard."""

    _active_lock = threading.Lock()
    _active_outputs: set[str] = set()

    def __init__(self, config: MixConfig):
        self.config = config
        self._last_error: MixError | None = None
        self._last_result: RunResult | None = None
        self._thread: threading.Thread | None = None
        self._control: ControlToken | None = None

    @classmethod
    def from_file(
        cls,
        path: str | os.PathLike[str],
        *,
        output_dir_override: str | os.PathLike[str] | None = None,
        trace_override: str | os.PathLike[str] | None = None,
    ) -> "MixEngine":
        return cls(
            MixConfig.from_file(
                path,
                output_dir_override=output_dir_override,
                trace_override=trace_override,
            )
        )

    @property
    def output_dir(self) -> Path:
        return self.config.output_path

    @property
    def index_path(self) -> Path:
        return self.output_dir / INDEX_FILENAME

    @property
    def last_error(self) -> MixError | None:
        return self._last_error

    @property
    def last_result(self) -> RunResult | None:
        return self._last_result

    @property
    def thread(self) -> threading.Thread | None:
        return self._thread

    @classmethod
    def active_jobs(cls) -> list[str]:
        with cls._active_lock:
            return sorted(cls._active_outputs)

    def _busy_key(self) -> str:
        try:
            return str(self.output_dir.resolve())
        except OSError:
            return str(self.output_dir.absolute())

    def _acquire(self) -> str:
        key = self._busy_key()
        with self._active_lock:
            if key in self._active_outputs:
                raise MixError(E_BUSY, "the same mix job is already running", details={"job_id": self.config.job_id})
            self._active_outputs.add(key)
        return key

    def _release(self, key: str) -> None:
        with self._active_lock:
            self._active_outputs.discard(key)

    def start_async(
        self,
        *,
        trace_path: str | os.PathLike[str] | None = None,
        append_trace: bool | None = None,
        resume: bool = True,
        fresh: bool = False,
        max_slices: int | None = None,
        control: ControlToken | None = None,
        on_event: Callable[[dict[str, Any]], None] | None = None,
    ) -> threading.Thread:
        if self._thread is not None and self._thread.is_alive():
            raise MixError(E_BUSY, "the same engine instance is already running")
        token = control or ControlToken()
        self._control = token
        reserved_key = self._acquire()

        def target() -> None:
            try:
                self.run(
                    trace_path=trace_path,
                    append_trace=append_trace,
                    resume=resume,
                    fresh=fresh,
                    max_slices=max_slices,
                    control=token,
                    on_event=on_event,
                    _reserved_key=reserved_key,
                )
            except MixError as exc:
                self._last_error = exc

        try:
            thread = threading.Thread(target=target, name=f"mixrec-{self.config.job_id}", daemon=True)
            self._thread = thread
            thread.start()
        except BaseException:
            self._release(reserved_key)
            raise
        return thread

    def pause(self) -> None:
        if self._control is None:
            raise MixError(E_CONFIG, "no active run to pause")
        self._control.pause()

    def resume_run(self) -> None:
        if self._control is None:
            raise MixError(E_CONFIG, "no active run to resume")
        self._control.resume()

    def cancel(self) -> None:
        if self._control is None:
            raise MixError(E_CONFIG, "no active run to cancel")
        self._control.cancel()

    def run(
        self,
        *,
        trace_path: str | os.PathLike[str] | None = None,
        append_trace: bool | None = None,
        resume: bool = True,
        fresh: bool = False,
        max_slices: int | None = None,
        control: ControlToken | None = None,
        on_event: Callable[[dict[str, Any]], None] | None = None,
        _reserved_key: str | None = None,
    ) -> RunResult:
        if max_slices is not None and max_slices < 0:
            raise MixError(E_CONFIG, "max_slices must be non-negative")
        key = _reserved_key if _reserved_key is not None else self._acquire()
        trace_file = Path(trace_path) if trace_path is not None else self.config.trace_file
        effective_append = (
            trace_file.exists() and resume and not fresh
            if append_trace is None
            else bool(append_trace)
        )
        trace = TraceWriter(trace_file, append=effective_append, listener=on_event)
        token = control or ControlToken()
        self._last_error = None
        try:
            return self._run_locked(trace, token, resume=resume, fresh=fresh, max_slices=max_slices)
        except _RunStopped as stopped:
            result = RunResult(
                job_id=self.config.job_id,
                output_dir=self.output_dir,
                index_path=self.index_path,
                trace_path=trace_file,
                index=self._read_index_optional(),
                canceled=stopped.reason == "cancelled",
                stopped_early=True,
                stop_reason=stopped.reason,
            )
            trace.emit(
                "run_stopped",
                job_id=self.config.job_id,
                reason=stopped.reason,
                committed_slices=int((result.index or {}).get("committed_slices", 0)),
            )
            self._last_result = result
            return result
        except MixError as exc:
            self._last_error = exc
            failed_index = self._read_index_optional()
            if failed_index is not None:
                self._last_result = RunResult(
                    job_id=self.config.job_id,
                    output_dir=self.output_dir,
                    index_path=self.index_path,
                    trace_path=trace_file,
                    index=failed_index,
                    stopped_early=True,
                    stop_reason=exc.code,
                )
            event = {"job_id": self.config.job_id, "code": exc.code}
            if exc.slice_index is not None:
                event["slice_index"] = exc.slice_index
            if exc.track_id is not None:
                event["track_id"] = exc.track_id
            if exc.details:
                event["details"] = exc.details
            trace.emit("run_failed", **event)
            raise
        finally:
            trace.close()
            self._release(key)

    def _read_index_optional(self) -> dict[str, Any] | None:
        try:
            return load_index(self.index_path) if self.index_path.exists() else None
        except MixError:
            return None

    def _run_locked(
        self,
        trace: TraceWriter,
        control: ControlToken,
        *,
        resume: bool,
        fresh: bool,
        max_slices: int | None,
    ) -> RunResult:
        if fresh and self.index_path.exists():
            raise MixError(
                E_CONFIG,
                "fresh recording would overwrite an existing committed index",
                details={"path": str(self.index_path)},
            )
        control.checkpoint()
        mixer = Mixer(self.config)
        total_samples = self.config.resolved_total_samples()
        total_slices = (total_samples + self.config.slice_samples - 1) // self.config.slice_samples

        existing: dict[str, Any] | None = None
        if self.index_path.exists():
            if not resume:
                raise MixError(
                    E_CONFIG,
                    "output index already exists; use resume or a new output directory",
                    details={"path": str(self.index_path)},
                )
            existing = load_index(self.index_path)
            self._prepare_recovery(existing, mixer)
            self._cleanup_stale_temps()
            index = existing
            trace.emit(
                "recovery_resume",
                index_version=int(index["index_version"]),
                job_id=self.config.job_id,
                resume_slice=int(index["committed_slices"]),
            )
        else:
            if not resume:
                pass
            output = self.config.output_contract()
            genesis = genesis_chain(self.config.job_id, output)
            index = {
                "schema_version": 1,
                "job_id": self.config.job_id,
                "index_version": 0,
                "slice_samples": self.config.slice_samples,
                "total_samples": total_samples,
                "total_slices": total_slices,
                "committed_slices": 0,
                "complete": total_slices == 0,
                "genesis_chain": genesis,
                "chain_head": genesis,
                "params_fingerprint": self.config.params_fingerprint(),
                "source_hashes": mixer.source_hashes,
                "track_versions": self.config.track_versions(),
                "tracks": [track.to_index_dict() for track in self.config.tracks],
                "output": output,
                "slices": [],
            }

        start_slice = int(index["committed_slices"])
        trace.emit(
            "job_start",
            job_id=self.config.job_id,
            params_fingerprint=self.config.params_fingerprint(),
            resume_slice=start_slice,
            slice_samples=self.config.slice_samples,
            total_samples=total_samples,
            total_slices=total_slices,
        )
        if index.get("complete"):
            if not self.index_path.exists():
                seal_index(index)
                atomic_write_json(self.index_path, index)
            trace.emit(
                "run_complete",
                committed_slices=start_slice,
                complete=True,
                index_version=int(index["index_version"]),
                job_id=self.config.job_id,
                total_slices=total_slices,
            )
            result = RunResult(
                job_id=self.config.job_id,
                output_dir=self.output_dir,
                index_path=self.index_path,
                trace_path=trace.path,
                index=index,
            )
            self._last_result = result
            return result

        self._run_initial_tasks(
            index,
            mixer,
            trace,
            control,
            start_slice=start_slice,
            total_slices=total_slices,
            max_slices=max_slices,
        )

        final = self._read_index_optional()
        if final is None:
            final = index
        if final.get("complete"):
            trace.emit(
                "run_complete",
                committed_slices=int(final["committed_slices"]),
                complete=True,
                index_version=int(final["index_version"]),
                job_id=self.config.job_id,
                total_slices=int(final["total_slices"]),
            )
            result = RunResult(
                job_id=self.config.job_id,
                output_dir=self.output_dir,
                index_path=self.index_path,
                trace_path=trace.path,
                index=final,
            )
        else:
            result = RunResult(
                job_id=self.config.job_id,
                output_dir=self.output_dir,
                index_path=self.index_path,
                trace_path=trace.path,
                index=final,
                stopped_early=True,
                stop_reason="max_slices",
            )
        self._last_result = result
        return result

    def _prepare_recovery(self, index: dict[str, Any], mixer: Mixer) -> None:
        try:
            validate_index(index, output_dir=self.output_dir, verify_files=True, recovery=True)
        except MixError:
            raise
        output = self.config.output_contract()
        if index.get("job_id") != self.config.job_id:
            raise MixError(E_RECOVERY_INVALID, "recovered index belongs to another job")
        if index.get("output") != output:
            raise MixError(
                E_RECOVERY_INVALID,
                "recovered output contract does not match configuration",
                details={"index_output": index.get("output"), "config_output": output},
            )
        expected_genesis = genesis_chain(self.config.job_id, output)
        if index.get("genesis_chain") != expected_genesis:
            raise MixError(E_RECOVERY_INVALID, "recovered index genesis is invalid")

        # The index is the authoritative parameter snapshot.  This allows a
        # process killed after an incremental publish to resume even if the
        # caller still has the old config file on disk.
        if index.get("params_fingerprint") != self.config.params_fingerprint():
            raw_tracks = index.get("tracks")
            if not isinstance(raw_tracks, list):
                raise MixError(E_RECOVERY_INVALID, "recovered track parameters are missing")
            try:
                recovered_tracks = [TrackSpec.from_dict(item, i) for i, item in enumerate(raw_tracks)]
            except MixError as exc:
                raise MixError(E_RECOVERY_INVALID, "cannot restore track parameters from index") from exc
            if len(recovered_tracks) != len(self.config.tracks):
                raise MixError(E_RECOVERY_INVALID, "recovered track count does not match configuration")
            self.config.tracks = recovered_tracks
            mixer.spec = self.config
            mixer.sources = {}
            # Rebuild through the normal source loader so every recovered track
            # is validated again, including delays and channel layout.
            rebuilt = Mixer(self.config)
            mixer.sources = rebuilt.sources
        if index.get("params_fingerprint") != self.config.params_fingerprint():
            raise MixError(E_RECOVERY_INVALID, "recovered parameter fingerprint is inconsistent")
        if index.get("track_versions") != self.config.track_versions():
            raise MixError(E_RECOVERY_INVALID, "recovered track versions are inconsistent")
        if index.get("source_hashes") != mixer.source_hashes:
            raise MixError(E_RECOVERY_INVALID, "source WAV hashes changed since the recording")

    def _run_initial_tasks(
        self,
        index: dict[str, Any],
        mixer: Mixer,
        trace: TraceWriter,
        control: ControlToken,
        *,
        start_slice: int,
        total_slices: int,
        max_slices: int | None,
    ) -> None:
        artifact_version = self.config.artifact_version()
        parameter_versions = self.config.track_versions()
        cursor = start_slice
        produced = 0
        budget_error: MixError | None = None
        while cursor < total_slices:
            control.checkpoint()
            if max_slices is not None and produced >= max_slices:
                raise _RunStopped("max_slices")
            remaining = total_slices - cursor
            if max_slices is not None:
                remaining = min(remaining, max_slices - produced)
            if remaining <= 0:
                raise _RunStopped("max_slices")
            desired = min(self.config.max_workers, remaining)
            tasks: list[SliceTask] = []
            plans: dict[int, Any] = {}
            reserved = 0
            while len(tasks) < desired:
                slice_index = cursor + len(tasks)
                start = slice_index * self.config.slice_samples
                end = min((slice_index + 1) * self.config.slice_samples, self.config.resolved_total_samples())
                task = SliceTask(
                    index=slice_index,
                    start_sample=start,
                    end_sample=end,
                    priority=int(self.config.task_priorities.get(slice_index, 0)),
                )
                try:
                    plan = plan_task_memory(self.config, mixer.sources, task.sample_count)
                except MixError as exc:
                    if exc.code != E_BUDGET_EXCEEDED:
                        raise
                    exc.slice_index = slice_index
                    budget_error = exc
                    break
                global_budget = self.config.memory_budget_bytes
                if global_budget is not None and reserved + plan.estimated_bytes > global_budget:
                    trace.emit(
                        "task_waiting",
                        estimated_bytes=plan.estimated_bytes,
                        reason="global_memory_budget",
                        reserved_bytes=reserved,
                        slice_index=slice_index,
                    )
                    if not tasks:
                        budget_error = MixError(
                            E_BUDGET_EXCEEDED,
                            "single slice task cannot fit the global memory budget",
                            slice_index=slice_index,
                            details={
                                "estimate": plan.estimated_bytes,
                                "memory_budget_bytes": global_budget,
                            },
                        )
                    break
                tasks.append(task)
                plans[slice_index] = plan
                reserved += plan.estimated_bytes
            if not tasks:
                if budget_error is not None:
                    self._trace_budget_rejection(trace, budget_error)
                    raise budget_error
                raise MixError(E_IO, "scheduler made no progress", slice_index=cursor)

            for slice_index, plan in plans.items():
                if len(plan.attempts) > 1:
                    trace.emit(
                        "budget_degraded",
                        attempts=[
                            {"block_samples": block, "estimated_bytes": estimate}
                            for block, estimate in plan.attempts
                        ],
                        slice_index=slice_index,
                    )
            trace.emit(
                "memory_reserved",
                bytes=reserved,
                max_workers=self.config.max_workers,
                tasks=[task.index for task in tasks],
            )
            for task in tasks:
                plan = plans[task.index]
                trace.emit(
                    "task_started",
                    block_samples=plan.block_samples,
                    end_sample=task.end_sample,
                    estimated_bytes=plan.estimated_bytes,
                    priority=task.priority,
                    slice_index=task.index,
                    start_sample=task.start_sample,
                )

            outcomes = self._execute_tasks(tasks, plans, mixer, control)
            for outcome in outcomes:
                if outcome.error is None:
                    trace.emit(
                        "task_completed",
                        estimated_bytes=outcome.estimated_bytes,
                        slice_index=outcome.task.index,
                    )
                else:
                    trace.emit(
                        "task_failed",
                        code=outcome.error.code,
                        slice_index=outcome.task.index,
                    )

            committed_this_batch = 0
            first_task_error: MixError | None = None
            for outcome in outcomes:
                if outcome.error is not None:
                    if first_task_error is None:
                        first_task_error = outcome.error
                    break
                expected_start = int(index["slices"][-1]["end_sample"]) if index["slices"] else 0
                if outcome.task.start_sample != expected_start:
                    first_task_error = MixError(
                        E_SLICE_GAP,
                        "slice start does not join the committed prefix",
                        slice_index=outcome.task.index,
                        details={"expected_start": expected_start, "actual_start": outcome.task.start_sample},
                    )
                    break
                try:
                    self._commit_initial_slice(
                        index,
                        outcome,
                        artifact_version=artifact_version,
                        parameter_versions=parameter_versions,
                        trace=trace,
                    )
                except MixError as exc:
                    first_task_error = exc
                    break
                committed_this_batch += 1
                produced += 1
            trace.emit("memory_released", bytes=reserved, tasks=[task.index for task in tasks])
            cursor += committed_this_batch

            if first_task_error is not None:
                if budget_error is not None:
                    self._trace_budget_rejection(trace, budget_error)
                raise first_task_error
            if budget_error is not None:
                self._trace_budget_rejection(trace, budget_error)
                raise budget_error

    def _trace_budget_rejection(self, trace: TraceWriter, error: MixError) -> None:
        details = error.details or {}
        trace.emit(
            "budget_rejected",
            estimate=details.get("estimate"),
            per_slice_budget_bytes=self.config.per_slice_budget_bytes,
            slice_index=error.slice_index,
            stage="per_slice",
        )

    def _execute_tasks(
        self,
        tasks: list[SliceTask],
        plans: dict[int, Any],
        mixer: Mixer,
        control: ControlToken,
    ) -> list[TaskOutcome]:
        outcomes: list[TaskOutcome] = []

        def work(task: SliceTask) -> TaskOutcome:
            try:
                control.checkpoint()
                pcm = mixer.render(
                    task.start_sample,
                    task.end_sample,
                    block_samples=plans[task.index].block_samples,
                )
                return TaskOutcome(
                    task=task,
                    pcm=pcm,
                    block_samples=plans[task.index].block_samples,
                    estimated_bytes=plans[task.index].estimated_bytes,
                )
            except _RunStopped:
                raise
            except BaseException as exc:
                return TaskOutcome(
                    task=task,
                    error=_error_from_exception(exc, task.index),
                    block_samples=plans[task.index].block_samples,
                    estimated_bytes=plans[task.index].estimated_bytes,
                )

        with ThreadPoolExecutor(max_workers=max(1, len(tasks)), thread_name_prefix="mixrec-slice") as pool:
            futures: list[Future[TaskOutcome]] = [pool.submit(work, task) for task in tasks]
            for future in futures:
                try:
                    outcomes.append(future.result())
                except _RunStopped:
                    raise
        # Future completion order is deliberately ignored: commit order is the
        # slice index order, never wall-clock order.
        outcomes.sort(key=lambda item: item.task.index)
        return outcomes

    def _commit_initial_slice(
        self,
        index: dict[str, Any],
        outcome: TaskOutcome,
        *,
        artifact_version: int,
        parameter_versions: dict[str, int],
        trace: TraceWriter,
    ) -> None:
        task = outcome.task
        pcm = outcome.pcm
        if pcm is None:
            raise MixError(E_IO, "slice task produced no samples", slice_index=task.index)
        if len(pcm) != task.sample_count:
            raise MixError(
                E_SLICE_GAP,
                "rendered slice sample count does not match its boundary",
                slice_index=task.index,
                details={"expected": task.sample_count, "actual": len(pcm)},
            )
        relative = f"slices/v{artifact_version:06d}/slice_{task.index:06d}.wav"
        target = self.output_dir / relative
        content_hash, file_hash = atomic_write_wav(
            target,
            pcm,
            self.config.output_rate,
            self.config.output_channels,
        )
        record: dict[str, Any] = {
            "index": task.index,
            "start_sample": task.start_sample,
            "end_sample": task.end_sample,
            "sample_count": task.sample_count,
            "content_sha256": content_hash,
            "file_sha256": file_hash,
            "file": relative,
            "artifact_version": artifact_version,
            "parameter_versions": dict(parameter_versions),
        }
        prev = str(index.get("chain_head") or index["genesis_chain"])
        record["prev_chain"] = prev
        record["chain_sha256"] = slice_chain_hash(record, genesis=str(index["genesis_chain"]), prev_chain=prev)
        index["slices"].append(record)
        index["chain_head"] = record["chain_sha256"]
        index["committed_slices"] = len(index["slices"])
        index["index_version"] = int(index["index_version"]) + 1
        index["complete"] = index["committed_slices"] >= index["total_slices"]
        seal_index(index)
        try:
            atomic_write_json(self.index_path, index)
        except MixError:
            try:
                target.unlink(missing_ok=True)
            except OSError:
                pass
            index["slices"].pop()
            index["chain_head"] = prev
            index["committed_slices"] = len(index["slices"])
            index["index_version"] = int(index["index_version"]) - 1
            index["complete"] = False
            raise
        try:
            self._save_config_snapshot(index)
        except MixError:
            trace.emit("snapshot_failed", job_id=self.config.job_id)
        trace.emit("slice_committed", slice_index=task.index)

    def _save_config_snapshot(self, index: dict[str, Any]) -> None:
        snapshot = {
            "schema_version": 1,
            "job_id": self.config.job_id,
            "output": self.config.output_contract(),
            "tracks": [track.to_index_dict() for track in self.config.tracks],
            "scheduler": {
                "max_workers": self.config.max_workers,
                "per_slice_budget_bytes": self.config.per_slice_budget_bytes,
                "memory_budget_bytes": self.config.memory_budget_bytes,
                "block_samples": self.config.block_samples,
                "task_priorities": {
                    str(key): value for key, value in sorted(self.config.task_priorities.items())
                },
            },
            "params_fingerprint": index.get("params_fingerprint"),
        }
        atomic_write_json(self.output_dir / "config_snapshot.json", snapshot)

    def _cleanup_stale_temps(self) -> None:
        """Remove temporary files only after a prefix has validated."""

        if not self.output_dir.exists():
            return
        patterns = (
            "index.json.tmp-*",
            "config_snapshot.json.tmp-*",
            "slices/**/*.tmp-*",
            "slices/**/*.stage-*",
        )
        for pattern in patterns:
            for path in self.output_dir.glob(pattern):
                if not path.is_file():
                    continue
                try:
                    path.unlink()
                except OSError:
                    pass

    def verify(self) -> dict[str, Any]:
        index = load_index(self.index_path)
        validate_index(index, output_dir=self.output_dir, verify_files=True, recovery=True)
        return index

    def update_track(
        self,
        track_id: str,
        *,
        gain_db: float | None = None,
        mute_ranges: list[list[float]] | None = None,
        delay_samples: int | None = None,
        trace_path: str | os.PathLike[str] | None = None,
        on_event: Callable[[dict[str, Any]], None] | None = None,
    ) -> dict[str, Any]:
        key = self._acquire()
        trace_file = Path(trace_path) if trace_path is not None else self.config.trace_file
        trace = TraceWriter(trace_file, append=trace_file.exists(), listener=on_event)
        try:
            return self._update_locked(
                track_id,
                gain_db=gain_db,
                mute_ranges=mute_ranges,
                delay_samples=delay_samples,
                trace=trace,
            )
        except MixError as exc:
            self._last_error = exc
            event = {"job_id": self.config.job_id, "code": exc.code}
            if exc.slice_index is not None:
                event["slice_index"] = exc.slice_index
            if exc.track_id is not None:
                event["track_id"] = exc.track_id
            trace.emit("incremental_failed", **event)
            raise
        finally:
            trace.close()
            self._release(key)

    def _update_locked(
        self,
        track_id: str,
        *,
        gain_db: float | None,
        mute_ranges: list[list[float]] | None,
        delay_samples: int | None,
        trace: TraceWriter,
    ) -> dict[str, Any]:
        index = load_index(self.index_path)
        validate_index(index, output_dir=self.output_dir, verify_files=True, recovery=True)
        if not index.get("complete"):
            raise MixError(E_RECOVERY_INVALID, "incremental update requires a complete slice index")
        if index.get("params_fingerprint") != self.config.params_fingerprint():
            raw_tracks = index.get("tracks")
            if not isinstance(raw_tracks, list) or len(raw_tracks) != len(self.config.tracks):
                raise MixError(E_RECOVERY_INVALID, "cannot restore the recorded track parameter state")
            try:
                restored = [TrackSpec.from_dict(item, i) for i, item in enumerate(raw_tracks)]
            except MixError as exc:
                raise MixError(E_RECOVERY_INVALID, "recorded track parameters are invalid") from exc
            self.config.tracks = restored
            if index.get("params_fingerprint") != self.config.params_fingerprint():
                raise MixError(E_RECOVERY_INVALID, "recorded parameter fingerprint is inconsistent")
        target_position = next((i for i, track in enumerate(self.config.tracks) if track.id == track_id), None)
        if target_position is None:
            raise MixError(E_CONFIG, "unknown track", track_id=track_id)
        old_track = self.config.tracks[target_position]
        old_index_version = int(index["index_version"])
        new_version = old_track.version + 1
        new_mutes = old_track.mute_ranges if mute_ranges is None else mute_ranges
        new_track = TrackSpec.from_dict(
            {
                "id": old_track.id,
                "path": old_track.path,
                "gain_db": old_track.gain_db if gain_db is None else gain_db,
                "mute_ranges": new_mutes,
                "delay_samples": old_track.delay_samples if delay_samples is None else delay_samples,
                "version": new_version,
                "sample_rate": old_track.sample_rate,
                "channels": old_track.channels,
                "max_delay_samples": old_track.max_delay_samples,
            },
            target_position,
        )
        gain_changed = abs(new_track.gain_db - old_track.gain_db) > 1e-15
        delay_changed = new_track.delay_samples != old_track.delay_samples
        mutes_changed = new_track.mute_ranges != old_track.mute_ranges
        changed_ranges = changed_mute_intervals(old_track, new_track, self.config.output_rate)
        if gain_changed or delay_changed:
            affected = list(range(int(index["total_slices"])))
            changed_ranges = [(0, int(index["total_samples"]))]
        elif mutes_changed:
            affected = [
                slice_index
                for slice_index in range(int(index["total_slices"]))
                if _slice_intersects_ranges(
                    slice_index,
                    self.config.slice_samples,
                    int(index["total_samples"]),
                    changed_ranges,
                )
            ]
        else:
            affected = []
        if not changed_ranges and (gain_changed or delay_changed or mutes_changed):
            changed_ranges = []
        self.config.tracks[target_position] = new_track
        mixer = Mixer(self.config)
        if index.get("source_hashes") != mixer.source_hashes:
            raise MixError(E_RECOVERY_INVALID, "source WAV hashes changed since the recording")
        new_fingerprint = self.config.params_fingerprint()
        trace.emit(
            "incremental_start",
            affected_slices=affected,
            changed_ranges=[[int(a), int(b)] for a, b in changed_ranges],
            job_id=self.config.job_id,
            new_version=new_version,
            old_version=old_track.version,
            track_id=track_id,
        )

        published = False
        try:
            new_records = self._recompute_incremental(index, mixer, affected, trace)
            old_artifact_max = max(
                (int(item.get("artifact_version", 0)) for item in index.get("slices", [])),
                default=0,
            )
            artifact_version = max(old_artifact_max + 1, self.config.artifact_version())
            parameter_versions = self.config.track_versions()
            for slice_index, outcome in new_records.items():
                task = outcome.task
                relative = f"slices/v{artifact_version:06d}/slice_{slice_index:06d}.wav"
                content_hash, file_hash = self._stage_incremental_wav(outcome, self.output_dir / relative)
                record = {
                    "index": slice_index,
                    "start_sample": task.start_sample,
                    "end_sample": task.end_sample,
                    "sample_count": task.sample_count,
                    "content_sha256": content_hash,
                    "file_sha256": file_hash,
                    "file": relative,
                    "artifact_version": artifact_version,
                    "parameter_versions": dict(parameter_versions),
                }
                index["slices"][slice_index] = record
            index["params_fingerprint"] = new_fingerprint
            index["track_versions"] = parameter_versions
            index["tracks"] = [track.to_index_dict() for track in self.config.tracks]
            index["source_hashes"] = mixer.source_hashes
            index["index_version"] = old_index_version + 1
            rebuild_chain(index)
            self._publish_incremental_staging()
            validate_index(index, output_dir=self.output_dir, verify_files=True, recovery=True)
            atomic_write_json(self.index_path, index)
            published = True
            self._incremental_final_paths = []
            try:
                self._save_config_snapshot(index)
            except MixError:
                # The published index is authoritative.  A snapshot failure is
                # reported through the trace but must not invalidate a complete
                # atomic publish.
                trace.emit("snapshot_failed", job_id=self.config.job_id)
        finally:
            if not published:
                self._cleanup_incremental_staging()
        trace.emit(
            "incremental_published",
            affected_slices=affected,
            artifact_version=artifact_version,
            index_version=int(index["index_version"]),
            job_id=self.config.job_id,
            reused_slices=int(index["total_slices"]) - len(affected),
        )
        self._last_result = RunResult(
            job_id=self.config.job_id,
            output_dir=self.output_dir,
            index_path=self.index_path,
            trace_path=trace.path,
            index=index,
        )
        return index

    def _recompute_incremental(
        self,
        index: dict[str, Any],
        mixer: Mixer,
        affected: list[int],
        trace: TraceWriter,
    ) -> dict[int, TaskOutcome]:
        outcomes: dict[int, TaskOutcome] = {}
        self._incremental_staging: list[Path] = []
        self._incremental_final_paths: list[Path] = []
        cursor = 0
        while cursor < len(affected):
            batch_indices = affected[cursor : cursor + self.config.max_workers]
            tasks: list[SliceTask] = []
            plans: dict[int, Any] = {}
            reserved = 0
            for slice_index in batch_indices:
                start = slice_index * self.config.slice_samples
                end = min((slice_index + 1) * self.config.slice_samples, int(index["total_samples"]))
                task = SliceTask(
                    index=slice_index,
                    start_sample=start,
                    end_sample=end,
                    priority=int(self.config.task_priorities.get(slice_index, 0)),
                )
                try:
                    plan = plan_task_memory(self.config, mixer.sources, task.sample_count)
                except MixError as exc:
                    exc.slice_index = slice_index
                    self._trace_budget_rejection(trace, exc)
                    raise
                if self.config.memory_budget_bytes is not None and reserved + plan.estimated_bytes > self.config.memory_budget_bytes:
                    trace.emit(
                        "task_waiting",
                        estimated_bytes=plan.estimated_bytes,
                        reason="global_memory_budget",
                        reserved_bytes=reserved,
                        slice_index=slice_index,
                    )
                    if not tasks:
                        raise MixError(
                            E_BUDGET_EXCEEDED,
                            "incremental slice cannot fit the global memory budget",
                            slice_index=slice_index,
                        )
                    break
                tasks.append(task)
                plans[slice_index] = plan
                reserved += plan.estimated_bytes
            trace.emit(
                "memory_reserved",
                bytes=reserved,
                max_workers=self.config.max_workers,
                tasks=[task.index for task in tasks],
            )
            for task in tasks:
                plan = plans[task.index]
                trace.emit(
                    "task_started",
                    block_samples=plan.block_samples,
                    end_sample=task.end_sample,
                    estimated_bytes=plan.estimated_bytes,
                    priority=task.priority,
                    slice_index=task.index,
                    start_sample=task.start_sample,
                )
            results = self._execute_tasks(tasks, plans, mixer, ControlToken())
            for outcome in results:
                if outcome.error is None:
                    trace.emit(
                        "task_completed",
                        estimated_bytes=outcome.estimated_bytes,
                        slice_index=outcome.task.index,
                    )
                    outcomes[outcome.task.index] = outcome
                else:
                    trace.emit("task_failed", code=outcome.error.code, slice_index=outcome.task.index)
            trace.emit("memory_released", bytes=reserved, tasks=[task.index for task in tasks])
            failed = next((item for item in results if item.error is not None), None)
            if failed is not None:
                raise failed.error or MixError(E_IO, "incremental slice failed", slice_index=failed.task.index)
            cursor += len(tasks)
        return outcomes

    def _stage_incremental_wav(self, outcome: TaskOutcome, target: Path) -> tuple[str, str]:
        if outcome.pcm is None:
            raise MixError(E_IO, "incremental task produced no samples", slice_index=outcome.task.index)
        if len(outcome.pcm) != outcome.task.sample_count:
            raise MixError(E_SLICE_GAP, "incremental slice size mismatch", slice_index=outcome.task.index)
        encoded = wav_bytes(outcome.pcm, self.config.output_rate, self.config.output_channels)
        target.parent.mkdir(parents=True, exist_ok=True)
        temporary = target.with_name(target.name + f".stage-{os.getpid()}")
        try:
            with temporary.open("wb") as handle:
                handle.write(encoded)
                handle.flush()
                os.fsync(handle.fileno())
        except OSError as exc:
            raise MixError(E_IO, f"cannot stage incremental slice: {exc}", slice_index=outcome.task.index) from exc
        self._incremental_staging.append(temporary)
        self._incremental_final_paths.append(target)
        return content_sha256(outcome.pcm), hashlib.sha256(encoded).hexdigest()

    def _publish_incremental_staging(self) -> None:
        try:
            for temporary in self._incremental_staging:
                final = temporary.with_name(temporary.name.split(".stage-", 1)[0])
                os.replace(temporary, final)
        except OSError as exc:
            raise MixError(E_IO, f"cannot publish incremental slice: {exc}") from exc
        self._incremental_staging = []

    def _cleanup_incremental_staging(self) -> None:
        for temporary in getattr(self, "_incremental_staging", []):
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass
        self._incremental_staging = []
        for final in getattr(self, "_incremental_final_paths", []):
            try:
                final.unlink(missing_ok=True)
            except OSError:
                pass
        self._incremental_final_paths = []


def _slice_intersects_ranges(
    slice_index: int,
    slice_samples: int,
    total_samples: int,
    ranges: Iterable[tuple[int, int]],
) -> bool:
    start = slice_index * slice_samples
    end = min((slice_index + 1) * slice_samples, total_samples)
    return any(max(start, a) < min(end, b) for a, b in ranges)
