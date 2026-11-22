"""Resumable parallel segment engine and deterministic full-recompute path."""

from __future__ import annotations

import json
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .audio_io import AudioData, write_wav_atomic
from .checkpoint import CheckpointStore, LoadInfo, SnapshotIdentity
from .config import RepairConfig, SessionConfig
from .dsp import DSPResult, denoise_segment
from .errors import AppError, param_error, state_error
from .metrics import AudioStats, audio_stats
from .segments import (
    SegmentRecord,
    SegmentSpec,
    SegmentStateMachine,
    SegmentStatus,
    split_segments,
    stitch_segment_outputs,
)


class RunStatus(str, Enum):
    IDLE = "IDLE"
    RUNNING = "RUNNING"
    PAUSED = "PAUSED"
    STOPPED = "STOPPED"
    COMPLETED = "COMPLETED"
    INCOMPLETE = "INCOMPLETE"
    FAILED = "FAILED"


@dataclass(frozen=True)
class ProgressEvent:
    status: RunStatus
    committed: int
    total: int
    resumed: int
    computed_this_run: int
    failed_indices: tuple[int, ...]
    message: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status.value,
            "committed": self.committed,
            "total": self.total,
            "resumed": self.resumed,
            "computed_this_run": self.computed_this_run,
            "failed_indices": list(self.failed_indices),
            "message": self.message,
        }


@dataclass(frozen=True)
class SessionResult:
    status: RunStatus
    committed: int
    total: int
    resumed: int
    computed_this_run: int
    failed_indices: tuple[int, ...]
    complete: bool
    elapsed_seconds: float
    checkpoint_dir: str
    message: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status.value,
            "committed": self.committed,
            "total": self.total,
            "resumed": self.resumed,
            "computed_this_run": self.computed_this_run,
            "failed_indices": list(self.failed_indices),
            "complete": self.complete,
            "elapsed_seconds": self.elapsed_seconds,
            "checkpoint_dir": self.checkpoint_dir,
            "message": self.message,
        }


@dataclass(frozen=True)
class SegmentWorkResult:
    spec: SegmentSpec
    config: RepairConfig
    dsp: DSPResult


def process_segment(
    audio: AudioData, spec: SegmentSpec, config: RepairConfig
) -> SegmentWorkResult:
    chunk = audio.slice(spec.start_sample, spec.end_sample)
    result = denoise_segment(chunk, config)
    return SegmentWorkResult(spec=spec, config=config, dsp=result)


def _error_payload(exc: BaseException, *, segment_index: int | None = None) -> dict[str, Any]:
    if isinstance(exc, AppError):
        payload = exc.to_dict()
    else:
        payload = {
            "ok": False,
            "code": "E_INTERNAL",
            "message": "segment processing failed",
            "details": {"reason": str(exc)},
        }
    if segment_index is not None:
        payload.setdefault("details", {})
        payload["details"]["segment_index"] = segment_index
    return payload


