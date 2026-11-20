"""并发控制与协作式取消。"""

from __future__ import annotations

import threading

from .errors import BusyError, CancelledError


class CancelToken:
    def __init__(self):
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def check(self) -> None:
        if self._event.is_set():
            raise CancelledError("任务被协作式取消")


class TaskManager:
    """同一测量同时只允许一个增强任务；重复启动返回 E_BUSY。"""

    def __init__(self):
        self._lock = threading.Lock()
        self._active = None

    def try_begin(self) -> CancelToken:
        with self._lock:
            if self._active is not None:
                raise BusyError("同一测量已有增强任务在运行")
            self._active = CancelToken()
            return self._active

    def end(self, token: CancelToken) -> None:
        with self._lock:
            if self._active is token:
                self._active = None
