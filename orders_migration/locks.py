from __future__ import annotations

import threading
import time
from contextlib import contextmanager
from typing import Iterator

from .errors import AppError


class OrderLocks:
    """Per-order mutual exclusion without a process-wide write lock."""

    def __init__(self) -> None:
        self._guard = threading.Lock()
        self._locks: dict[int, list[object]] = {}

    @contextmanager
    def acquire(self, order_id: int, timeout: float = 30.0) -> Iterator[None]:
        with self._guard:
            entry = self._locks.get(order_id)
            if entry is None:
                entry = [threading.RLock(), 0]
                self._locks[order_id] = entry
            entry[1] = int(entry[1]) + 1
            lock = entry[0]
        acquired = lock.acquire(timeout=timeout)
        try:
            if not acquired:
                raise AppError(
                    "E_BUSY",
                    f"timed out waiting for order {order_id}",
                    {"orderId": order_id},
                    503,
                )
            yield
        finally:
            if acquired:
                lock.release()
            with self._guard:
                entry[1] = int(entry[1]) - 1
                if entry[1] == 0:
                    self._locks.pop(order_id, None)


class WriteGate:
    """Coordinates write cutover/drain with in-flight dual writes and migrations."""

    def __init__(self) -> None:
        self._condition = threading.Condition()
        self._stopping = False
        self._in_flight_writes = 0
        self._in_flight_migrations = 0

    @property
    def stopping(self) -> bool:
        with self._condition:
            return self._stopping

    def enter_write(self) -> None:
        with self._condition:
            if self._stopping:
                raise AppError(
                    "E_WRITE_STOPPED",
                    "new writes are stopped for a controlled rollback",
                    {"state": "draining"},
                    503,
                )
            self._in_flight_writes += 1

    def leave_write(self) -> None:
        with self._condition:
            self._in_flight_writes = max(0, self._in_flight_writes - 1)
            self._condition.notify_all()

    def enter_migration(self) -> None:
        with self._condition:
            if self._stopping:
                raise AppError(
                    "E_WRITE_STOPPED",
                    "migration is paused for a controlled rollback",
                    {"state": "draining"},
                    503,
                )
            self._in_flight_migrations += 1

    def leave_migration(self) -> None:
        with self._condition:
            self._in_flight_migrations = max(0, self._in_flight_migrations - 1)
            self._condition.notify_all()

    def drain(self, timeout_seconds: float) -> dict[str, int]:
        deadline = time.monotonic() + timeout_seconds
        with self._condition:
            self._stopping = True
            while self._in_flight_writes or self._in_flight_migrations:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AppError(
                        "E_ROLLBACK",
                        "timed out draining in-flight writes and migrations",
                        {
                            "inFlightWrites": self._in_flight_writes,
                            "inFlightMigrations": self._in_flight_migrations,
                        },
                        409,
                    )
                self._condition.wait(min(remaining, 0.25))
            return {
                "inFlightWrites": self._in_flight_writes,
                "inFlightMigrations": self._in_flight_migrations,
            }

    def resume_writes(self) -> None:
        with self._condition:
            self._stopping = False
            self._condition.notify_all()

    @property
    def counts(self) -> dict[str, int]:
        with self._condition:
            return {
                "inFlightWrites": self._in_flight_writes,
                "inFlightMigrations": self._in_flight_migrations,
                "stopping": int(self._stopping),
            }
