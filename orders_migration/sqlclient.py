from __future__ import annotations

import re
import threading
import time
from collections import defaultdict
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Iterator, Sequence

from .config import Config, DatabaseConfig
from .errors import AppError, conn_pool_error


_DB_PATTERN = re.compile(
    r"(?i)(?P<prefix>\b(?:DATABASE|DBQ|Initial\s+Catalog)\s*=\s*)(?P<db>[^;]+)"
)
_BRACED = re.compile(r"^\{.*\}$")


class DatabaseAccessError(Exception):
    """A transport or SQL Server failure while accessing one database."""

    def __init__(self, database: str, operation: str, original: BaseException):
        self.database = database
        self.operation = operation
        self.original = original
        super().__init__(f"{database} {operation} failed: {original}")

    @property
    def sqlstate(self) -> str | None:
        return getattr(self.original, "sqlstate", None)

    @property
    def number(self) -> int | None:
        args = getattr(self.original, "args", ())
        if len(args) > 1 and isinstance(args[1], int):
            return args[1]
        return getattr(self.original, "number", None)


def parse_connection_pairs(connection_string: str) -> dict[str, str]:
    pairs: dict[str, str] = {}
    current: list[str] = []
    depth = 0
    for char in connection_string:
        if char == "{":
            depth += 1
        elif char == "}":
            depth = max(0, depth - 1)
        if char == ";" and depth == 0:
            part = "".join(current).strip()
            current = []
            if part and "=" in part:
                key, value = part.split("=", 1)
                value = value.strip()
                if _BRACED.match(value):
                    value = value[1:-1]
                pairs[key.strip().lower()] = value
        else:
            current.append(char)
    part = "".join(current).strip()
    if part and "=" in part:
        key, value = part.split("=", 1)
        value = value.strip()
        if _BRACED.match(value):
            value = value[1:-1]
        pairs[key.strip().lower()] = value
    return pairs


def connection_database(connection_string: str) -> str | None:
    match = _DB_PATTERN.search(connection_string)
    return match.group("db").strip() if match else None


def connection_with_database(connection_string: str, database: str) -> str:
    match = _DB_PATTERN.search(connection_string)
    if match:
        return (
            connection_string[: match.start("db")]
            + database
            + connection_string[match.end("db") :]
        )
    return connection_string.rstrip(";") + f";DATABASE={database};"


def resolve_connection_string(
    connection_string: str, database: str | None = None
) -> str:
    if database:
        return connection_with_database(connection_string, database)
    return connection_string


def _available_pyodbc() -> bool:
    try:
        import pyodbc

        return bool(pyodbc.drivers())
    except Exception:
        return False


def choose_driver(configured: str, connection_string: str) -> str:
    if configured != "auto":
        return configured
    lowered = connection_string.lower()
    if "dsn=" in lowered or "driver=" in lowered:
        return "pyodbc"
    if _available_pyodbc() and "server=" in lowered and "uid=" in lowered:
        return "pyodbc"
    return "pymssql"


def _connect_pyodbc(
    connection_string: str, connect_timeout: float, command_timeout: float
) -> Any:
    import pyodbc

    connection = pyodbc.connect(
        connection_string,
        autocommit=False,
        timeout=int(max(1, round(connect_timeout))),
    )
    connection.timeout = max(1, int(round(command_timeout)))
    return connection


def _connect_pymssql(
    connection_string: str, connect_timeout: float, command_timeout: float
) -> Any:
    import pymssql

    pairs = parse_connection_pairs(connection_string)
    server = pairs.get("server") or pairs.get("host") or "127.0.0.1"
    if "\\" in server:
        server = server.split("\\", 1)[0]
    if "," in server:
        server, embedded_port = server.rsplit(",", 1)
        pairs.setdefault("port", embedded_port)
    port = int(pairs.get("port", 1433))
    database = pairs.get("database") or pairs.get("dbq") or "master"
    user = pairs.get("user") or pairs.get("uid") or "sa"
    password = pairs.get("password") or pairs.get("pwd") or ""
    return pymssql.connect(
        server=server,
        port=port,
        user=user,
        password=password,
        database=database,
        login_timeout=max(1, int(round(connect_timeout))),
        timeout=max(1, int(round(command_timeout))),
        charset="UTF-8",
        appname="orders-migration",
    )


