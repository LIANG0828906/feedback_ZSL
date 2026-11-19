from __future__ import annotations

import threading
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import Callable

from .config import Config
from .database import Database


@dataclass
class _CacheEntry:
    expires_at: float
    rows: list[dict[str, object]]


class QueryCache:
    def __init__(self, config: Config, *, clock=time.monotonic) -> None:
        self.config = config
        self.clock = clock
        self._lock = threading.RLock()
        self._entries: OrderedDict[
            tuple[str, str, int, int, bool], _CacheEntry
        ] = OrderedDict()

    def get(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        slim: bool,
    ) -> list[dict[str, object]] | None:
        key = (tenant_id, status, page, page_size, slim)
        now = self.clock()
        with self._lock:
            entry = self._entries.get(key)
            if entry is None:
                return None
            if entry.expires_at <= now:
                self._entries.pop(key, None)
                return None
            self._entries.move_to_end(key)
            return [dict(row) for row in entry.rows]

    def put(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
        slim: bool,
        rows: list[dict[str, object]],
    ) -> None:
        key = (tenant_id, status, page, page_size, slim)
        expires_at = self.clock() + self.config.fallback.cache_ttl_seconds
        with self._lock:
            self._entries[key] = _CacheEntry(
                expires_at=expires_at,
                rows=[dict(row) for row in rows],
            )
            self._entries.move_to_end(key)
            while len(self._entries) > self.config.fallback.cache_max_entries:
                self._entries.popitem(last=False)

    def invalidate_tenant(self, tenant_id: str) -> None:
        with self._lock:
            for key in [key for key in self._entries if key[0] == tenant_id]:
                self._entries.pop(key, None)

    def size(self) -> int:
        with self._lock:
            return len(self._entries)


class SlimSnapshot:
    """In-memory reduced-field source used when the database is unavailable."""

    def __init__(
        self,
        config: Config,
        database: Database,
        *,
        loader: Callable[[str, int], list[dict[str, object]]] | None = None,
    ) -> None:
        self.config = config
        self.loader = loader or database.fetch_snapshot_rows
        self._lock = threading.RLock()
        self._rows: dict[str, tuple[dict[str, object], ...]] = {}
        self.loaded = False

    def load(self) -> None:
        rows: dict[str, tuple[dict[str, object], ...]] = {}
        max_rows = self.config.fallback.snapshot_max_orders_per_tenant
        for tenant in self.config.tenants:
            rows[tenant.tenant_id] = tuple(self.loader(tenant.tenant_id, max_rows))
        with self._lock:
            self._rows = rows
            self.loaded = True

    def query(
        self,
        *,
        tenant_id: str,
        status: str,
        page: int,
        page_size: int,
    ) -> list[dict[str, object]]:
        with self._lock:
            rows = self._rows.get(tenant_id, ())
        selected = [
            row
            for row in rows
            if row["status"] == status
        ]
        start = (page - 1) * page_size
        page_rows = selected[start : start + page_size]
        return [
            {
                "order_id": row["order_id"],
                "status": row["status"],
                "created_at": row["created_at"],
            }
            for row in page_rows
        ]

    def order_count(self) -> int:
        with self._lock:
            return sum(len(rows) for rows in self._rows.values())
