from __future__ import annotations

import sys
import threading
import time
import tracemalloc
from dataclasses import dataclass

from .errors import MemoryLimitError


def current_rss_bytes() -> int:
    if sys.platform == "win32":
        return _windows_rss()
    try:
        import resource

        value = int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
        if sys.platform == "darwin":
            return value
        return value * 1024
    except Exception:
        return 0


def _windows_rss() -> int:
    try:
        import ctypes
        from ctypes import wintypes

        class PROCESS_MEMORY_COUNTERS(ctypes.Structure):
            _fields_ = [
                ("cb", wintypes.DWORD),
                ("PageFaultCount", wintypes.DWORD),
                ("PeakWorkingSetSize", ctypes.c_size_t),
                ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t),
                ("PeakPagefileUsage", ctypes.c_size_t),
            ]

        counters = PROCESS_MEMORY_COUNTERS()
        counters.cb = ctypes.sizeof(counters)
        process = ctypes.windll.kernel32.GetCurrentProcess()
        if ctypes.windll.psapi.GetProcessMemoryInfo(
            process, ctypes.byref(counters), counters.cb
        ):
            return int(counters.WorkingSetSize)
    except Exception:
        pass
    return 0


@dataclass
class MemorySnapshot:
    current_rss_bytes: int
    peak_rss_bytes: int
    python_current_bytes: int
    python_peak_bytes: int


class MemoryGuard:
    def __init__(self, limit_bytes: int):
        self.limit_bytes = int(limit_bytes)
        self.started = False
        self._peak_rss = 0
        self._lock = threading.Lock()

    def start(self) -> None:
        if not self.started:
            tracemalloc.start()
            self.started = True

    def snapshot(self) -> MemorySnapshot:
        current_rss = current_rss_bytes()
        with self._lock:
            self._peak_rss = max(self._peak_rss, current_rss)
        py_current = py_peak = 0
        if self.started:
            py_current, py_peak = tracemalloc.get_traced_memory()
        return MemorySnapshot(
            current_rss_bytes=current_rss,
            peak_rss_bytes=max(self._peak_rss, current_rss),
            python_current_bytes=int(py_current),
            python_peak_bytes=int(py_peak),
        )

    def check(self, context: str) -> None:
        snapshot = self.snapshot()
        rss = snapshot.current_rss_bytes or snapshot.python_current_bytes
        if rss and rss > self.limit_bytes:
            raise MemoryLimitError(
                "configured memory limit exceeded; committed windows are preserved",
                details={
                    "context": context,
                    "limit_bytes": self.limit_bytes,
                    "current_rss_bytes": rss,
                    "peak_rss_bytes": snapshot.peak_rss_bytes,
                },
            )

    def stop(self) -> None:
        if self.started:
            tracemalloc.stop()
            self.started = False


def deterministic_memory_estimate(
    *,
    telemetry_rows: int,
    supplement_rows: int,
    state_rows: int,
    slots: int,
    batch_buffer_bytes: int,
) -> int:
    # A deterministic accounting model used in the reproducible part of the
    # report. Runtime RSS is reported separately and is intentionally volatile.
    fixed = 24 * 1024 * 1024
    event_bytes = (telemetry_rows + supplement_rows) * 144
    state_bytes = state_rows * 192
    slot_bytes = slots * 64 * 1024
    return int(fixed + event_bytes + state_bytes + slot_bytes + batch_buffer_bytes)


def wait_for_memory_release(deadline_seconds: float = 0.05) -> None:
    # Small, bounded pause used after large bulk objects go out of scope.
    time.sleep(max(0.0, min(deadline_seconds, 0.05)))

