"""HTTP/2 (h2c) server/client helpers built on the h2 library."""
from __future__ import annotations

import asyncio
from dataclasses import dataclass

import h2.config
import h2.connection
import h2.events
import h2.settings

from .h1 import RawResponse, RequestError

H2_PREFACE_HEAD = b"PRI * HTTP/2.0\r\n\r\n"
BIG_WINDOW = 16 * 1024 * 1024


@dataclass
class H2Resp:
    status: int
    headers: list[tuple[str, str]]
    body: bytes


class H2Reset(Exception):
    """Handler asks to reset the whole TCP connection abruptly."""


async def _pump(conn, writer, pending: dict[int, tuple[list, bytes, int]]):
    """Send queued response data respecting flow control."""
    for sid in list(pending):
        headers, body, offset = pending[sid]
        if headers is not None:
            conn.send_headers(sid, headers, end_stream=(len(body) == 0))
            headers = None
        while offset < len(body):
            window = min(conn.local_flow_control_window(sid),
                         conn.max_outbound_frame_size)
            if window <= 0:
                break
            chunk = body[offset:offset + window]
            offset += len(chunk)
            conn.send_data(sid, chunk, end_stream=(offset == len(body)))
        if offset >= len(body):
            del pending[sid]
        else:
            pending[sid] = (headers, body, offset)
    data = conn.data_to_send()
    if data:
        writer.write(data)
        await writer.drain()


async def run_h2_server(reader: asyncio.StreamReader,
                        writer: asyncio.StreamWriter, handler,
                        upgraded_settings: bytes | None = None,
                        first_request: H2Resp | None = None,
                        first_request_meta: tuple | None = None,
                        preface_head: bytes = b"") -> None:
    """Drive one h2 server connection.

    handler(method, path, headers) -> H2Resp, or raises H2Reset.
    For upgraded connections pass upgraded_settings (HTTP2-Settings header
    value) and first_request_meta=(method, path, headers) for stream 1.
    """
    config = h2.config.H2Configuration(client_side=False,
                                       header_encoding="utf-8")
    conn = h2.connection.H2Connection(config=config)
    if upgraded_settings is not None:
        conn.initiate_upgrade_connection(upgraded_settings)
    else:
        conn.initiate_connection()
    writer.write(conn.data_to_send())
    await writer.drain()

    pending: dict[int, tuple[list, bytes, int]] = {}
    streams: dict[int, dict] = {}

    async def answer(sid: int, method: str, path: str,
                     headers: list[tuple[str, str]]):
        try:
            resp = await handler(method, path, headers)
        except H2Reset:
            transport = writer.transport
            if transport is not None:
                transport.abort()
            return "reset"
        hdrs = [(":status", str(resp.status))] + list(resp.headers)
        pending[sid] = (hdrs, resp.body, 0)
        return None

    if first_request_meta is not None:
        method, path, headers = first_request_meta
        if await answer(1, method, path, headers) == "reset":
            return
    await _pump(conn, writer, pending)

    if preface_head:
        # prior-knowledge: the head of the preface was already consumed
        conn.receive_data(preface_head)

    while True:
        try:
            data = await reader.read(65535)
        except ConnectionResetError:
            return
        if not data:
            return
        try:
            events = conn.receive_data(data)
        except Exception:
            return
        reset = False
        for event in events:
            if isinstance(event, h2.events.RequestReceived):
                streams[event.stream_id] = {"headers": event.headers}
            elif isinstance(event, h2.events.StreamEnded):
                info = streams.pop(event.stream_id, None)
                if info is None:
                    continue
                hdrs = {k: v for k, v in info["headers"]}
                method = hdrs.get(":method", "GET")
                path = hdrs.get(":path", "/")
                result = await answer(event.stream_id, method, path,
                                      info["headers"])
                if result == "reset":
                    reset = True
                    break
            elif isinstance(event, (h2.events.ConnectionTerminated,
                                    h2.events.StreamReset)):
                if isinstance(event, h2.events.ConnectionTerminated):
                    return
        if reset:
            return
        await _pump(conn, writer, pending)


