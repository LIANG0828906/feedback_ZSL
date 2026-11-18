from __future__ import annotations

import asyncio
import os
import socket
import struct
from typing import Mapping
from urllib.parse import urlsplit


MAX_HEAD_BYTES = 64 * 1024


class HttpParseError(Exception):
    pass


def parse_headers(lines: list[bytes]) -> dict[str, str]:
    headers: dict[str, str] = {}
    for raw in lines:
        if b":" not in raw:
            continue
        key, value = raw.split(b":", 1)
        key_text = key.decode("latin-1").strip().lower()
        value_text = value.decode("latin-1").strip()
        if key_text in headers:
            headers[key_text] = f"{headers[key_text]}, {value_text}"
        else:
            headers[key_text] = value_text
    return headers


async def read_head(reader: asyncio.StreamReader, timeout_ms: int | None = None) -> bytes:
    coroutine = reader.readuntil(b"\r\n\r\n")
    if timeout_ms is None:
        return await coroutine
    return await asyncio.wait_for(coroutine, timeout=timeout_ms / 1000.0)


async def read_request_head(
    reader: asyncio.StreamReader, timeout_ms: int | None = None
) -> tuple[str, list[str], dict[str, str]] | None:
    try:
        raw = await read_head(reader, timeout_ms)
    except asyncio.IncompleteReadError as exc:
        if not exc.partial:
            return None
        raise HttpParseError("incomplete request head") from exc
    if len(raw) > MAX_HEAD_BYTES:
        raise HttpParseError("request head too large")
    lines = raw[:-4].split(b"\r\n")
    if not lines:
        raise HttpParseError("empty request")
    request_line = lines[0].decode("latin-1")
    parts = request_line.split(" ")
    if len(parts) != 3:
        raise HttpParseError("malformed request line")
    method, target, _version = parts
    headers = parse_headers(lines[1:])
    return method, target, headers


async def read_response_head(
    reader: asyncio.StreamReader, timeout_ms: int | None = None
) -> tuple[int, str, dict[str, str]]:
    try:
        raw = await read_head(reader, timeout_ms)
    except asyncio.IncompleteReadError as exc:
        if not exc.partial:
            raise ConnectionResetError("upstream closed before response") from exc
        raise HttpParseError("incomplete response head") from exc
    if len(raw) > MAX_HEAD_BYTES:
        raise HttpParseError("response head too large")
    lines = raw[:-4].split(b"\r\n")
    status_line = lines[0].decode("latin-1")
    parts = status_line.split(" ", 2)
    if len(parts) < 2 or not parts[1].isdigit():
        raise HttpParseError("malformed status line")
    code = int(parts[1])
    reason = parts[2] if len(parts) > 2 else ""
    headers = parse_headers(lines[1:])
    return code, reason, headers


def normalize_target(target: str, default_host: str, default_port: int) -> str:
    if target.startswith("/"):
        return target
    parsed = urlsplit(target)
    if parsed.scheme not in ("http", "https"):
        raise HttpParseError("forward proxy expects an absolute http URI")
    path = parsed.path or "/"
    if parsed.query:
        path += "?" + parsed.query
    return path


def content_length(headers: Mapping[str, str]) -> int:
    raw = headers.get("content-length")
    if raw is None:
        return 0
    try:
        value = int(raw)
    except ValueError as exc:
        raise HttpParseError("invalid content-length") from exc
    if value < 0:
        raise HttpParseError("negative content-length")
    return value


async def read_exact(
    reader: asyncio.StreamReader,
    count: int,
    chunk_size: int = 65536,
    timeout_ms: int | None = None,
) -> bytes:
    chunks: list[bytes] = []
    remaining = count
    while remaining:
        piece = await reader.read(min(chunk_size, remaining))
        if not piece:
            raise ConnectionResetError("peer closed while reading body")
        chunks.append(piece)
        remaining -= len(piece)
    return b"".join(chunks)


async def drain_exact(reader: asyncio.StreamReader, count: int, chunk_size: int = 65536) -> int:
    remaining = count
    while remaining:
        piece = await reader.read(min(chunk_size, remaining))
        if not piece:
            raise ConnectionResetError("peer closed while reading body")
        remaining -= len(piece)
    return count


def encode_response(
    status: int,
    reason: str,
    headers: Mapping[str, str],
    body: bytes = b"",
) -> bytes:
    lines = [f"HTTP/1.1 {status} {reason}"]
    merged = {key.lower(): value for key, value in headers.items()}
    merged["content-length"] = str(len(body))
    for key, value in merged.items():
        lines.append(f"{key}: {value}")
    return ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1") + body


def encode_forward_request(
    method: str,
    path: str,
    headers: Mapping[str, str],
    body_size: int,
) -> bytes:
    lines = [f"{method} {path} HTTP/1.1"]
    for key, value in headers.items():
        lines.append(f"{key}: {value}")
    if not any(key.lower() == "content-length" for key in headers) and body_size:
        lines.append(f"Content-Length: {body_size}")
    return ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1")


def force_reset(writer: asyncio.StreamWriter | None) -> None:
    if writer is None:
        return
    try:
        sock = writer.get_extra_info("socket")
        if sock is not None:
            linger = struct.pack("HH", 1, 0) if os.name == "nt" else struct.pack("ii", 1, 0)
            sock.setsockopt(
                socket.SOL_SOCKET,
                socket.SO_LINGER,
                linger,
            )
    except (OSError, AttributeError):
        pass
    try:
        writer.transport.abort()
    except Exception:
        writer.close()
