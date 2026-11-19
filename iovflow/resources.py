from __future__ import annotations

import threading
import time
from typing import Callable

from .errors import EngineError
from .util import process_rss_bytes


class MemoryMonitor:
    def __init__(
        self,
        pids: Callable[[], list[int]],
        limit_mb: int,
        interval: float = 0.2,
    ) -> None:
        self.pids = pids
        self.limit_mb = int(limit_mb)
        self.interval = interval
        self.peak_mb = 0.0
        self.exceeded = False
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="iovflow-memory-monitor", daemon=True)

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=1.0)

    def _run(self) -> None:
        while not self._stop.is_set():
            try:
                total = 0
                for pid in self.pids():
                    total += process_rss_bytes(pid)
                mb = total / (1024 * 1024)
                if mb > self.peak_mb:
                    self.peak_mb = mb
                if mb > self.limit_mb:
                    self.exceeded = True
            except Exception:
                pass
            self._stop.wait(self.interval)

    def check(self) -> None:
        if self.exceeded or self.peak_mb > self.limit_mb:
            raise EngineError(
                "IOV_RESOURCE_MEMORY",
                f"process tree RSS exceeded {self.limit_mb} MiB (peak {self.peak_mb:.1f} MiB)",
                context={"peak_mb": round(self.peak_mb, 2), "limit_mb": self.limit_mb},
            )

