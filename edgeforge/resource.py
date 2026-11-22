from __future__ import annotations

import threading
import time

from .errors import CancelledError


class RunControl:
    def __init__(self) -> None:
        self._pause = threading.Event()
        self._cancel = threading.Event()
        self._lock = threading.Lock()

    @property
    def paused(self) -> bool:
        return self._pause.is_set()

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def pause(self) -> None:
        self._pause.set()

    def resume(self) -> None:
        self._pause.clear()

    def cancel(self) -> None:
        with self._lock:
            self._cancel.set()
            self._pause.clear()

    def check(self) -> None:
        if self._cancel.is_set():
            raise CancelledError("运行已取消，仅保留已原子提交的检查点")

    def wait_if_paused(self) -> None:
        while self._pause.is_set() and not self._cancel.is_set():
            time.sleep(0.02)
        self.check()


class MemoryBudget:
    def __init__(self, capacity_bytes: int) -> None:
        self.capacity_bytes = int(capacity_bytes)
        self._reserved_bytes = 0
        self._peak_bytes = 0
        self._lock = threading.Lock()

    @property
    def reserved_bytes(self) -> int:
        with self._lock:
            return self._reserved_bytes

    @property
    def peak_bytes(self) -> int:
        with self._lock:
            return self._peak_bytes

    def try_reserve(self, amount: int) -> bool:
        if amount < 0:
            raise ValueError("amount must be non-negative")
        with self._lock:
            if self._reserved_bytes + amount > self.capacity_bytes:
                return False
            self._reserved_bytes += amount
            self._peak_bytes = max(self._peak_bytes, self._reserved_bytes)
            return True

    def release(self, amount: int) -> None:
        with self._lock:
            self._reserved_bytes = max(0, self._reserved_bytes - amount)