class RepairSession:
    """A deterministic segment scheduler with durable commit boundaries."""

    def __init__(
        self,
        audio: AudioData,
        repair_config: RepairConfig,
        session_config: SessionConfig,
        checkpoint_dir: str | Path,
        *,
        clear_checkpoint: bool = False,
        progress_callback: Callable[[ProgressEvent], None] | None = None,
        segment_callback: Callable[[SegmentRecord], None] | None = None,
    ) -> None:
        self.audio = audio
        self.repair_config = repair_config.validate()
        self.session_config = session_config.validate(audio.channels)
        self.specs = split_segments(audio.total_samples, self.session_config.segment_samples)
        self.checkpoint_dir = Path(checkpoint_dir)
        self._progress_callback = progress_callback
        self._segment_callback = segment_callback
        self._config_lock = threading.RLock()
        self._session_lock = threading.RLock()
        self._run_lock = threading.Lock()
        self._stop_event = threading.Event()
        self._pause_event = threading.Event()
        self._status = RunStatus.IDLE
        self._computed_this_run = 0
        self._resumed_count = 0
        self._last_message = ""
        self._result: SessionResult | None = None

        identity = SnapshotIdentity.from_audio(
            audio, self.session_config, len(self.specs)
        )
        self.store = CheckpointStore(self.checkpoint_dir, identity, self.repair_config)
        if clear_checkpoint:
            self.store.reset()
        load_info, loaded = self.store.inspect_and_load()
        self.load_info = load_info
        if load_info.state in ("new", "invalidated"):
            self.store.create()
        self.state_machine = SegmentStateMachine(self.specs)
        self._restore_loaded(loaded)

    def _restore_loaded(self, loaded: dict[int, Any]) -> None:
        if not loaded:
            return
        invalid_reason = ""
        for index, segment in loaded.items():
            if index < 0 or index >= len(self.specs):
                invalid_reason = f"committed segment index {index} is out of range"
                break
            if segment.spec != self.specs[index]:
                invalid_reason = f"committed segment {index} boundary mismatch"
                break
        if invalid_reason:
            moved = self.store.invalidate_current(invalid_reason)
            self.store.create()
            self.load_info = LoadInfo(
                state="invalidated",
                valid=False,
                reason=invalid_reason,
                invalidated_to=moved,
            )
            return
        for index in sorted(loaded):
            segment = loaded[index]
            self.state_machine.restore_committed(
                index,
                config_fingerprint=segment.config_fingerprint,
                output_sha256=segment.output_sha256,
            )
            if self._segment_callback is not None:
                self._segment_callback(self.state_machine.get(index))
        self._resumed_count = len(loaded)

    def set_repair_config(self, config: RepairConfig) -> None:
        validated = config.validate()
        with self._config_lock:
            self.repair_config = validated

    def set_callbacks(
        self,
        *,
        progress_callback: Callable[[ProgressEvent], None] | None = None,
        segment_callback: Callable[[SegmentRecord], None] | None = None,
    ) -> None:
        self._progress_callback = progress_callback
        self._segment_callback = segment_callback

    def current_config(self) -> RepairConfig:
        with self._config_lock:
            return self.repair_config

    def set_resource_limits(self, max_workers: int, max_buffer_bytes: int) -> None:
        with self._session_lock:
            current = self.session_config
            self.session_config = SessionConfig(
                segment_samples=current.segment_samples,
                max_workers=max_workers,
                max_buffer_bytes=max_buffer_bytes,
            ).validate(self.audio.channels)

    def request_pause(self) -> None:
        self._pause_event.set()

    def request_resume(self) -> None:
        self._pause_event.clear()

    def request_stop(self) -> None:
        self._stop_event.set()
        self._pause_event.clear()

    @property
    def status(self) -> RunStatus:
        return self._status

    @property
    def result(self) -> SessionResult | None:
        return self._result

    def _failed_indices(self) -> tuple[int, ...]:
        return tuple(
            self.state_machine.indexes_with({SegmentStatus.FAILED})
        )

    def _emit_progress(self, message: str) -> None:
        committed, total = self.state_machine.progress()
        event = ProgressEvent(
            status=self._status,
            committed=committed,
            total=total,
            resumed=self._resumed_count,
            computed_this_run=self._computed_this_run,
            failed_indices=self._failed_indices(),
            message=message,
        )
        if self._progress_callback is not None:
            self._progress_callback(event)

    def _wait_if_paused(self) -> bool:
        if not self._pause_event.is_set():
            return not self._stop_event.is_set()
        self._status = RunStatus.PAUSED
        self._emit_progress("Paused at a committed segment boundary")
        while self._pause_event.is_set() and not self._stop_event.is_set():
            self._stop_event.wait(0.05)
        if self._stop_event.is_set():
            self._status = RunStatus.STOPPED
            self._emit_progress("Stopped; only durable committed segments are recoverable")
            return False
        self._status = RunStatus.RUNNING
        self._emit_progress("Resumed from durable segments")
        return True

    def _submit_batch(
        self,
        executor: ThreadPoolExecutor,
        indexes: list[int],
        configs: list[RepairConfig],
    ) -> dict[int, Future[SegmentWorkResult]]:
        futures: dict[int, Future[SegmentWorkResult]] = {}
        for index, config in zip(indexes, configs, strict=True):
            record = self.state_machine.transition(index, SegmentStatus.PROCESSING)
            if self._segment_callback is not None:
                self._segment_callback(record)
            futures[index] = executor.submit(
                process_segment, self.audio, self.specs[index], config
            )
        self._emit_progress(f"Processing {len(indexes)} segment(s) in parallel")
        return futures

    def _commit_future(
        self, index: int, future: Future[SegmentWorkResult]
    ) -> bool:
        try:
            work = future.result()
        except BaseException as exc:  # noqa: BLE001 - isolate a single failed segment
            failure = _error_payload(exc, segment_index=index)
            try:
                record = self.state_machine.mark_failed(index, failure)
            except AppError:
                record = self.state_machine.get(index)
            if self._segment_callback is not None:
                self._segment_callback(record)
            self._emit_progress(f"Segment {index} failed; retry is available")
            return False

        try:
            digest = self.store.save_segment(
                work.spec,
                work.dsp.output,
                work.config,
                dsp_details={
                    "mean_gain": work.dsp.mean_gain,
                    "attenuation_db": work.dsp.attenuation_db,
                    "noise_power_mean": work.dsp.noise_power_mean,
                },
            )
            record = self.state_machine.mark_committed(
                index,
                config_fingerprint=work.config.fingerprint(),
                output_sha256=digest,
            )
        except BaseException as exc:  # noqa: BLE001
            failure = _error_payload(exc, segment_index=index)
            try:
                record = self.state_machine.mark_failed(index, failure)
            except AppError:
                record = self.state_machine.get(index)
            if self._segment_callback is not None:
                self._segment_callback(record)
            self._emit_progress(f"Segment {index} could not be committed")
            return False
        if self._segment_callback is not None:
            self._segment_callback(record)
        self._computed_this_run += 1
        self._emit_progress(f"Committed segment {index}")
        return True

    def run(
        self,
        *,
        retry_failed: bool = False,
        max_new_segments: int | None = None,
    ) -> SessionResult:
        """Process pending segments until completion, stop or pause.

        Concurrency never owns checkpoint writes.  Futures are committed on the
        session thread in ascending segment order, which keeps checkpoint
        ordering and export ordering stable.
        """

        if not self._run_lock.acquire(blocking=False):
            raise state_error("a processing run is already active")
        started = time.perf_counter()
        self._computed_this_run = 0
        self._stop_event.clear()
        retry_available = set(
            self.state_machine.indexes_with({SegmentStatus.FAILED})
            if retry_failed
            else []
        )
        try:
            self._status = RunStatus.RUNNING
            self._emit_progress("Processing started")
            while True:
                if not self._wait_if_paused():
                    break
                pending = self.state_machine.indexes_with({SegmentStatus.PENDING})
                candidates = sorted(set(pending) | retry_available)
                if not candidates:
                    break
                if (
                    max_new_segments is not None
                    and self._computed_this_run >= max_new_segments
                ):
                    break
                remaining_budget = len(candidates)
                if max_new_segments is not None:
                    remaining_budget = max(
                        1, max_new_segments - self._computed_this_run
                    )
                batch_size = min(
                    self.session_config.max_workers,
                    len(candidates),
                    remaining_budget,
                )
                batch = candidates[:batch_size]
                configs = []
                for _index in batch:
                    configs.append(self.current_config())
                retry_available.difference_update(batch)
                with ThreadPoolExecutor(
                    max_workers=batch_size, thread_name_prefix="repair-segment"
                ) as executor:
                    futures = self._submit_batch(executor, batch, configs)
                    for index in batch:
                        self._commit_future(index, futures[index])
                if self._stop_event.is_set():
                    self._status = RunStatus.STOPPED
                    break
                if self._pause_event.is_set():
                    continue
                if (
                    max_new_segments is not None
                    and self._computed_this_run >= max_new_segments
                ):
                    break

            committed, total = self.state_machine.progress()
            failures = self._failed_indices()
            if self._status == RunStatus.STOPPED:
                message = "Stopped. No completion report was produced; resume uses committed segments only."
            elif committed == total:
                self._status = RunStatus.COMPLETED
                message = "All segments committed and verified"
            elif failures and not self.state_machine.indexes_with(
                {SegmentStatus.PENDING, SegmentStatus.PROCESSING}
            ):
                self._status = RunStatus.FAILED
                message = "Processing ended with failed segments; retry is available"
            elif self._pause_event.is_set():
                self._status = RunStatus.PAUSED
                message = "Paused at a committed segment boundary"
            else:
                self._status = RunStatus.INCOMPLETE
                message = "Run stopped before all segments committed"
            complete = committed == total
            result = SessionResult(
                status=self._status,
                committed=committed,
                total=total,
                resumed=self._resumed_count,
                computed_this_run=self._computed_this_run,
                failed_indices=failures,
                complete=complete,
                elapsed_seconds=time.perf_counter() - started,
                checkpoint_dir=str(self.checkpoint_dir),
                message=message,
            )
            self._result = result
            self._last_message = message
            self._emit_progress(message)
            return result
        finally:
            self._run_lock.release()

    def collect_outputs(self) -> np.ndarray:
        records = self.state_machine.snapshot()
        not_committed = [
            index
            for index, record in records.items()
            if record.status != SegmentStatus.COMMITTED
        ]
        if not_committed:
            raise state_error(
                "cannot assemble output before every segment is committed",
                uncommitted=not_committed,
                committed=self.state_machine.committed_count(),
                total=self.state_machine.total_count,
            )
        loaded = self.store.load_all_committed()
        outputs = {index: segment.output for index, segment in loaded.items()}
        return stitch_segment_outputs(
            outputs,
            self.specs,
            self.audio.total_samples,
            self.audio.channels,
        )

    def output_stats(self) -> AudioStats:
        return audio_stats(self.collect_outputs(), self.audio.sample_rate)

    def export(self, path: str | Path) -> AudioStats:
        output = self.collect_outputs()
        write_wav_atomic(path, output, self.audio.sample_rate, sample_dtype="float32")
        return audio_stats(output, self.audio.sample_rate)

    def preview_segment(
        self, index: int, config: RepairConfig | None = None
    ) -> tuple[np.ndarray, np.ndarray, DSPResult]:
        """Compute a non-persisted preview without changing segment state."""

        if index < 0 or index >= len(self.specs):
            raise param_error("segment index is out of range", segment_index=index)
        record = self.state_machine.get(index)
        original = self.audio.slice(
            self.specs[index].start_sample, self.specs[index].end_sample
        )
        if record.status == SegmentStatus.COMMITTED and config is None:
            repaired = self.store.load_segment(index).output
            return original, repaired, DSPResult(
                output=repaired,
                mean_gain=float("nan"),
                attenuation_db=float("nan"),
                noise_power_mean=float("nan"),
            )
        if record.status in (SegmentStatus.PROCESSING,):
            raise state_error(
                "cannot preview a segment while it is processing",
                segment_index=index,
            )
        active = (config or self.current_config()).validate()
        result = denoise_segment(original, active)
        return original, result.output, result

    def state_snapshot(self) -> dict[int, SegmentRecord]:
        return self.state_machine.snapshot()

    def diagnostics(self) -> dict[str, Any]:
        return {
            "audio": self.audio.identity_dict(),
            "checkpoint_dir": str(self.checkpoint_dir),
            "load_info": self.load_info.to_dict(),
            "segment_samples": self.session_config.segment_samples,
            "max_workers": self.session_config.max_workers,
            "max_buffer_bytes": self.session_config.max_buffer_bytes,
            "total_segments": len(self.specs),
            "status": self._status.value,
            "result": self._result.to_dict() if self._result else None,
        }


