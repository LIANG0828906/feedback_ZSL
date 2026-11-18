from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .events import EventStore
from .http import (
    HttpParseError,
    content_length,
    encode_forward_request,
    force_reset,
    read_exact,
    read_response_head,
)
from .util import SequentialIds, utc_now


LOGGER = logging.getLogger("connectlab.client")


@dataclass
class ClientConnection:
    connection_id: str
    reader: asyncio.StreamReader
    writer: asyncio.StreamWriter
    last_request_id: str


@dataclass
class RequestPlan:
    seq: int
    request_id: str
    path: str
    method: str
    body_size: int
    body_file: Path | None
    worker_index: int
    started_at: str
    finished_at: str | None = None
    outcome: str = "incomplete"
    status_code: int | None = None
    error_code: str | None = None
    client_connection_id: str | None = None
    proxy_downstream_connection_id: str | None = None
    proxy_upstream_connection_id: str | None = None
    upstream_connection_id: str | None = None
    injection_applied: bool = False
    details: dict[str, Any] | None = None


class ClientSession:
    def __init__(
        self,
        *,
        batch_id: str,
        proxy_host: str,
        proxy_port: int,
        upstream_host: str,
        upstream_port: int,
        events: EventStore,
        ids: SequentialIds,
        per_hop_timeout_ms: int,
        injection: dict[str, Any],
    ) -> None:
        self.batch_id = batch_id
        self.proxy_host = proxy_host
        self.proxy_port = int(proxy_port)
        self.upstream_host = upstream_host
        self.upstream_port = int(upstream_port)
        self.events = events
        self.ids = ids
        self.per_hop_timeout_ms = per_hop_timeout_ms
        self.injection = injection
        self.keepalive_connection: ClientConnection | None = None
        self._body_cache: dict[str, bytes] = {}
        self._active_plan: RequestPlan | None = None
        self._active_connection: ClientConnection | None = None

    async def execute(self, plan: RequestPlan, injected: bool) -> RequestPlan:
        self._active_plan = plan
        plan.started_at = utc_now()
        plan.injection_applied = injected
        plan.details = {}
        connection: ClientConnection | None = None
        try:
            if plan.path == "keepalive" and self.keepalive_connection is not None:
                if (
                    not self.keepalive_connection.writer.is_closing()
                    and not self.keepalive_connection.reader.at_eof()
                ):
                    connection = self.keepalive_connection
                else:
                    await self._close_connection(
                        self.keepalive_connection,
                        plan.request_id,
                        initiator="client",
                        reason="keepalive_connection_not_usable",
                    )
                    self.keepalive_connection = None
            if connection is None:
                connection = await self._connect(plan)
            self._active_connection = connection
            plan.client_connection_id = connection.connection_id
            connection.last_request_id = plan.request_id

            await self._send_request(connection, plan, injected)
            status, response_headers = await self._read_response(connection, plan)
            plan.status_code = status
            plan.proxy_downstream_connection_id = response_headers.get(
                "x-proxy-downstream-connection-id"
            )
            plan.proxy_upstream_connection_id = response_headers.get(
                "x-proxy-upstream-connection-id"
            )
            plan.upstream_connection_id = response_headers.get(
                "x-upstream-connection-id"
            )
            error_header = response_headers.get("x-connectlab-error")
            keepalive = (
                plan.path == "keepalive"
                and "close" not in response_headers.get("connection", "").lower()
            )
            if keepalive and not connection.writer.is_closing():
                self.keepalive_connection = connection
            else:
                await self._close_connection(
                    connection,
                    plan.request_id,
                    initiator="client",
                    reason="client_connection_close",
                )
                if connection is self.keepalive_connection:
                    self.keepalive_connection = None
            if status == 200:
                plan.outcome = "success"
            elif error_header == "proxy_read_timeout" or status == 504:
                plan.outcome = "timeout"
                plan.error_code = error_header or "proxy_read_timeout"
            else:
                plan.outcome = "http_error"
                plan.error_code = error_header or f"HTTP_{status}"
        except asyncio.CancelledError:
            plan.outcome = "incomplete"
            plan.error_code = "BATCH_DEADLINE_EXCEEDED"
            if connection is not None:
                try:
                    await self.events.add(
                        batch_id=self.batch_id,
                        request_id=plan.request_id,
                        hop="client",
                        connection_id=connection.connection_id,
                        correlation_id=f"corr-{connection.connection_id}",
                        kind="timeout",
                        initiator="timer",
                        details={
                            "reason": "batch_deadline_exceeded",
                            "phase": "client_request",
                        },
                    )
                except ValueError:
                    pass
                try:
                    force_reset(connection.writer)
                except Exception:
                    pass
            if connection is self.keepalive_connection:
                self.keepalive_connection = None
            raise
        except asyncio.TimeoutError as exc:
            plan.outcome = "timeout"
            plan.error_code = "client_hop_timeout"
            if connection is not None:
                try:
                    await self.events.add(
                        batch_id=self.batch_id,
                        request_id=plan.request_id,
                        hop="client",
                        connection_id=connection.connection_id,
                        correlation_id=f"corr-{connection.connection_id}",
                        kind="timeout",
                        initiator="timer",
                        details={
                            "error": type(exc).__name__,
                            "phase": "client_hop",
                            "timeout_ms": self.per_hop_timeout_ms,
                        },
                    )
                except ValueError:
                    pass
                try:
                    force_reset(connection.writer)
                except Exception:
                    pass
            if connection is self.keepalive_connection:
                self.keepalive_connection = None
        except (ConnectionResetError, BrokenPipeError, asyncio.IncompleteReadError, HttpParseError) as exc:
            plan.outcome = "reset"
            plan.error_code = type(exc).__name__
            if connection is not None:
                try:
                    await self.events.add(
                        batch_id=self.batch_id,
                        request_id=plan.request_id,
                        hop="client",
                        connection_id=connection.connection_id,
                        correlation_id=f"corr-{connection.connection_id}",
                        kind="reset",
                        initiator="peer",
                        details={
                            "error": type(exc).__name__,
                            "message": str(exc),
                        },
                    )
                except ValueError:
                    pass
                try:
                    force_reset(connection.writer)
                except Exception:
                    pass
            if connection is self.keepalive_connection:
                self.keepalive_connection = None
        except OSError as exc:
            plan.outcome = "reset"
            plan.error_code = type(exc).__name__
            if connection is not None:
                try:
                    await self.events.add(
                        batch_id=self.batch_id,
                        request_id=plan.request_id,
                        hop="client",
                        connection_id=connection.connection_id,
                        correlation_id=f"corr-{connection.connection_id}",
                        kind="reset",
                        initiator="client_os",
                        details={"error": type(exc).__name__, "message": str(exc)},
                    )
                except ValueError:
                    pass
                try:
                    force_reset(connection.writer)
                except Exception:
                    pass
            if connection is self.keepalive_connection:
                self.keepalive_connection = None
        except Exception as exc:
            LOGGER.exception("client request failed")
            plan.outcome = "reset"
            plan.error_code = type(exc).__name__
            if connection is not None:
                try:
                    await self.events.add(
                        batch_id=self.batch_id,
                        request_id=plan.request_id,
                        hop="client",
                        connection_id=connection.connection_id,
                        correlation_id=f"corr-{connection.connection_id}",
                        kind="reset",
                        initiator="client",
                        details={"error": type(exc).__name__, "message": str(exc)},
                    )
                except ValueError:
                    pass
                try:
                    force_reset(connection.writer)
                except Exception:
                    pass
        finally:
            plan.finished_at = utc_now()
            self._active_plan = None
            self._active_connection = None
        return plan

    async def _connect(self, plan: RequestPlan) -> ClientConnection:
        connection_id = self.ids.next()
        correlation_id = f"corr-{connection_id}"
        await self.events.add(
            batch_id=self.batch_id,
            request_id=plan.request_id,
            hop="client",
            connection_id=connection_id,
            correlation_id=correlation_id,
            kind="connect_start",
            initiator="client",
            details={
                "direction": "client_to_proxy",
                "target": f"{self.proxy_host}:{self.proxy_port}",
            },
        )
        try:
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(self.proxy_host, self.proxy_port),
                timeout=self.per_hop_timeout_ms / 1000.0,
            )
        except asyncio.TimeoutError:
            await self.events.add(
                batch_id=self.batch_id,
                request_id=plan.request_id,
                hop="client",
                connection_id=connection_id,
                correlation_id=correlation_id,
                kind="connect_timeout",
                initiator="timer",
                details={
                    "direction": "client_to_proxy",
                    "timeout_ms": self.per_hop_timeout_ms,
                },
            )
            raise
        except OSError as exc:
            await self.events.add(
                batch_id=self.batch_id,
                request_id=plan.request_id,
                hop="client",
                connection_id=connection_id,
                correlation_id=correlation_id,
                kind="connect_fail",
                initiator="client",
                details={"direction": "client_to_proxy", "error": type(exc).__name__},
            )
            raise
        await self.events.add(
            batch_id=self.batch_id,
            request_id=plan.request_id,
            hop="client",
            connection_id=connection_id,
            correlation_id=correlation_id,
            kind="connect_success",
            initiator="client",
            details={
                "direction": "client_to_proxy",
                "local": self._name(writer.get_extra_info("sockname")),
                "remote": self._name(writer.get_extra_info("peername")),
            },
        )
        return ClientConnection(
            connection_id=connection_id,
            reader=reader,
            writer=writer,
            last_request_id=plan.request_id,
        )

    async def _send_request(
        self, connection: ClientConnection, plan: RequestPlan, injected: bool
    ) -> None:
        body = self._body(plan)
        connection_type = "keep-alive" if plan.path == "keepalive" else "close"
        headers = {
            "Host": f"{self.upstream_host}:{self.upstream_port}",
            "Content-Length": str(len(body)),
            "Connection": connection_type,
            "X-Batch-Id": self.batch_id,
            "X-Request-Id": plan.request_id,
            "X-Client-Conn-Id": connection.connection_id,
            "X-Upstream-Target": f"{self.upstream_host}:{self.upstream_port}",
            "X-Per-Hop-Timeout-Ms": str(self.per_hop_timeout_ms),
            "X-Injection-Match": "1" if injected else "0",
            "X-Injection-Type": self.injection["type"] if injected else "none",
            "X-Injection-Id": self.injection["id"],
            "X-ConnectLab-Path": plan.path,
        }
        absolute_target = (
            f"http://{self.upstream_host}:{self.upstream_port}"
            f"{self._path_for(plan)}"
        )
        head = encode_forward_request(plan.method, absolute_target, headers, len(body))
        connection.writer.write(head)
        await connection.writer.drain()
        await self.events.add(
            batch_id=self.batch_id,
            request_id=plan.request_id,
            hop="client",
            connection_id=connection.connection_id,
            correlation_id=f"corr-{connection.connection_id}",
            kind="request_sent",
            initiator="client",
            details={
                "body_size": len(body),
                "method": plan.method,
                "path": plan.path,
                "reused_connection": connection.connection_id
                == (
                    self.keepalive_connection.connection_id
                    if self.keepalive_connection is not None
                    else None
                ),
            },
        )
        offset = 0
        while offset < len(body):
            piece = body[offset : offset + 65536]
            connection.writer.write(piece)
            await connection.writer.drain()
            offset += len(piece)

    async def _read_response(
        self, connection: ClientConnection, plan: RequestPlan
    ) -> tuple[int, dict[str, str]]:
        timeout_ms = max(
            self.per_hop_timeout_ms * 2,
            self.per_hop_timeout_ms + 500,
        )
        status, _reason, headers = await read_response_head(
            connection.reader, timeout_ms
        )
        await self.events.add(
            batch_id=self.batch_id,
            request_id=plan.request_id,
            hop="client",
            connection_id=connection.connection_id,
            correlation_id=f"corr-{connection.connection_id}",
            kind="response_start",
            initiator="proxy",
            details={"status": status},
        )
        length = content_length(headers)
        if length:
            await read_exact(connection.reader, length)
        await self.events.add(
            batch_id=self.batch_id,
            request_id=plan.request_id,
            hop="client",
            connection_id=connection.connection_id,
            correlation_id=f"corr-{connection.connection_id}",
            kind="response_complete",
            initiator="client",
            details={"status": status, "response_bytes": length},
        )
        return status, headers

    async def _close_connection(
        self,
        connection: ClientConnection,
        request_id: str,
        *,
        initiator: str,
        reason: str,
    ) -> None:
        try:
            await self.events.add(
                batch_id=self.batch_id,
                request_id=request_id,
                hop="client",
                connection_id=connection.connection_id,
                correlation_id=f"corr-{connection.connection_id}",
                kind="close",
                initiator=initiator,
                details={"reason": reason, "direction": "client_to_proxy"},
            )
        except ValueError:
            pass
        try:
            connection.writer.close()
            await connection.writer.wait_closed()
        except (ConnectionError, OSError):
            pass

    async def close(self, request_id: str) -> None:
        if self.keepalive_connection is not None:
            connection = self.keepalive_connection
            self.keepalive_connection = None
            await self._close_connection(
                connection,
                request_id,
                initiator="client",
                reason="batch_complete",
            )

    def _body(self, plan: RequestPlan) -> bytes:
        if plan.method != "POST" or plan.body_size == 0:
            return b""
        if plan.body_file is None:
            return b""
        key = str(plan.body_file)
        if key not in self._body_cache:
            with open(plan.body_file, "rb") as handle:
                self._body_cache[key] = handle.read(plan.body_size)
        return self._body_cache[key]

    @staticmethod
    def _path_for(plan: RequestPlan) -> str:
        return f"/{plan.path}"

    @staticmethod
    def _name(value: Any) -> str | None:
        if not value:
            return None
        return f"{value[0]}:{value[1]}"
