from __future__ import annotations

import contextlib
import queue
import threading
import time
from collections.abc import Iterator
from dataclasses import dataclass

import psycopg
from psycopg.rows import dict_row

from .config import SiteConfig
from .errors import ConnectionLimitError


@dataclass
class PoolStats:
    limit: int
    open: int
    idle: int
    waiting: int


class PgPool:
    def __init__(self, site: SiteConfig, size: int, statement_timeout_ms: int = 5000):
        if size < 1:
            raise ValueError("pool size must be positive")
        self.site = site
        self.size = size
        self.statement_timeout_ms = statement_timeout_ms
        self._idle: queue.LifoQueue[psycopg.Connection] = queue.LifoQueue()
        self._lock = threading.Lock()
        self._open = 0
        self._closed = False

    def _new_connection(self) -> psycopg.Connection:
        conn = psycopg.connect(
            self.site.dsn,
            autocommit=False,
            row_factory=dict_row,
            application_name=f"dualwrite-{self.site.name}",
        )
        conn.execute(f"SET statement_timeout = {self.statement_timeout_ms}")
        conn.execute("SET lock_timeout = 2000")
        conn.execute("SET idle_in_transaction_session_timeout = 15000")
        conn.commit()
        return conn

    def acquire(self, timeout_ms: int) -> psycopg.Connection:
        deadline = time.monotonic() + max(0, timeout_ms) / 1000.0
        while True:
            try:
                conn = self._idle.get_nowait()
            except queue.Empty:
                with self._lock:
                    if self._closed:
                        raise ConnectionLimitError("connection pool is closed")
                    if self._open < self.size:
                        self._open += 1
                        should_open = True
                    else:
                        should_open = False
                if should_open:
                    try:
                        return self._new_connection()
                    except Exception:
                        with self._lock:
                            self._open -= 1
                        raise
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ConnectionLimitError(
                        f"no database connection available for site {self.site.name} within {timeout_ms}ms"
                    )
                try:
                    conn = self._idle.get(timeout=min(remaining, 0.1))
                except queue.Empty:
                    continue

            if conn.closed:
                with self._lock:
                    self._open -= 1
                continue
            return conn

    def release(self, conn: psycopg.Connection) -> None:
        if self._closed:
            with contextlib.suppress(Exception):
                conn.close()
            with self._lock:
                self._open -= 1
            return
        try:
            if conn.info.transaction_status != psycopg.pq.TransactionStatus.IDLE:
                conn.rollback()
        except Exception:
            with contextlib.suppress(Exception):
                conn.close()
            with self._lock:
                self._open -= 1
            return
        self._idle.put(conn)

    @contextlib.contextmanager
    def connection(self, timeout_ms: int) -> Iterator[psycopg.Connection]:
        conn = self.acquire(timeout_ms)
        broken = False
        try:
            yield conn
        except BaseException:
            broken = True
            raise
        finally:
            if broken:
                try:
                    conn.rollback()
                except Exception:
                    conn.close()
                    with self._lock:
                        self._open -= 1
                else:
                    self.release(conn)
            else:
                self.release(conn)

    def stats(self) -> PoolStats:
        with self._lock:
            open_count = self._open
        return PoolStats(
            limit=self.size,
            open=open_count,
            idle=self._idle.qsize(),
            waiting=0,
        )

    def close(self) -> None:
        self._closed = True
        while True:
            try:
                conn = self._idle.get_nowait()
            except queue.Empty:
                break
            with contextlib.suppress(Exception):
                conn.close()


class DatabaseSet:
    def __init__(
        self,
        sites: dict[str, SiteConfig],
        max_connections: int,
        statement_timeout_ms: int,
    ):
        per_site = max(1, max_connections // 2)
        self.pools = {
            name: PgPool(site, per_site, statement_timeout_ms)
            for name, site in sites.items()
        }

    def pool(self, site: str) -> PgPool:
        return self.pools[site]

    def close(self) -> None:
        for pool in self.pools.values():
            pool.close()

    def stats(self) -> dict[str, PoolStats]:
        return {name: pool.stats() for name, pool in self.pools.items()}
