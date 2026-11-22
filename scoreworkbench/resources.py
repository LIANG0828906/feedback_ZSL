"""Resource boundaries: open-file limit, event-buffer cap, serial write lock.

Batch loading/conversion may run in parallel threads, while every mutation of
the persistent snapshot store goes through a process-wide serial lock so that
commit order stays stable.
"""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager

from .constants import RESOURCE_LIMITS
from .errors import SemanticError

_OPEN_FILE_SEMAPHORE = threading.BoundedSemaphore(RESOURCE_LIMITS["max_open_files"])
_PERSIST_WRITE_LOCK = threading.RLock()


@contextmanager
def open_file_slot():
    """Bound the number of files opened concurrently by parallel loaders."""
    acquired = _OPEN_FILE_SEMAPHORE.acquire(timeout=30)
    if not acquired:
        raise SemanticError(
            "Timed out waiting for an open-file slot",
            {"limit": RESOURCE_LIMITS["max_open_files"]},
        )
    try:
        yield
    finally:
        _OPEN_FILE_SEMAPHORE.release()


@contextmanager
def serial_persist_write():
    """Serialise every snapshot write inside the process."""
    with _PERSIST_WRITE_LOCK:
        yield


def check_event_buffer(count: int) -> None:
    if count > RESOURCE_LIMITS["max_event_buffer"]:
        raise SemanticError(
            "In-memory event buffer exceeds the configured limit",
            {"event_count": count, "limit": RESOURCE_LIMITS["max_event_buffer"]},
        )


def parallel_map(func, items, max_workers: int | None = None):
    """Threaded map used for batch load/convert; writes remain serial."""
    workers = max_workers or min(8, len(items) or 1)
    with ThreadPoolExecutor(max_workers=workers) as pool:
        return list(pool.map(func, items))
