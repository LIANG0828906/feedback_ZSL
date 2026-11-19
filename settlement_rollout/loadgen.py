from __future__ import annotations

import http.client
import queue
import threading
import time
from dataclasses import dataclass
from typing import Any


@dataclass
class LoadTarget:
    instance_id: str
    group: str
    host: str
    port: int


class LoadGenerator:
    def __init__(
        self,
        targets: list[LoadTarget],
        rate_per_second: float = 12.0,
        workers: int = 12,
        timeout_seconds: float = 10.0,
    ) -> None:
        if not targets:
            raise ValueError("at least one load target is required")
        self.targets = targets
        self.rate_per_second = float(rate_per_second)
        self.worker_count = int(workers)
        self.timeout_seconds = float(timeout_seconds)
        if self.rate_per_second <= 0:
            raise ValueError("rate_per_second must be positive")
        if self.worker_count <= 0:
            raise ValueError("workers must be positive")
        queue_size = max(64, int(self.rate_per_second * 5))
        self._queue: queue.Queue[tuple[int, LoadTarget] | None] = queue.Queue(
            maxsize=queue_size
        )
        self._stop = threading.Event()
        self._lock = threading.Lock()
        self._next_target = 0
        self._scheduler: threading.Thread | None = None
        self._workers: list[threading.Thread] = []
        self.attempts = 0
        self.completed = 0
        self.success = 0
        self.server_errors = 0
        self.connection_errors = 0
        self.results: list[dict[str, Any]] = []

    def start(self) -> None:
        self._scheduler = threading.Thread(
            target=self._schedule,
            name="settlement-load-scheduler",
            daemon=True,
        )
        self._scheduler.start()
        for index in range(self.worker_count):
            worker = threading.Thread(
                target=self._worker,
                name=f"settlement-load-{index}",
                daemon=True,
            )
            worker.start()
            self._workers.append(worker)

    def _schedule(self) -> None:
        interval = 1.0 / self.rate_per_second
        deadline = time.monotonic()
        request_id = 0
        while not self._stop.is_set():
            with self._lock:
                target = self.targets[self._next_target % len(self.targets)]
                self._next_target += 1
                request_id += 1
                self.attempts += 1
            item = (request_id, target)
            while not self._stop.is_set():
                try:
                    self._queue.put(item, timeout=0.1)
                    break
                except queue.Full:
                    continue
            deadline += interval
            delay = deadline - time.monotonic()
            if delay > 0:
                self._stop.wait(delay)
            else:
                deadline = time.monotonic()

    def _worker(self) -> None:
        while True:
            try:
                item = self._queue.get(timeout=0.1)
            except queue.Empty:
                if self._stop.is_set():
                    return
                continue
            try:
                if item is None:
                    return
                request_id, target = item
                started = time.monotonic()
                status: int | None = None
                error: str | None = None
                connection = http.client.HTTPConnection(
                    target.host,
                    target.port,
                    timeout=self.timeout_seconds,
                )
                try:
                    connection.request(
                        "GET",
                        f"/settle?request_id={request_id}&instance={target.instance_id}",
                        headers={"Connection": "close"},
                    )
                    response = connection.getresponse()
                    response.read()
                    status = response.status
                except (OSError, http.client.HTTPException) as exc:
                    error = type(exc).__name__
                finally:
                    connection.close()
                elapsed = time.monotonic() - started
                with self._lock:
                    self.completed += 1
                    if error is not None:
                        self.connection_errors += 1
                    elif status is not None and status >= 500:
                        self.server_errors += 1
                    elif status is not None:
                        self.success += 1
                    self.results.append(
                        {
                            "request_id": request_id,
                            "instance_id": target.instance_id,
                            "status": status,
                            "error": error,
                            "started_monotonic": started,
                            "elapsed_seconds": elapsed,
                        }
                    )
            finally:
                self._queue.task_done()

    def stop(self, timeout_seconds: float = 5.0) -> None:
        self._stop.set()
        deadline = time.monotonic() + timeout_seconds
        while self._queue.unfinished_tasks > 0 and time.monotonic() < deadline:
            time.sleep(0.02)
        if self._scheduler is not None:
            self._scheduler.join(timeout=1.0)
        for worker in self._workers:
            while worker.is_alive() and time.monotonic() < deadline:
                worker.join(timeout=0.05)

    def summary(self) -> dict[str, Any]:
        with self._lock:
            return {
                "attempts": self.attempts,
                "completed": self.completed,
                "success": self.success,
                "server_errors": self.server_errors,
                "connection_errors": self.connection_errors,
            }

    def results_since(self, monotonic_ts: float) -> list[dict[str, Any]]:
        with self._lock:
            return [
                dict(item)
                for item in self.results
                if item["started_monotonic"] >= monotonic_ts
            ]
