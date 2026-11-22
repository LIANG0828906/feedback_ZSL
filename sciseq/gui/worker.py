"""Background processing worker used by the desktop example."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from PySide6.QtCore import QObject, Signal, Slot

from ..config import PipelineConfig
from ..engine import SequenceProcessor
from ..errors import SciSeqError
from ..export import export_result, export_with_recomputation
from ..loader import load_sequence


@dataclass(frozen=True)
class JobSpec:
    kind: str
    sequence_path: Path
    checkpoint_dir: Path
    config: PipelineConfig
    export_dir: Path | None = None
    preview_result: Any = None
    reset: bool = False


class ProcessorWorker(QObject):
    progress = Signal(int, int, object)
    state_changed = Signal(int, str)
    finished = Signal(object)
    failed = Signal(object)
    done = Signal()

    def __init__(
        self,
        spec: JobSpec,
        pause_event: threading.Event,
        cancel_event: threading.Event,
    ) -> None:
        super().__init__()
        self.spec = spec
        self.pause_event = pause_event
        self.cancel_event = cancel_event

    def _callback(self, committed: int, total: int, result: Any) -> None:
        self.progress.emit(committed, total, result)

    @Slot()
    def run(self) -> None:
        try:
            sequence = load_sequence(self.spec.sequence_path)
            processor = SequenceProcessor(
                sequence,
                self.spec.config,
                checkpoint_dir=self.spec.checkpoint_dir,
                state_callback=self.state_changed.emit,
            )
            if self.spec.kind == "incremental":
                outcome = processor.run_incremental(
                    resume=True,
                    parallel=True,
                    pause_event=self.pause_event,
                    cancel_event=self.cancel_event,
                    progress_callback=self._callback,
                    reset=self.spec.reset,
                )
                self.finished.emit({"kind": "incremental", "outcome": outcome})
            elif self.spec.kind == "full":
                result = processor.run_full(
                    parallel=True,
                    progress_callback=self._callback,
                    cancel_event=self.cancel_event,
                )
                self.finished.emit({"kind": "full", "result": result})
            elif self.spec.kind == "preview":
                result = processor.run_preview(progress_callback=self._callback)
                self.finished.emit({"kind": "preview", "result": result})
            elif self.spec.kind == "export":
                if self.spec.export_dir is None:
                    raise ValueError("export directory is not configured")
                preview = self.spec.preview_result
                if preview is None:
                    result = processor.run_full(
                        parallel=True,
                        progress_callback=self._callback,
                    )
                    summary = export_result(
                        result,
                        self.spec.export_dir,
                        config=self.spec.config,
                    )
                else:
                    summary = export_with_recomputation(
                        processor,
                        preview,
                        self.spec.export_dir,
                        progress_callback=self._callback,
                    )
                self.finished.emit(
                    {
                        "kind": "export",
                        "summary": summary,
                        "path": str(self.spec.export_dir),
                    }
                )
            else:
                raise ValueError(f"unknown worker job: {self.spec.kind}")
        except SciSeqError as exc:
            self.failed.emit(exc)
        except Exception as exc:
            self.failed.emit(
                SciSeqError(
                    "E_DECODE",
                    "background processing failed",
                    field="worker",
                    details={"reason": str(exc)},
                )
            )
        finally:
            self.done.emit()

