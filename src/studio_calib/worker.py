from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

from .config import CalibrationConfig
from .errors import CalibrationError
from .pipeline import CalibrationPipeline, ProcessResult


@dataclass
class JobCompletion:
    status: str
    result: ProcessResult | None
    error: CalibrationError | None
    progress_stage: str
    progress_fraction: float


class ProcessingCoordinator:
    """Run one processing job at a time and publish only complete results."""

    def __init__(self, pipeline: CalibrationPipeline) -> None:
        self.pipeline = pipeline
        self._lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._cancel_event: threading.Event | None = None
        self._last_result: ProcessResult | None = None
        self._completion: JobCompletion | None = None
        self._progress_stage = "idle"
        self._progress_fraction = 0.0

    @property
    def is_running(self) -> bool:
        with self._lock:
            return self._thread is not None and self._thread.is_alive()

    @property
    def last_result(self) -> ProcessResult | None:
        with self._lock:
            return self._last_result

    @property
    def progress(self) -> tuple[str, float]:
        with self._lock:
            return self._progress_stage, self._progress_fraction

    def submit(self, wav_path: str, config: CalibrationConfig) -> None:
        with self._lock:
            if self._thread is not None and self._thread.is_alive():
                raise CalibrationError(
                    "E_BUSY", "a processing job is already running", "measurement"
                )
            cancel_event = threading.Event()
            self._cancel_event = cancel_event
            self._completion = None
            self._progress_stage = "queued"
            self._progress_fraction = 0.0
            previous = self._last_result
            thread = threading.Thread(
                target=self._run_job,
                args=(wav_path, config, previous, cancel_event),
                name="studio-calibration-worker",
                daemon=True,
            )
            self._thread = thread
            thread.start()

    def _run_job(
        self,
        wav_path: str,
        config: CalibrationConfig,
        previous: ProcessResult | None,
        cancel_event: threading.Event,
    ) -> None:
        def progress(stage: str, fraction: float) -> None:
            with self._lock:
                self._progress_stage = stage
                self._progress_fraction = fraction

        try:
            result = self.pipeline.run(
                wav_path,
                config,
                previous_result=previous,
                cancel_event=cancel_event,
                progress_callback=progress,
            )
        except CalibrationError as exc:
            with self._lock:
                self._completion = JobCompletion(
                    status="cancelled" if exc.code == "E_CANCELLED" else "error",
                    result=None,
                    error=exc,
                    progress_stage=self._progress_stage,
                    progress_fraction=self._progress_fraction,
                )
        except Exception as exc:  # pragma: no cover - defensive boundary
            error = CalibrationError("E_INTERNAL", f"unexpected worker failure: {exc}")
            with self._lock:
                self._completion = JobCompletion(
                    status="error",
                    result=None,
                    error=error,
                    progress_stage=self._progress_stage,
                    progress_fraction=self._progress_fraction,
                )
        else:
            with self._lock:
                self._last_result = result
                self._completion = JobCompletion(
                    status="committed",
                    result=result,
                    error=None,
                    progress_stage="complete",
                    progress_fraction=1.0,
                )
        finally:
            with self._lock:
                current = self._thread
                if current is not None and current is threading.current_thread():
                    self._thread = None

    def cancel(self) -> bool:
        with self._lock:
            if self._thread is None or not self._thread.is_alive():
                return False
            if self._cancel_event is not None:
                self._cancel_event.set()
            self._progress_stage = "cancelling"
            return True

    def take_completion(self) -> JobCompletion | None:
        with self._lock:
            completion = self._completion
            self._completion = None
            return completion

