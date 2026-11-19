"""Minimal HTTP/1.1 server framework and client used by backends and probes."""
from __future__ import annotations

import asyncio
from dataclasses import dataclass, field

H2_PREFACE = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"


@dataclass
class H1Request:
    method: str
    target: str
    version: str
    headers: list[tuple[str, str]]

    def header(self, name: str) -> str | None:
        lname = name.lower()
        for k, v in self.headers:
            if k.lower() == lname:
                return v
        return None


@dataclass
class RawResponse:
    status: int
    headers: list[tuple[str, str]]
    body: bytes
    http_version: str

    def header(self, name: str) -> str | None:
        lname = name.lower()
        for k, v in self.headers:
            if k.lower() == lname:
                return v
        return None


class RequestError(Exception):
    pass


async def read_request(reader: asyncio.StreamReader) -> H1Request | str | None:
    """Read one request. Returns H1Request, "preface" for h2 prior-knowledge,
    or None on clean EOF."""
    try:
        head = await reader.readuntil(b"\r\n\r\n")
    except (asyncio.IncompleteReadError, asyncio.LimitOverrunError):
        return None
    except ConnectionResetError:
        return None
    if head.startswith(b"PRI * HTTP/2.0"):
        return "preface"
    try:
        text = head.decode("iso-8859-1")
        lines = text.split("\r\n")
        method, target, version = lines[0].split(" ", 2)
        headers = []
        for line in lines[1:]:
            if not line:
                continue
            name, _, value = line.partition(":")
            headers.append((name.strip(), value.strip()))
        return H1Request(method=method, target=target, version=version,
                         headers=headers)
    except ValueError as exc:
        raise RequestError(str(exc)) from exc


REASONS = {
    101: "Switching Protocols", 200: "OK", 304: "Not Modified",
    400: "Bad Request", 404: "Not Found", 405: "Method Not Allowed",
    426: "Upgrade Required", 500: "Internal Server Error",
    502: "Bad Gateway", 505: "HTTP Version Not Supported",
}


async def write_response(writer: asyncio.StreamWriter, method: str,
                         status: int, headers: list[tuple[str, str]],
                         body: bytes = b"", chunked: bool = False,
                         chunk_sizes: tuple[int, ...] = (16384, 8192),
                         trickle: float = 0.0) -> None:
    head = [f"HTTP/1.1 {status} {REASONS.get(status, 'Status')}"]
    for k, v in headers:
        head.append(f"{k}: {v}")
    is_head = method.upper() == "HEAD"
    no_body_status = status in (101, 204, 304)
    if no_body_status:
        pass
    elif chunked and not is_head:
        head.append("Transfer-Encoding: chunked")
    else:
        head.append(f"Content-Length: {len(body)}")
    head.append("Connection: keep-alive")
    writer.write(("\r\n".join(head) + "\r\n\r\n").encode("iso-8859-1"))
    if is_head:
        await writer.drain()
        return
    if chunked:
        if trickle > 0:
            # Give the downstream cache a chance to start streaming so the
            # chunked framing is observable deterministically.
            await asyncio.sleep(trickle)
        offset = 0
        idx = 0
        while offset < len(body):
            size = chunk_sizes[idx % len(chunk_sizes)]
            piece = body[offset:offset + size]
            writer.write(f"{len(piece):x}\r\n".encode() + piece + b"\r\n")
            offset += len(piece)
            idx += 1
        writer.write(b"0\r\n\r\n")
    else:
        writer.write(body)
    await writer.drain()


async def read_response(reader: asyncio.StreamReader, method: str) -> RawResponse:
    head = await reader.readuntil(b"\r\n\r\n")
    text = head.decode("iso-8859-1")
    lines = text.split("\r\n")
    parts = lines[0].split(" ", 2)
    if len(parts) < 2 or not parts[1].isdigit():
        raise RequestError(f"bad status line: {lines[0]!r}")
    version = parts[0]
    status = int(parts[1])
    headers = []
    for line in lines[1:]:
        if not line:
            continue
        name, _, value = line.partition(":")
        headers.append((name.strip(), value.strip()))
    resp = RawResponse(status=status, headers=headers, body=b"",
                       http_version=version)
    if method.upper() == "HEAD" or status in (101, 204, 304):
        return resp
    te = (resp.header("transfer-encoding") or "").lower()
    if "chunked" in te:
        chunks = bytearray()
        while True:
            size_line = await reader.readuntil(b"\r\n")
            size = int(size_line.strip().split(b";")[0], 16)
            if size == 0:
                await reader.readuntil(b"\r\n")
                break
            chunks += await reader.readexactly(size)
            await reader.readexactly(2)
        resp.body = bytes(chunks)
        return resp
    cl = resp.header("content-length")
    if cl is not None:
        resp.body = await reader.readexactly(int(cl))
        return resp
    resp.body = await reader.read()
    return resp


async def h1_fetch(host: str, port: int, method: str, target: str,
                   headers: list[tuple[str, str]] | None = None,
                   timeout: float = 15.0) -> RawResponse:
    """One-shot HTTP/1.1 request on a fresh connection."""
    async def _go() -> RawResponse:
        reader, writer = await asyncio.open_connection(host, port)
        try:
            hdrs = list(headers or [])
            lines = [f"{method} {target} HTTP/1.1",
                     f"Host: {host}:{port}"]
            lines += [f"{k}: {v}" for k, v in hdrs]
            lines.append("Connection: close")
            writer.write(("\r\n".join(lines) + "\r\n\r\n").encode("iso-8859-1"))
            await writer.drain()
            return await read_response(reader, method)
        finally:
            writer.close()
    return await asyncio.wait_for(_go(), timeout)


class H1Server:
    """asyncio HTTP/1.1 server with keep-alive.

    handler(request, reader, writer) may:
      - return None after writing a full response via write_response()
        (connection stays open for keep-alive),
      - return "close" to close after its response,
      - return "hijack" if it took over the raw connection entirely.
    preface_handler(reader, writer) handles h2 prior-knowledge prefaces.
    """

    def __init__(self, handler, preface_handler=None):
        self.handler = handler
        self.preface_handler = preface_handler
        self.server: asyncio.Server | None = None

    async def _conn(self, reader: asyncio.StreamReader,
                    writer: asyncio.StreamWriter) -> None:
        try:
            while True:
                try:
                    req = await read_request(reader)
                except RequestError:
                    await write_response(writer, "GET", 400, [], b"bad request")
                    return
                if req is None:
                    return
                if req == "preface":
                    if self.preface_handler is not None:
                        await self.preface_handler(reader, writer)
                    return
                result = await self.handler(req, reader, writer)
                if result in ("close", "hijack"):
                    return
        except (ConnectionResetError, BrokenPipeError, asyncio.IncompleteReadError):
            return
        finally:
            try:
                writer.close()
            except Exception:
                pass

    async def start(self, host: str, port: int) -> None:
        self.server = await asyncio.start_server(self._conn, host, port)

    async def stop(self) -> None:
        if self.server is not None:
            self.server.close()
            await self.server.wait_closed()
