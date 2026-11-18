from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Any

from .events import EventStore
from .http import (
    HttpParseError,
    content_length,
    encode_forward_request,
    force_reset,
    normalize_target,
    read_request_head,
    read_response_head,
)
from .injection import TYPE_SYN_DROP
from .util import SequentialIds, mono_ms, utc_now


LOGGER = logging.getLogger("connectlab.proxy")


@dataclass
class UpstreamConnection:
    connection_id: str
    correlation_id: str
    batch_id: str
    target: str
    reader: asyncio.StreamReader
    writer: asyncio.StreamWriter
    last_request_id: str
    created_at: str
    reuse_count: int = 0
    closed: bool = False


class UpstreamPool:
    def __init__(self, limit: int) -> None:
        self.limit = limit
        self._idle: dict[tuple[str, str], list[UpstreamConnection]] = {}
        self._lock = asyncio.Lock()

    async def take(
        self, batch_id: str, target: str
    ) -> UpstreamConnection | None:
        async with self._lock:
            bucket = self._idle.get((batch_id, target), [])
            while bucket:
                connection = bucket.pop()
                if connection.closed or connection.writer.is_closing():
                    connection.closed = True
                    continue
                return connection
            return None

    async def put(self, connection: UpstreamConnection) -> None:
        if connection.closed or connection.writer.is_closing():
            return
        async with self._lock:
            bucket = self._idle.setdefault(
                (connection.batch_id, connection.target), []
            )
            if len(bucket) < self.limit:
                bucket.append(connection)
                return
        connection.closed = True
        connection.writer.close()

    async def pop_batch(
        self, batch_id: str
    ) -> list[UpstreamConnection]:
        async with self._lock:
            result: list[UpstreamConnection] = []
            for key in list(self._idle):
                if key[0] != batch_id:
                    continue
                result.extend(self._idle.pop(key))
            return result


