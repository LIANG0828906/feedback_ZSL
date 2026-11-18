from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import socket
from typing import Any

from .events import EventStore
from .http import (
    HttpParseError,
    content_length,
    encode_response,
    force_reset,
    read_request_head,
)
from .injection import TYPE_PROXY_READ_TIMEOUT, TYPE_UPSTREAM_DISCONNECT
from .util import SequentialIds, mono_ms, utc_now


LOGGER = logging.getLogger("connectlab.upstream")


class UpstreamServer:
    def __init__(self, config: dict[str, Any], events: EventStore) -> None:
        self.config = config
        self.events = events
        self.ids = SequentialIds("u")
        self._servers: list[asyncio.AbstractServer] = []

    async def start(self) -> None:
        cfg = self.config["upstream"]
        ports = [int(cfg["listen_port"])]
        for port in cfg.get("extra_ports", []):
            parsed = int(port)
            if parsed not in ports:
                ports.append(parsed)
        for port in ports:
            server = await asyncio.start_server(
                self.handle_client,
                cfg["listen_host"],
                port,
                backlog=128,
            )
            self._servers.append(server)
            LOGGER.info("upstream listening on %s:%s", cfg["listen_host"], port)

    async def stop(self) -> None:
        for server in self._servers:
            server.close()
            await server.wait_closed()
        self._servers.clear()

    async def handle_client(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        accepted_at = utc_now()
        accepted_mono = mono_ms()
        connection_id = self.ids.next()
        batch_id = ""
        request_id = ""
        correlation_id = f"corr-{connection_id}"
        proxy_connection_id: str | None = None
        first_request = True
        close_recorded = False

        try:
            while True:
                timeout_ms = int(self.config["upstream"]["read_timeout_ms"])
                try:
                    request = await read_request_head(reader, timeout_ms)
                except asyncio.TimeoutError:
                    if request_id:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="upstream",
                            connection_id=connection_id,
                            peer_connection_id=proxy_connection_id,
                            correlation_id=correlation_id,
                            kind="timeout",
                            initiator="timer",
                            details={"phase": "request_head", "timeout_ms": timeout_ms},
                        )
                    break
                except (ConnectionResetError, asyncio.IncompleteReadError, HttpParseError) as exc:
                    if request_id:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="upstream",
                            connection_id=connection_id,
                            peer_connection_id=proxy_connection_id,
                            correlation_id=correlation_id,
                            kind="reset",
                            initiator="peer",
                            details={"error": type(exc).__name__, "message": str(exc)},
                        )
                        close_recorded = True
                    break

                if request is None:
                    if request_id:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="upstream",
                            connection_id=connection_id,
                            peer_connection_id=proxy_connection_id,
                            correlation_id=correlation_id,
                            kind="close",
                            initiator="peer",
                            details={"reason": "eof_before_next_request"},
                        )
                        close_recorded = True
                    break

                method, target, headers = request
                request_id = headers.get("x-request-id", f"{connection_id}-r")
                batch_id = headers.get("x-batch-id", "unknown")
                correlation_id = headers.get("x-upstream-correlation-id", correlation_id)
                proxy_connection_id = headers.get("x-proxy-upstream-conn-id")
                path = target.split("?", 1)[0]
                body_size = content_length(headers)
                injection_type = headers.get("x-injection-type", "none")
                injection_applied = headers.get("x-injection-match", "0") == "1"

                if first_request:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="connect_success",
                        initiator="proxy",
                        ts=accepted_at,
                        ts_mono_ms=accepted_mono,
                        details={
                            "remote": self._peer_name(writer),
                            "local": self._local_name(writer),
                        },
                    )
                    first_request = False

                await self.events.add(
                    batch_id=batch_id,
                    request_id=request_id,
                    hop="upstream",
                    connection_id=connection_id,
                    peer_connection_id=proxy_connection_id,
                    correlation_id=correlation_id,
                    kind="request_received",
                    initiator="proxy",
                    details={
                        "method": method,
                        "path": path,
                        "body_size": body_size,
                        "injection_type": injection_type if injection_applied else "none",
                    },
                )

                injected_disconnect = (
                    injection_applied and injection_type == TYPE_UPSTREAM_DISCONNECT
                )
                injected_read_timeout = (
                    injection_applied and injection_type == TYPE_PROXY_READ_TIMEOUT
                )

                if injected_disconnect and path == "/upload":
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="reset",
                        initiator="upstream",
                        details={
                            "fault": TYPE_UPSTREAM_DISCONNECT,
                            "phase": "before_upload_body",
                            "injected": True,
                        },
                    )
                    close_recorded = True
                    force_reset(writer)
                    break

                try:
                    digest = await self._receive_body(reader, body_size)
                except (ConnectionResetError, asyncio.IncompleteReadError) as exc:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="reset",
                        initiator="peer",
                        details={"phase": "request_body", "error": type(exc).__name__},
                    )
                    close_recorded = True
                    break

                await self.events.add(
                    batch_id=batch_id,
                    request_id=request_id,
                    hop="upstream",
                    connection_id=connection_id,
                    peer_connection_id=proxy_connection_id,
                    correlation_id=correlation_id,
                    kind="request_body_complete",
                    initiator="upstream",
                    details={"body_size": body_size, "sha256": digest},
                )

                if injected_read_timeout:
                    per_hop = int(headers.get("x-per-hop-timeout-ms", "1500"))
                    extra = int(self.config["upstream"]["proxy_read_timeout_extra_ms"])
                    delay_ms = per_hop + extra
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="response_delayed",
                        initiator="upstream",
                        details={
                            "fault": TYPE_PROXY_READ_TIMEOUT,
                            "delay_ms": delay_ms,
                            "injected": True,
                        },
                    )
                    await asyncio.sleep(delay_ms / 1000.0)
                    if writer.is_closing():
                        close_recorded = True
                        break
                    try:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="upstream",
                            connection_id=connection_id,
                            peer_connection_id=proxy_connection_id,
                            correlation_id=correlation_id,
                            kind="write_failed",
                            initiator="upstream",
                            details={"fault": TYPE_PROXY_READ_TIMEOUT, "reason": "proxy_gone"},
                        )
                    except ValueError:
                        pass
                    close_recorded = True
                    force_reset(writer)
                    break

                if injected_disconnect:
                    if path == "/keepalive":
                        await self._send_response(
                            writer,
                            request_id=request_id,
                            batch_id=batch_id,
                            connection_id=connection_id,
                            correlation_id=correlation_id,
                            proxy_connection_id=proxy_connection_id,
                            path=path,
                            body_size=body_size,
                            digest=digest,
                            close_after=False,
                        )
                        grace_ms = int(
                            self.config["upstream"]["early_disconnect_after_response_ms"]
                        )
                        await asyncio.sleep(grace_ms / 1000.0)
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="upstream",
                            connection_id=connection_id,
                            peer_connection_id=proxy_connection_id,
                            correlation_id=correlation_id,
                            kind="reset",
                            initiator="upstream",
                            details={
                                "fault": TYPE_UPSTREAM_DISCONNECT,
                                "phase": "after_response",
                                "injected": True,
                                "grace_ms": grace_ms,
                            },
                        )
                        close_recorded = True
                        force_reset(writer)
                        break

                    await asyncio.sleep(
                        int(self.config["upstream"]["early_disconnect_before_response_ms"])
                        / 1000.0
                    )
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="reset",
                        initiator="upstream",
                        details={
                            "fault": TYPE_UPSTREAM_DISCONNECT,
                            "phase": "before_response",
                            "injected": True,
                        },
                    )
                    close_recorded = True
                    force_reset(writer)
                    break

                request_connection = headers.get("connection", "").lower()
                close_after = "close" in request_connection
                await self._send_response(
                    writer,
                    request_id=request_id,
                    batch_id=batch_id,
                    connection_id=connection_id,
                    correlation_id=correlation_id,
                    proxy_connection_id=proxy_connection_id,
                    path=path,
                    body_size=body_size,
                    digest=digest,
                    close_after=close_after,
                )
                if close_after:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="close",
                        initiator="upstream",
                        details={"reason": "connection_close_semantics"},
                    )
                    close_recorded = True
                    writer.close()
                    await writer.wait_closed()
                    break
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            LOGGER.exception("upstream connection error")
            if request_id:
                try:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="reset",
                        initiator="upstream",
                        details={"error": type(exc).__name__, "message": str(exc)},
                    )
                    close_recorded = True
                except ValueError:
                    pass
        finally:
            if not close_recorded and request_id:
                try:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="upstream",
                        connection_id=connection_id,
                        peer_connection_id=proxy_connection_id,
                        correlation_id=correlation_id,
                        kind="close",
                        initiator="upstream",
                        details={"reason": "handler_exit"},
                    )
                except ValueError:
                    pass
            try:
                writer.close()
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass

    async def _receive_body(
        self, reader: asyncio.StreamReader, body_size: int
    ) -> str:
        digest = hashlib.sha256()
        remaining = body_size
        while remaining:
            piece = await reader.read(min(65536, remaining))
            if not piece:
                raise ConnectionResetError("peer closed during upload")
            digest.update(piece)
            remaining -= len(piece)
        return digest.hexdigest()

    async def _send_response(
        self,
        writer: asyncio.StreamWriter,
        *,
        request_id: str,
        batch_id: str,
        connection_id: str,
        correlation_id: str,
        proxy_connection_id: str | None,
        path: str,
        body_size: int,
        digest: str,
        close_after: bool,
    ) -> None:
        body = json.dumps(
            {
                "batch_id": batch_id,
                "connection_id": connection_id,
                "path": path,
                "received_bytes": body_size,
                "request_id": request_id,
                "sha256": digest,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="upstream",
            connection_id=connection_id,
            peer_connection_id=proxy_connection_id,
            correlation_id=correlation_id,
            kind="response_start",
            initiator="upstream",
            details={"status": 200, "path": path},
        )
        delay_ms = int(self.config["upstream"]["write_delay_ms"])
        if delay_ms:
            await asyncio.sleep(delay_ms / 1000.0)
        headers = {
            "Content-Type": "application/json",
            "Connection": "close" if close_after else "keep-alive",
            "X-Upstream-Connection-Id": connection_id,
            "X-Upstream-Correlation-Id": correlation_id,
        }
        writer.write(encode_response(200, "OK", headers, body))
        await writer.drain()

    @staticmethod
    def _peer_name(writer: asyncio.StreamWriter) -> str | None:
        peer = writer.get_extra_info("peername")
        if not peer:
            return None
        return f"{peer[0]}:{peer[1]}"

    @staticmethod
    def _local_name(writer: asyncio.StreamWriter) -> str | None:
        local = writer.get_extra_info("sockname")
        if not local:
            return None
        return f"{local[0]}:{local[1]}"