def process_full_recompute(
    audio: AudioData,
    repair_config: RepairConfig,
    session_config: SessionConfig,
    *,
    progress_callback: Callable[[int, int], None] | None = None,
) -> np.ndarray:
    """One-shot path: no checkpoint read/write, identical per-segment DSP."""

    config = repair_config.validate()
    session = session_config.validate(audio.channels)
    specs = split_segments(audio.total_samples, session.segment_samples)
    outputs: dict[int, np.ndarray] = {}
    cursor = 0
    while cursor < len(specs):
        batch = specs[cursor : cursor + session.max_workers]
        with ThreadPoolExecutor(
            max_workers=len(batch), thread_name_prefix="repair-full"
        ) as executor:
            futures = [
                executor.submit(process_segment, audio, spec, config) for spec in batch
            ]
            for spec, future in zip(batch, futures, strict=True):
                outputs[spec.index] = future.result().dsp.output
        cursor += len(batch)
        if progress_callback is not None:
            progress_callback(cursor, len(specs))
    return stitch_segment_outputs(outputs, specs, audio.total_samples, audio.channels)


def process_realtime_with_checkpoint(
    audio: AudioData,
    repair_config: RepairConfig,
    session_config: SessionConfig,
    checkpoint_dir: str | Path,
    *,
    clear_checkpoint: bool = True,
    progress_callback: Callable[[ProgressEvent], None] | None = None,
) -> tuple[np.ndarray, SessionResult, RepairSession]:
    """Serial commit-as-you-go path used by the acceptance comparison."""

    serial = SessionConfig(
        segment_samples=session_config.segment_samples,
        max_workers=1,
        max_buffer_bytes=session_config.max_buffer_bytes,
    )
    session = RepairSession(
        audio,
        repair_config,
        serial,
        checkpoint_dir,
        clear_checkpoint=clear_checkpoint,
        progress_callback=progress_callback,
    )
    result = session.run()
    output = session.collect_outputs() if result.complete else np.empty((0, audio.channels))
    return output, result, session


def write_session_report(path: str | Path, result: SessionResult, session: RepairSession) -> None:
    """Write only truthful completed/partial reports; never fakes force-kill data."""

    payload = {
        "report_kind": "complete" if result.complete else "partial",
        "result": result.to_dict(),
        "diagnostics": session.diagnostics(),
    }
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )

