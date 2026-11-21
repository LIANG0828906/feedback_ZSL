"""Listener-scoped non-blocking rebuild/render leases."""

from __future__ import annotations

import fcntl
import os
import re
import threading
from contextlib import contextmanager
from pathlib import Path
from typing import Dict, Iterator

from .codes import AcousticError, E_BUSY


_SAFE_ID = re.compile(r"^[A-Za-z0-9_.-]+$")


class TaskLeaseManager:
    def __init__(self, root: str | os.PathLike[str]) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self._mutex = threading.Lock()
        self._held_in_process: Dict[str, int] = {}

    def _validate_listener(self, listener_id: str) -> str:
        if not _SAFE_ID.match(listener_id):
            raise AcousticError(
                E_BUSY,
                "Listener id is not safe for a task lease.",
                injection_point="task:listener_id",
                details={"listener_id": listener_id},
            )
        return listener_id

    @contextmanager
    def acquire(self, listener_id: str, operation: str = "acoustic") -> Iterator[None]:
        listener_id = self._validate_listener(listener_id)
        with self._mutex:
            if listener_id in self._held_in_process:
                raise AcousticError(
                    E_BUSY,
                    "A rebuild or render task is already active for this listener.",
                    injection_point=f"task:listener={listener_id}",
                    details={
                        "listener_id": listener_id,
                        "requested_operation": operation,
                    },
                )
            self._held_in_process[listener_id] = 1

        lock_path = self.root / f"{listener_id}.lock"
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        locked = False
        try:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                locked = True
            except BlockingIOError as exc:
                raise AcousticError(
                    E_BUSY,
                    "A rebuild or render task is already active for this listener.",
                    injection_point=f"task:listener={listener_id}",
                    details={
                        "listener_id": listener_id,
                        "requested_operation": operation,
                    },
                ) from exc
            yield
        finally:
            if locked:
                try:
                    fcntl.flock(fd, fcntl.LOCK_UN)
                except OSError:
                    pass
            os.close(fd)
            with self._mutex:
                self._held_in_process.pop(listener_id, None)