class H2ClientConn:
    """Prior-knowledge h2c client, one request at a time."""

    def __init__(self, reader, writer, conn):
        self._reader = reader
        self._writer = writer
        self._conn = conn
        self._buf_events: list = []

    @classmethod
    async def connect(cls, host: str, port: int,
                      timeout: float = 15.0) -> "H2ClientConn":
        async def _go():
            reader, writer = await asyncio.open_connection(host, port)
            config = h2.config.H2Configuration(client_side=True,
                                               header_encoding="utf-8")
            conn = h2.connection.H2Connection(config=config)
            conn.initiate_connection()
            conn.update_settings(
                {h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: BIG_WINDOW})
            conn.increment_flow_control_window(BIG_WINDOW)
            writer.write(conn.data_to_send())
            await writer.drain()
            return cls(reader, writer, conn)
        return await asyncio.wait_for(_go(), timeout)

    async def request(self, method: str, path: str, authority: str,
                      headers: list[tuple[str, str]] | None = None,
                      timeout: float = 15.0) -> RawResponse:
        conn = self._conn
        sid = conn.get_next_available_stream_id()
        hdrs = [(":method", method), (":path", path), (":scheme", "http"),
                (":authority", authority)]
        for k, v in (headers or []):
            hdrs.append((k.lower(), v))
        conn.send_headers(sid, hdrs, end_stream=True)
        self._writer.write(conn.data_to_send())
        await self._writer.drain()

        resp_headers: list[tuple[str, str]] = []
        body = bytearray()
        status = 0

        async def _wait():
            nonlocal status
            while True:
                data = await self._reader.read(65535)
                if not data:
                    raise RequestError(f"h2 connection closed on {path}")
                events = self._conn.receive_data(data)
                for event in events:
                    if isinstance(event, h2.events.ResponseReceived):
                        resp_headers.extend(event.headers)
                        for k, v in event.headers:
                            if k == ":status":
                                status = int(v)
                    elif isinstance(event, h2.events.DataReceived):
                        body.extend(event.data)
                        self._conn.acknowledge_received_data(
                            event.flow_controlled_length, event.stream_id)
                    elif isinstance(event, h2.events.StreamEnded):
                        return
                    elif isinstance(event, (h2.events.StreamReset,
                                            h2.events.ConnectionTerminated)):
                        raise RequestError(f"h2 stream reset on {path}")

        await asyncio.wait_for(_wait(), timeout)
        out = self._conn.data_to_send()
        if out:
            self._writer.write(out)
            await self._writer.drain()
        return RawResponse(status=status, headers=resp_headers,
                           body=bytes(body), http_version="HTTP/2")

    async def close(self):
        try:
            self._writer.close()
        except Exception:
            pass