def open_connection(
    connection_string: str,
    driver: str,
    connect_timeout: float,
    command_timeout: float,
) -> tuple[Any, str]:
    actual_driver = choose_driver(driver, connection_string)
    if actual_driver == "pyodbc":
        return (
            _connect_pyodbc(connection_string, connect_timeout, command_timeout),
            actual_driver,
        )
    return (
        _connect_pymssql(connection_string, connect_timeout, command_timeout),
        actual_driver,
    )


def _translate_placeholders(sql: str, dialect: str) -> str:
    if dialect == "pymssql":
        return sql.replace("?", "%s")
    return sql


@dataclass
class ManagedConnection:
    database: str
    connection: Any
    dialect: str
    created_at: float
    last_used: float


class ConnectionPool:
    """A bounded, reusable pool shared by all configured databases.

    The configured maximum is a hard bound on both leased and idle physical
    connections. Acquisitions that cannot be satisfied in time fail with
    E_CONN_POOL instead of waiting indefinitely.
    """

    def __init__(self, config: DatabaseConfig, max_connections: int,
                 acquire_timeout_ms: int, idle_ttl_seconds: float):
        self._config = config
        self._max = max_connections
        self._acquire_timeout = acquire_timeout_ms / 1000.0
        self._idle_ttl = idle_ttl_seconds
        self._condition = threading.Condition()
        self._idle: dict[str, list[ManagedConnection]] = defaultdict(list)
        self._leased = 0
        self._open = 0
        self._peak = 0
        self._acquire_count = 0
        self._timeouts = 0
        self._closed = False

    @property
    def metrics(self) -> dict[str, int]:
        with self._condition:
            return {
                "openConnections": self._open,
                "leasedConnections": self._leased,
                "idleConnections": sum(len(v) for v in self._idle.values()),
                "peakConnections": self._peak,
                "maxConnections": self._max,
                "acquisitions": self._acquire_count,
                "acquireTimeouts": self._timeouts,
            }

    def _connect(self, key: str, connection_string: str) -> ManagedConnection:
        now = time.monotonic()
        connection, dialect = open_connection(
            connection_string,
            self._config.driver,
            self._config.connect_timeout_seconds,
            self._config.command_timeout_seconds,
        )
        return ManagedConnection(key, connection, dialect, now, now)

    def acquire(self, key: str, connection_string: str) -> ManagedConnection:
        deadline = time.monotonic() + self._acquire_timeout
        reserved = False
        with self._condition:
            while True:
                queue = self._idle.get(key)
                while queue:
                    candidate = queue.pop()
                    age = time.monotonic() - candidate.last_used
                    if self._idle_ttl and age > self._idle_ttl:
                        self._close_locked(candidate)
                        continue
                    self._leased += 1
                    self._acquire_count += 1
                    candidate.last_used = time.monotonic()
                    return candidate

                if self._open < self._max:
                    self._open += 1
                    self._leased += 1
                    self._peak = max(self._peak, self._open)
                    self._acquire_count += 1
                    reserved = True
                    break

                evicted = self._evict_locked(exclude=key)
                if evicted:
                    self._open += 1
                    self._leased += 1
                    self._peak = max(self._peak, self._open)
                    self._acquire_count += 1
                    reserved = True
                    break

                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    self._timeouts += 1
                    raise conn_pool_error()
                self._condition.wait(min(remaining, 0.25))

        assert reserved
        try:
            return self._connect(key, connection_string)
        except BaseException:
            with self._condition:
                self._leased -= 1
                self._open -= 1
                self._condition.notify_all()
            raise

    def release(self, managed: ManagedConnection, healthy: bool = True) -> None:
        with self._condition:
            self._leased = max(0, self._leased - 1)
            if healthy and not self._closed:
                managed.last_used = time.monotonic()
                self._idle[managed.database].append(managed)
            else:
                self._close_locked(managed)
            self._condition.notify()

    def discard(self, managed: ManagedConnection) -> None:
        self.release(managed, healthy=False)

    def _close_locked(self, managed: ManagedConnection) -> None:
        try:
            managed.connection.close()
        except Exception:
            pass
        self._open = max(0, self._open - 1)

    def _evict_locked(self, exclude: str) -> bool:
        for key, queue in self._idle.items():
            if key != exclude and queue:
                self._close_locked(queue.pop())
                return True
        return False

    def close(self) -> None:
        with self._condition:
            self._closed = True
            for queue in self._idle.values():
                while queue:
                    self._close_locked(queue.pop())
            self._condition.notify_all()


