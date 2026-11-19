from __future__ import annotations

import threading
from contextlib import contextmanager
from typing import Any, Callable, Iterator, Protocol, Sequence

from .config import EndpointConfig


class PoolExhaustedError(RuntimeError):
    pass


class DatabaseUnavailableError(RuntimeError):
    pass


class DbConnection(Protocol):
    autocommit: bool
    timeout: int

    def cursor(self) -> Any: ...

    def commit(self) -> None: ...

    def rollback(self) -> None: ...

    def close(self) -> None: ...


Connector = Callable[[EndpointConfig], DbConnection]


def pyodbc_connector(endpoint: EndpointConfig) -> DbConnection:
    try:
        import pyodbc
    except ImportError:
        raise DatabaseUnavailableError(
            "pyodbc is not installed; install ordersync-consistency[sqlserver]"
        ) from None
    try:
        connection = pyodbc.connect(
            endpoint.connection_string,
            autocommit=False,
            timeout=endpoint.login_timeout_seconds,
        )
        connection.timeout = endpoint.query_timeout_seconds
        return connection
    except Exception as exc:
        raise DatabaseUnavailableError("SQL Server connection failed") from exc


def _sqlstate(exc: BaseException) -> str:
    args = getattr(exc, "args", ())
    if args and isinstance(args[0], str):
        return args[0]
    return ""


def is_connection_error(exc: BaseException) -> bool:
    state = _sqlstate(exc)
    if state[:2] in {"08", "HY"}:
        return True
    name = type(exc).__name__.lower()
    return isinstance(exc, DatabaseUnavailableError) or "operational" in name


def is_write_conflict(exc: BaseException) -> bool:
    state = _sqlstate(exc)
    text = str(exc)
    return (
        state == "23000"
        or "2627" in text
        or "2601" in text
        or "547" in text
        or "duplicate key" in text.casefold()
        or "foreign key" in text.casefold()
    )


def row_to_dict(cursor: Any, row: Sequence[Any]) -> dict[str, Any]:
    return {
        str(column[0]): value
        for column, value in zip(cursor.description or (), row)
    }


class BoundedConnectionPool:
    """Lazy, bounded connection pool with a hard acquisition deadline."""

    def __init__(
        self,
        endpoint: EndpointConfig,
        connector: Connector = pyodbc_connector,
    ) -> None:
        self.endpoint = endpoint
        self._connector = connector
        self._permits = threading.BoundedSemaphore(endpoint.pool_size)
        self._idle: list[DbConnection] = []
        self._lock = threading.Lock()
        self._closed = False

    def acquire(self, timeout_seconds: float | None = None) -> DbConnection:
        if timeout_seconds is None:
            timeout_seconds = self.endpoint.pool_acquire_timeout_ms / 1000.0
        if timeout_seconds < 0:
            timeout_seconds = 0
        if not self._permits.acquire(timeout=timeout_seconds):
            raise PoolExhaustedError("connection pool is exhausted")

        try:
            with self._lock:
                if self._closed:
                    raise DatabaseUnavailableError("connection pool is closed")
                if self._idle:
                    return self._idle.pop()
            return self._connector(self.endpoint)
        except BaseException:
            self._permits.release()
            raise

    def release(self, connection: DbConnection, *, broken: bool = False) -> None:
        reusable = not broken and not self._closed
        if reusable:
            with self._lock:
                if not self._closed:
                    self._idle.append(connection)
                    self._permits.release()
                    return
        try:
            connection.close()
        except Exception:
            pass
        self._permits.release()

    def close(self) -> None:
        with self._lock:
            self._closed = True
            idle = self._idle
            self._idle = []
        for connection in idle:
            try:
                connection.close()
            except Exception:
                pass


class SqlClient:
    def __init__(
        self,
        endpoint: EndpointConfig,
        connector: Connector = pyodbc_connector,
        *,
        pool: BoundedConnectionPool | None = None,
    ) -> None:
        self.endpoint = endpoint
        self._pool = pool or BoundedConnectionPool(endpoint, connector)
        self._query_timeout = endpoint.query_timeout_seconds

    def query(
        self,
        sql: str,
        params: Sequence[Any] = (),
        *,
        timeout_seconds: float | None = None,
    ) -> list[dict[str, Any]]:
        connection = self._pool.acquire(timeout_seconds)
        cursor = None
        try:
            connection.timeout = self._query_timeout
            cursor = connection.cursor()
            cursor.execute(sql, tuple(params))
            if cursor.description is None:
                rows: list[dict[str, Any]] = []
            else:
                rows = [row_to_dict(cursor, row) for row in cursor.fetchall()]
            connection.commit()
        except BaseException as exc:
            try:
                connection.rollback()
            except Exception:
                pass
            self._pool.release(connection, broken=is_connection_error(exc))
            raise
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
        self._pool.release(connection)
        return rows

    def query_one(
        self,
        sql: str,
        params: Sequence[Any] = (),
        *,
        timeout_seconds: float | None = None,
    ) -> dict[str, Any] | None:
        rows = self.query(sql, params, timeout_seconds=timeout_seconds)
        return rows[0] if rows else None

    @contextmanager
    def transaction(self) -> Iterator[Any]:
        connection = self._pool.acquire()
        cursor = None
        try:
            connection.timeout = self._query_timeout
            cursor = connection.cursor()
            cursor.execute("SET XACT_ABORT ON")
            yield cursor
            connection.commit()
        except BaseException as exc:
            try:
                connection.rollback()
            except Exception:
                pass
            self._pool.release(connection, broken=is_connection_error(exc))
            raise
        else:
            self._pool.release(connection)
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass

    def close(self) -> None:
        self._pool.close()


class DedicatedConnection:
    """A single private connection used by monitoring, never by request handlers."""

    def __init__(
        self,
        endpoint: EndpointConfig,
        connector: Connector = pyodbc_connector,
    ) -> None:
        self.endpoint = endpoint
        self._connector = connector
        self._connection: DbConnection | None = None
        self._lock = threading.Lock()

    def _get_connection(self) -> DbConnection:
        if self._connection is None:
            self._connection = self._connector(self.endpoint)
            self._connection.timeout = self.endpoint.query_timeout_seconds
        return self._connection

    def query(
        self, sql: str, params: Sequence[Any] = ()
    ) -> list[dict[str, Any]]:
        with self._lock:
            connection = self._get_connection()
            cursor = None
            try:
                cursor = connection.cursor()
                cursor.execute(sql, tuple(params))
                if cursor.description is None:
                    rows: list[dict[str, Any]] = []
                else:
                    rows = [
                        row_to_dict(cursor, row) for row in cursor.fetchall()
                    ]
                connection.commit()
                return rows
            except BaseException:
                self.close_locked()
                raise
            finally:
                if cursor is not None:
                    try:
                        cursor.close()
                    except Exception:
                        pass

    def query_one(
        self, sql: str, params: Sequence[Any] = ()
    ) -> dict[str, Any] | None:
        rows = self.query(sql, params)
        return rows[0] if rows else None

    def close_locked(self) -> None:
        if self._connection is not None:
            try:
                self._connection.close()
            except Exception:
                pass
            self._connection = None

    def close(self) -> None:
        with self._lock:
            self.close_locked()