class H2CUpgradedClient:
    """Client for backend C: HTTP/1.1 Upgrade: h2c then multiplexed h2."""

    def __init__(self, reader, writer, conn):
        self._reader = reader
        self._writer = writer
        self._conn = conn
        self._pending: dict[int, asyncio.Future] = {}
        self._responses: dict[int, dict] = {}
        self._reader_task: asyncio.Task | None = None
        self.alive = True

    @classmethod
    async def connect(cls, host: str, port: int, method: str, path: str,
                      authority: str,
                      headers: list[tuple[str, str]] | None = None,
                      timeout: float = 15.0) -> tuple["H2CUpgradedClient", RawResponse]:
        """Perform the h2c upgrade; the triggering request is stream 1."""
        async def _go():
            reader, writer = await asyncio.open_connection(host, port)
            config = h2.config.H2Configuration(client_side=True,
                                               header_encoding="utf-8")
            conn = h2.connection.H2Connection(config=config)
            settings_header = conn.initiate_upgrade_connection()
            lines = [f"{method} {path} HTTP/1.1", f"Host: {authority}",
                     "Connection: Upgrade, HTTP2-Settings", "Upgrade: h2c",
                     f"HTTP2-Settings: {settings_header.decode()}"]
            for k, v in (headers or []):
                lines.append(f"{k}: {v}")
            writer.write(("\r\n".join(lines) + "\r\n\r\n").encode())
            await writer.drain()
            head = await reader.readuntil(b"\r\n\r\n")
            status_line = head.decode("iso-8859-1").split("\r\n")[0]
            if " 101 " not in status_line:
                raise RequestError(f"h2c upgrade refused: {status_line}")
            conn.update_settings(
                {h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: BIG_WINDOW})
            conn.increment_flow_control_window(BIG_WINDOW)
            writer.write(conn.data_to_send())
            await writer.drain()
            return reader, writer, conn
        reader, writer, conn = await asyncio.wait_for(_go(), timeout)
        client = cls(reader, writer, conn)
        client._reader_task = asyncio.ensure_future(client._read_loop())
        first = await client._await_stream(1, timeout)
        return client, first

    async def _read_loop(self):
        try:
            while True:
                data = await self._reader.read(65535)
                if not data:
                    raise RequestError("h2c connection closed by peer")
                events = self._conn.receive_data(data)
                for event in events:
                    sid = getattr(event, "stream_id", None)
                    if isinstance(event, h2.events.ResponseReceived):
                        self._responses.setdefault(sid, {"headers": [],
                                                         "body": bytearray(),
                                                         "done": None})
                        self._responses[sid]["headers"] = list(event.headers)
                    elif isinstance(event, h2.events.DataReceived):
                        if sid in self._responses:
                            self._responses[sid]["body"].extend(event.data)
                        self._conn.acknowledge_received_data(
                            event.flow_controlled_length, event.stream_id)
                    elif isinstance(event, h2.events.StreamEnded):
                        if sid in self._responses:
                            self._finish(sid)
                    elif isinstance(event, (h2.events.StreamReset,
                                            h2.events.ConnectionTerminated)):
                        raise RequestError(f"h2c reset (event={event})")
                out = self._conn.data_to_send()
                if out:
                    self._writer.write(out)
                    await self._writer.drain()
        except Exception as exc:
            self.alive = False
            for sid, fut in list(self._pending.items()):
                if not fut.done():
                    fut.set_exception(exc)
            self._pending.clear()

    def _finish(self, sid: int):
        info = self._responses.get(sid)
        if info is None:
            return
        status = 0
        for k, v in info["headers"]:
            if k == ":status":
                status = int(v)
        result = RawResponse(status=status, headers=info["headers"],
                             body=bytes(info["body"]), http_version="HTTP/2")
        info["result"] = result
        fut = self._pending.pop(sid, None)
        if fut is not None and not fut.done():
            fut.set_result(result)

    async def _await_stream(self, sid: int, timeout: float) -> RawResponse:
        info = self._responses.get(sid)
        if info is not None and info.get("result") is not None:
            return info["result"]
        fut: asyncio.Future = asyncio.get_event_loop().create_future()
        self._pending[sid] = fut
        info = self._responses.get(sid)
        if info is not None and info.get("result") is not None:
            self._pending.pop(sid, None)
            return info["result"]
        return await asyncio.wait_for(fut, timeout)

    async def request(self, method: str, path: str, authority: str,
                      headers: list[tuple[str, str]] | None = None,
                      timeout: float = 15.0) -> RawResponse:
        if not self.alive:
            raise RequestError("h2c session is dead")
        sid = self._conn.get_next_available_stream_id()
        self._responses[sid] = {"headers": [], "body": bytearray(),
                                "done": None}
        hdrs = [(":method", method), (":path", path), (":scheme", "http"),
                (":authority", authority)]
        for k, v in (headers or []):
            hdrs.append((k.lower(), v))
        self._conn.send_headers(sid, hdrs, end_stream=True)
        self._writer.write(self._conn.data_to_send())
        await self._writer.drain()
        return await self._await_stream(sid, timeout)

    async def close(self):
        self.alive = False
        if self._reader_task is not None:
            self._reader_task.cancel()
        try:
            self._writer.close()
        except Exception:
            pass