class Session:
    def __init__(self, managed: ManagedConnection):
        self._managed = managed
        self._connection = managed.connection
        self._dialect = managed.dialect

    @property
    def database(self) -> str:
        return self._managed.database

    @property
    def connection(self) -> Any:
        return self._connection

    def cursor(self) -> Any:
        return self._connection.cursor()

    def execute(self, sql: str, params: Sequence[Any] = ()) -> int:
        cursor = self.cursor()
        try:
            cursor.execute(
                _translate_placeholders(sql, self._dialect), tuple(params)
            )
            return int(cursor.rowcount)
        finally:
            cursor.close()

    def executemany(self, sql: str, params: Sequence[Sequence[Any]]) -> None:
        cursor = self.cursor()
        try:
            cursor.executemany(
                _translate_placeholders(sql, self._dialect),
                [tuple(row) for row in params],
            )
        finally:
            cursor.close()

    def query(
        self, sql: str, params: Sequence[Any] = ()
    ) -> list[dict[str, Any]]:
        cursor = self.cursor()
        try:
            cursor.execute(
                _translate_placeholders(sql, self._dialect), tuple(params)
            )
            if cursor.description is None:
                return []
            columns = [column[0] for column in cursor.description]
            return [dict(zip(columns, row)) for row in cursor.fetchall()]
        finally:
            cursor.close()

    def query_one(
        self, sql: str, params: Sequence[Any] = ()
    ) -> dict[str, Any] | None:
        rows = self.query(sql, params)
        return rows[0] if rows else None

    def commit(self) -> None:
        self._connection.commit()

    def rollback(self) -> None:
        try:
            self._connection.rollback()
        except Exception:
            pass


class DatabaseHub:
    def __init__(self, config: Config):
        self.config = config
        db = config.database
        self._connection_strings: dict[str, str] = {
            "legacy": resolve_connection_string(db.legacy_connection_string)
        }
        for shard in range(db.shard_count):
            self._connection_strings[f"shard{shard}"] = (
                resolve_connection_string(db.shard_connection_string(shard))
            )
        self._connection_strings["master"] = resolve_connection_string(
            db.legacy_connection_string, "master"
        )
        self.pool = ConnectionPool(
            db,
            config.pool.max_connections,
            config.pool.acquire_timeout_ms,
            config.pool.idle_ttl_seconds,
        )

    def connection_string(self, key: str) -> str:
        try:
            return self._connection_strings[key]
        except KeyError as exc:
            raise KeyError(f"unknown database key {key!r}") from exc

    def keys(self) -> list[str]:
        return [key for key in self._connection_strings if key != "master"]

    def database_name(self, key: str) -> str:
        return connection_database(self.connection_string(key)) or key

    @contextmanager
    def transaction(self, key: str) -> Iterator[Session]:
        try:
            managed = self.pool.acquire(key, self.connection_string(key))
        except AppError:
            raise
        except BaseException as exc:
            raise DatabaseAccessError(key, "connect", exc) from exc
        session = Session(managed)
        healthy = True
        try:
            session.execute("SET XACT_ABORT ON; SET NOCOUNT ON;")
            yield session
            session.commit()
        except BaseException as exc:
            session.rollback()
            if isinstance(exc, AppError):
                raise
            healthy = not isinstance(exc, (OSError, ConnectionError))
            raise DatabaseAccessError(managed.database, "transaction", exc) from exc
        finally:
            self.pool.release(managed, healthy=healthy)

    @contextmanager
    def read(self, key: str) -> Iterator[Session]:
        try:
            managed = self.pool.acquire(key, self.connection_string(key))
        except AppError:
            raise
        except BaseException as exc:
            raise DatabaseAccessError(key, "connect", exc) from exc
        session = Session(managed)
        healthy = True
        try:
            session.execute("SET XACT_ABORT ON; SET NOCOUNT ON;")
            yield session
            session.rollback()
        except BaseException as exc:
            session.rollback()
            if isinstance(exc, AppError):
                raise
            healthy = False
            raise DatabaseAccessError(managed.database, "read", exc) from exc
        finally:
            self.pool.release(managed, healthy=healthy)

    def close(self) -> None:
        self.pool.close()


def shard_key(shard: int) -> str:
    return f"shard{shard}"


def tenant_shard(tenant_id: int, shard_count: int) -> int:
    return tenant_id % shard_count
