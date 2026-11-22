"""Atomic in-memory publication with complete old/new previews."""

from __future__ import annotations

import threading
from copy import deepcopy
from typing import Any

from .snapshot import SnapshotStore


class VersionStore:
    def __init__(
        self,
        report: dict[str, Any] | None = None,
        *,
        snapshot_store: SnapshotStore | None = None,
    ) -> None:
        self._lock = threading.RLock()
        self._current = deepcopy(report) if report is not None else None
        self.snapshot_store = snapshot_store
        self.preview_count = 0

    def publish(self, report: dict[str, Any]) -> int:
        if self.snapshot_store is not None:
            self.snapshot_store.save_published(report)
        with self._lock:
            self._current = deepcopy(report)
            return int(report["published_version"])

    def preview(self) -> dict[str, Any] | None:
        with self._lock:
            self.preview_count += 1
            return deepcopy(self._current)

    @property
    def version(self) -> int | None:
        with self._lock:
            if self._current is None:
                return None
            return int(self._current.get("published_version"))

