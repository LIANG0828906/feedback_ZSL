"""Background solve QThread."""

from __future__ import annotations

import threading

from PyQt5.QtCore import QThread, pyqtSignal

from ..errors import BusinessError
from ..pipeline import SolvePipeline
from ..solver import SolveCache


class SolveWorker(QThread):
    progress = pyqtSignal(object)
    completed = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(
        self,
        pipeline: SolvePipeline,
        *,
        resume: bool = False,
        cache: SolveCache | None = None,
        parent=None,
    ):
        super().__init__(parent)
        self.pipeline = pipeline
        self.resume = resume
        self.cache = cache
        self.cancel_event = threading.Event()
        self.pause_event = threading.Event()

    def run(self) -> None:
        try:
            report = self.pipeline.run(
                resume=self.resume,
                cancel_event=self.cancel_event,
                pause_event=self.pause_event,
                progress=self.progress.emit,
                cache=self.cache,
            )
            self.completed.emit(report)
        except BusinessError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.failed.emit(
                {"code": "E_INTERNAL", "message": f"{type(exc).__name__}: {exc}"}
            )

    def request_pause(self, paused: bool) -> None:
        if paused:
            self.pause_event.set()
        else:
            self.pause_event.clear()

    def request_cancel(self) -> None:
        self.cancel_event.set()
        self.pause_event.clear()