class ProxyServer:
    def __init__(self, config: dict[str, Any], events: EventStore) -> None:
        self.config = config
        self.events = events
        self.ids = SequentialIds("p-up")
        self.down_ids = SequentialIds("p-down")
        self.pool = UpstreamPool(int(config["proxy"]["pool_size"]))
        self._server: asyncio.AbstractServer | None = None
        self._allowed_targets = {
            f"{item['upstream_host']}:{int(item['upstream_port'])}"
            for item in config.get("targets", {}).values()
        }

    async def start(self) -> None:
        cfg = self.config["proxy"]
        self._server = await asyncio.start_server(
            self.handle_client,
            cfg["listen_host"],
            int(cfg["listen_port"]),
            backlog=128,
        )
        LOGGER.info(
            "embedded observer proxy listening on %s:%s",
            cfg["listen_host"],
            cfg["listen_port"],
        )

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
        for batch_id in set(key[0] for key in list(self.pool._idle)):
            for connection in await self.pool.pop_batch(batch_id):
                connection.closed = True
                connection.writer.close()

    async def close_batch(self, batch_id: str) -> None:
        connections = await self.pool.pop_batch(batch_id)
        for connection in connections:
            if connection.closed:
                continue
            try:
                await self.events.add(
                    batch_id=batch_id,
                    request_id=connection.last_request_id,
                    hop="proxy",
                    connection_id=connection.connection_id,
                    correlation_id=connection.correlation_id,
                    kind="close",
                    initiator="proxy",
                    details={"reason": "batch_pool_drain", "reused_request_count": connection.reuse_count},
                )
            except ValueError:
                pass
            connection.closed = True
            connection.writer.close()

    async def handle_client(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        accepted_at = utc_now()
        accepted_mono = mono_ms()
        downstream_id = self.down_ids.next()
        downstream_correlation = f"corr-{downstream_id}"
        downstream_open_recorded = False
        downstream_close_recorded = False
        last_request_id = ""
        last_batch_id = ""
        client_connection_id = ""

        try:
            while True:
                timeout_ms = int(
                    self.config["proxy"]["downstream_idle_timeout_ms"]
                )
                try:
                    request = await read_request_head(reader, timeout_ms)
                except asyncio.TimeoutError:
                    break
                except (ConnectionResetError, asyncio.IncompleteReadError, HttpParseError):
                    if last_request_id:
                        await self._record_downstream_close(
                            last_batch_id,
                            last_request_id,
                            downstream_id,
                            downstream_correlation,
                            client_connection_id,
                            initiator="peer",
                            reason="downstream_read_error",
                        )
                        downstream_close_recorded = True
                    break

                if request is None:
                    if last_request_id:
                        await self._record_downstream_close(
                            last_batch_id,
                            last_request_id,
                            downstream_id,
                            downstream_correlation,
                            client_connection_id,
                            initiator="peer",
                            reason="downstream_eof",
                        )
                        downstream_close_recorded = True
                    break

                method, target, headers = request
                batch_id = headers.get("x-batch-id", "unknown")
                request_id = headers.get("x-request-id", f"{downstream_id}-r")
                client_connection_id = headers.get("x-client-conn-id", "")
                last_request_id = request_id
                last_batch_id = batch_id
                path = normalize_target(target, "127.0.0.1", 80)
                body_size = content_length(headers)
                per_hop_timeout_ms = int(headers.get("x-per-hop-timeout-ms", "1500"))
                upstream_target = headers.get("x-upstream-target", "")

                if not downstream_open_recorded:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=downstream_id,
                        peer_connection_id=client_connection_id or None,
                        correlation_id=downstream_correlation,
                        kind="connect_success",
                        initiator="client",
                        ts=accepted_at,
                        ts_mono_ms=accepted_mono,
                        details={
                            "direction": "client_to_proxy",
                            "remote": self._peer_name(writer),
                            "local": self._local_name(writer),
                        },
                    )
                    downstream_open_recorded = True

                await self.events.add(
                    batch_id=batch_id,
                    request_id=request_id,
                    hop="proxy",
                    connection_id=downstream_id,
                    peer_connection_id=client_connection_id or None,
                    correlation_id=downstream_correlation,
                    kind="request_received",
                    initiator="client",
                    details={
                        "method": method,
                        "path": path,
                        "body_size": body_size,
                    },
                )

                if not upstream_target:
                    upstream_target = next(iter(self._allowed_targets), "")
                if upstream_target not in self._allowed_targets:
                    await self._send_error(
                        writer,
                        request_id,
                        downstream_id,
                        downstream_correlation,
                        client_connection_id,
                        batch_id,
                        502,
                        "Bad Gateway",
                        "invalid_upstream_target",
                    )
                    break

                injection_applied = headers.get("x-injection-match", "0") == "1"
                injection_type = headers.get("x-injection-type", "none")
                close_downstream = "close" in headers.get("connection", "").lower()
                keep_upstream = not close_downstream

                if injection_applied and injection_type == TYPE_SYN_DROP:
                    attempt_id = self.ids.next()
                    attempt_corr = f"corr-{attempt_id}"
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=attempt_id,
                        correlation_id=attempt_corr,
                        kind="connect_start",
                        initiator="proxy",
                        details={"target": upstream_target, "direction": "proxy_to_upstream"},
                    )
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=attempt_id,
                        correlation_id=attempt_corr,
                        kind="syn_dropped",
                        initiator="local_fault_gate",
                        details={
                            "fault": TYPE_SYN_DROP,
                            "injected": True,
                            "syn_ack_received": False,
                            "target": upstream_target,
                            "phase": "tcp_handshake",
                        },
                    )
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=downstream_id,
                        peer_connection_id=client_connection_id or None,
                        correlation_id=downstream_correlation,
                        kind="reset",
                        initiator="proxy",
                        details={
                            "reason": "upstream_syn_not_reached",
                            "upstream_attempt": attempt_id,
                        },
                    )
                    downstream_close_recorded = True
                    force_reset(writer)
                    return

                upstream: UpstreamConnection | None = None
                reused = False
                if keep_upstream:
                    upstream = await self.pool.take(batch_id, upstream_target)
                if upstream is not None:
                    reused = True
                    upstream.reuse_count += 1
                    upstream.last_request_id = request_id
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=upstream.connection_id,
                        correlation_id=upstream.correlation_id,
                        kind="reuse_attempt",
                        initiator="proxy",
                        details={
                            "direction": "proxy_to_upstream",
                            "proxy_aware_before_send": bool(
                                upstream.closed or upstream.writer.is_closing()
                            ),
                            "reuse_index": upstream.reuse_count,
                            "target": upstream_target,
                        },
                    )

                if upstream is None:
                    upstream = await self._open_upstream(
                        batch_id=batch_id,
                        request_id=request_id,
                        target=upstream_target,
                        timeout_ms=min(
                            per_hop_timeout_ms,
                            int(self.config["proxy"]["connect_timeout_ms"]),
                        ),
                    )
                    if upstream is None:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="proxy",
                            connection_id=downstream_id,
                            peer_connection_id=client_connection_id or None,
                            correlation_id=downstream_correlation,
                            kind="reset",
                            initiator="proxy",
                            details={"reason": "upstream_connect_failed"},
                        )
                        downstream_close_recorded = True
                        force_reset(writer)
                        return

                try:
                    await self._forward_request(
                        reader,
                        writer,
                        upstream,
                        method=method,
                        path=path,
                        headers=headers,
                        body_size=body_size,
                        batch_id=batch_id,
                        request_id=request_id,
                        downstream_id=downstream_id,
                        downstream_correlation=downstream_correlation,
                        client_connection_id=client_connection_id,
                        timeout_ms=per_hop_timeout_ms,
                        reused=reused,
                        close_downstream=close_downstream,
                    )
                except asyncio.TimeoutError:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=upstream.connection_id,
                        correlation_id=upstream.correlation_id,
                        kind="timeout",
                        initiator="timer",
                        details={
                            "phase": "upstream_response_head",
                            "timeout_ms": per_hop_timeout_ms,
                            "reused": reused,
                            "fault_class": "proxy_read_timeout",
                        },
                    )
                    upstream.closed = True
                    force_reset(upstream.writer)
                    await self._send_error(
                        writer,
                        request_id,
                        downstream_id,
                        downstream_correlation,
                        client_connection_id,
                        batch_id,
                        504,
                        "Gateway Timeout",
                        "proxy_read_timeout",
                    )
                    downstream_close_recorded = True
                    return
                except (ConnectionResetError, asyncio.IncompleteReadError, BrokenPipeError) as exc:
                    details = {
                        "phase": "upstream_io",
                        "error": type(exc).__name__,
                        "reused": reused,
                        "proxy_aware_before_send": False if reused else True,
                        "detection": "write_or_read_failure",
                    }
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=upstream.connection_id,
                        correlation_id=upstream.correlation_id,
                        kind="reset",
                        initiator="peer",
                        details=details,
                    )
                    if reused:
                        await self.events.add(
                            batch_id=batch_id,
                            request_id=request_id,
                            hop="proxy",
                            connection_id=upstream.connection_id,
                            correlation_id=upstream.correlation_id,
                            kind="reuse_failure",
                            initiator="proxy",
                            details={
                                "fault_class": "reuse_failed_connection",
                                "old_connection": True,
                                "proxy_aware_before_send": False,
                            },
                        )
                    upstream.closed = True
                    force_reset(upstream.writer)
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=downstream_id,
                        peer_connection_id=client_connection_id or None,
                        correlation_id=downstream_correlation,
                        kind="reset",
                        initiator="proxy",
                        details={"reason": "upstream_connection_lost", "reused": reused},
                    )
                    downstream_close_recorded = True
                    force_reset(writer)
                    return

                if close_downstream or not keep_upstream:
                    await self.events.add(
                        batch_id=batch_id,
                        request_id=request_id,
                        hop="proxy",
                        connection_id=downstream_id,
                        peer_connection_id=client_connection_id or None,
                        correlation_id=downstream_correlation,
                        kind="close",
                        initiator="proxy",
                        details={"reason": "request_connection_close"},
                    )
                    downstream_close_recorded = True
                    writer.close()
                    await writer.wait_closed()
                    return
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            LOGGER.exception("proxy connection error")
            if last_request_id:
                try:
                    await self.events.add(
                        batch_id=last_batch_id,
                        request_id=last_request_id,
                        hop="proxy",
                        connection_id=downstream_id,
                        peer_connection_id=client_connection_id or None,
                        correlation_id=downstream_correlation,
                        kind="reset",
                        initiator="proxy",
                        details={"error": type(exc).__name__, "message": str(exc)},
                    )
                    downstream_close_recorded = True
                except ValueError:
                    pass
        finally:
            if last_request_id and not downstream_close_recorded:
                try:
                    await self._record_downstream_close(
                        last_batch_id,
                        last_request_id,
                        downstream_id,
                        downstream_correlation,
                        client_connection_id,
                        initiator="peer",
                        reason="downstream_handler_exit",
                    )
                except ValueError:
                    pass
            try:
                writer.close()
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass

    async def _open_upstream(
        self,
        *,
        batch_id: str,
        request_id: str,
        target: str,
        timeout_ms: int,
    ) -> UpstreamConnection | None:
        connection_id = self.ids.next()
        correlation_id = f"corr-{connection_id}"
        host, port_text = target.rsplit(":", 1)
        port = int(port_text)
        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=connection_id,
            correlation_id=correlation_id,
            kind="connect_start",
            initiator="proxy",
            details={"target": target, "direction": "proxy_to_upstream"},
        )
        try:
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(host, port),
                timeout=max(timeout_ms, 1) / 1000.0,
            )
        except asyncio.TimeoutError:
            await self.events.add(
                batch_id=batch_id,
                request_id=request_id,
                hop="proxy",
                connection_id=connection_id,
                correlation_id=correlation_id,
                kind="connect_timeout",
                initiator="timer",
                details={"target": target, "timeout_ms": timeout_ms},
            )
            return None
        except OSError as exc:
            await self.events.add(
                batch_id=batch_id,
                request_id=request_id,
                hop="proxy",
                connection_id=connection_id,
                correlation_id=correlation_id,
                kind="connect_fail",
                initiator="proxy",
                details={"target": target, "error": type(exc).__name__},
            )
            return None
        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=connection_id,
            correlation_id=correlation_id,
            kind="connect_success",
            initiator="proxy",
            details={
                "target": target,
                "direction": "proxy_to_upstream",
                "local": self._local_name(writer),
            },
        )
        return UpstreamConnection(
            connection_id=connection_id,
            correlation_id=correlation_id,
            batch_id=batch_id,
            target=target,
            reader=reader,
            writer=writer,
            last_request_id=request_id,
            created_at=utc_now(),
        )

    async def _forward_request(
        self,
        downstream_reader: asyncio.StreamReader,
        downstream_writer: asyncio.StreamWriter,
        upstream: UpstreamConnection,
        *,
        method: str,
        path: str,
        headers: dict[str, str],
        body_size: int,
        batch_id: str,
        request_id: str,
        downstream_id: str,
        downstream_correlation: str,
        client_connection_id: str,
        timeout_ms: int,
        reused: bool,
        close_downstream: bool,
    ) -> None:
        hop_by_hop = {
            "connection",
            "proxy-connection",
            "keep-alive",
            "te",
            "trailer",
            "transfer-encoding",
            "upgrade",
        }
        forwarded: dict[str, str] = {}
        for key, value in headers.items():
            if key.lower() in hop_by_hop:
                continue
            forwarded[key] = value
        forwarded["connection"] = "close" if close_downstream else "keep-alive"
        forwarded["x-batch-id"] = batch_id
        forwarded["x-request-id"] = request_id
        forwarded["x-client-conn-id"] = client_connection_id
        forwarded["x-proxy-downstream-conn-id"] = downstream_id
        forwarded["x-proxy-upstream-conn-id"] = upstream.connection_id
        forwarded["x-upstream-correlation-id"] = upstream.correlation_id
        forwarded["x-per-hop-timeout-ms"] = str(timeout_ms)

        upstream.writer.write(
            encode_forward_request(method, path, forwarded, body_size)
        )
        await upstream.writer.drain()

        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=upstream.connection_id,
            correlation_id=upstream.correlation_id,
            kind="request_forwarded",
            initiator="proxy",
            details={"method": method, "path": path, "body_size": body_size, "reused": reused},
        )

        remaining = body_size
        while remaining:
            piece = await downstream_reader.read(min(65536, remaining))
            if not piece:
                raise ConnectionResetError("downstream closed during upload")
            upstream.writer.write(piece)
            await upstream.writer.drain()
            remaining -= len(piece)

        if body_size:
            await self.events.add(
                batch_id=batch_id,
                request_id=request_id,
                hop="proxy",
                connection_id=upstream.connection_id,
                correlation_id=upstream.correlation_id,
                kind="request_body_forwarded",
                initiator="proxy",
                details={"body_size": body_size, "reused": reused},
            )

        status, reason, response_headers = await read_response_head(
            upstream.reader, timeout_ms
        )
        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=upstream.connection_id,
            correlation_id=upstream.correlation_id,
            kind="response_start",
            initiator="upstream",
            details={"status": status, "reused": reused},
        )

        length = content_length(response_headers)
        response_connection = response_headers.get("connection", "").lower()
        upstream_requested_close = "close" in response_connection or close_downstream
        output_headers = {
            "Content-Type": response_headers.get("content-type", "application/octet-stream"),
            "Content-Length": str(length),
            "Connection": "close" if upstream_requested_close else "keep-alive",
            "X-Proxy-Downstream-Connection-Id": downstream_id,
            "X-Proxy-Upstream-Connection-Id": upstream.connection_id,
            "X-Upstream-Connection-Id": response_headers.get(
                "x-upstream-connection-id", ""
            ),
            "X-Upstream-Correlation-Id": response_headers.get(
                "x-upstream-correlation-id", upstream.correlation_id
            ),
            "X-ConnectLab-Reuse": "1" if reused else "0",
        }
        status_line = f"HTTP/1.1 {status} {reason}\r\n"
        head = status_line + "".join(
            f"{key}: {value}\r\n" for key, value in output_headers.items()
        ) + "\r\n"
        downstream_writer.write(head.encode("latin-1"))

        remaining = length
        while remaining:
            piece = await upstream.reader.read(min(65536, remaining))
            if not piece:
                raise ConnectionResetError("upstream closed during response body")
            downstream_writer.write(piece)
            remaining -= len(piece)
        await downstream_writer.drain()

        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=upstream.connection_id,
            correlation_id=upstream.correlation_id,
            kind="response_complete",
            initiator="proxy",
            details={"status": status, "response_bytes": length, "reused": reused},
        )

        if upstream_requested_close:
            await self.events.add(
                batch_id=batch_id,
                request_id=request_id,
                hop="proxy",
                connection_id=upstream.connection_id,
                correlation_id=upstream.correlation_id,
                kind="close",
                initiator="proxy",
                details={"reason": "upstream_connection_close_semantics"},
            )
            upstream.closed = True
            upstream.writer.close()
        else:
            await self.pool.put(upstream)

    async def _send_error(
        self,
        writer: asyncio.StreamWriter,
        request_id: str,
        downstream_id: str,
        downstream_correlation: str,
        client_connection_id: str,
        batch_id: str,
        status: int,
        reason: str,
        error_code: str,
    ) -> None:
        body = f'{{"code":"{error_code}","request_id":"{request_id}"}}'.encode("utf-8")
        head = (
            f"HTTP/1.1 {status} {reason}\r\n"
            "Content-Type: application/json\r\n"
            f"Content-Length: {len(body)}\r\n"
            "Connection: close\r\n"
            f"X-Proxy-Downstream-Connection-Id: {downstream_id}\r\n"
            f"X-ConnectLab-Error: {error_code}\r\n"
            "\r\n"
        ).encode("latin-1")
        try:
            writer.write(head + body)
            await writer.drain()
        except (ConnectionError, OSError):
            pass

    async def _record_downstream_close(
        self,
        batch_id: str,
        request_id: str,
        downstream_id: str,
        correlation_id: str,
        client_connection_id: str,
        *,
        initiator: str,
        reason: str,
    ) -> None:
        await self.events.add(
            batch_id=batch_id,
            request_id=request_id,
            hop="proxy",
            connection_id=downstream_id,
            peer_connection_id=client_connection_id or None,
            correlation_id=correlation_id,
            kind="close",
            initiator=initiator,
            details={"reason": reason, "direction": "client_to_proxy"},
        )

    @staticmethod
    def _peer_name(writer: asyncio.StreamWriter) -> str | None:
        peer = writer.get_extra_info("peername")
        return f"{peer[0]}:{peer[1]}" if peer else None

    @staticmethod
    def _local_name(writer: asyncio.StreamWriter) -> str | None:
        local = writer.get_extra_info("sockname")
        return f"{local[0]}:{local[1]}" if local else None
