"""Qt worker objects for session runs and full-cache audits."""

from __future__ import annotations

from typing import Callable

from PySide6.QtCore import QObject, Signal, Slot


class SessionWorker(QObject):
    event = Signal(object)
    completed = Signal(str, object)
    failed = Signal(str, object)

    def __init__(
        self,
        task_name: str,
        action: Callable[[Callable[[dict], None]], object],
    ) -> None:
        super().__init__()
        self._task_name = task_name
        self._action = action

    @Slot()
    def run(self) -> None:
        try:
            result = self._action(self.event.emit)
        except Exception as exc:
            self.failed.emit(
                self._task_name,
                {
                    "code": "E_WORKER",
                    "message": f"{type(exc).__name__}: {exc}",
                },
            )
            return
        self.completed.emit(self._task_name, result)

