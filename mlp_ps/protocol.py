from __future__ import annotations

import asyncio
import json
import socket
import struct
from dataclasses import dataclass
from typing import Any, Dict, Mapping, Optional, Tuple

from .errors import BusinessError, E_PROTOCOL
from .util import canonical_json_bytes


HEADER = struct.Struct("!II")
MAX_HEADER_BYTES = 1024 * 1024


@dataclass(frozen=True)
class Frame:
    header: Dict[str, Any]
    payload: bytes

    @property
    def message_type(self) -> str:
        return str(self.header.get("type", ""))


def encode_frame(header: Mapping[str, Any], payload: bytes = b"") -> bytes:
    header_bytes = canonical_json_bytes(dict(header))
    if len(header_bytes) > MAX_HEADER_BYTES:
        raise ValueError("protocol header is too large")
    return HEADER.pack(len(header_bytes), len(payload)) + header_bytes + payload


def decode_header(data: bytes) -> Dict[str, Any]:
    try:
        value = json.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise BusinessError(E_PROTOCOL, "invalid JSON protocol header") from exc
    if not isinstance(value, dict) or not isinstance(value.get("type"), str):
        raise BusinessError(E_PROTOCOL, "protocol header must be an object with a string type")
    return value


async def read_frame(
    reader: asyncio.StreamReader,
    max_payload_bytes: int,
    timeout_s: Optional[float] = None,
) -> Frame:
    async def _read() -> Frame:
        prefix = await reader.readexactly(HEADER.size)
        header_length, payload_length = HEADER.unpack(prefix)
        if header_length <= 0 or header_length > MAX_HEADER_BYTES:
            raise BusinessError(E_PROTOCOL, "invalid protocol header length")
        if payload_length < 0 or payload_length > max_payload_bytes:
            raise BusinessError(
                E_PROTOCOL,
                "protocol payload exceeds configured maximum",
                {"payload_bytes": payload_length, "max": max_payload_bytes},
            )
        header_data = await reader.readexactly(header_length)
        payload = await reader.readexactly(payload_length) if payload_length else b""
        return Frame(header=decode_header(header_data), payload=payload)

    if timeout_s is None:
        return await _read()
    return await asyncio.wait_for(_read(), timeout=timeout_s)


async def write_frame(
    writer: asyncio.StreamWriter,
    header: Mapping[str, Any],
    payload: bytes = b"",
    timeout_s: Optional[float] = None,
) -> None:
    async def _write() -> None:
        writer.write(encode_frame(header, payload))
        await writer.drain()

    if timeout_s is None:
        await _write()
    else:
        await asyncio.wait_for(_write(), timeout=timeout_s)


def _recv_exact(sock: socket.socket, size: int) -> bytes:
    chunks = []
    remaining = size
    while remaining:
        chunk = sock.recv(remaining)
        if not chunk:
            raise ConnectionError("peer closed the connection")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def sync_send_frame(
    sock: socket.socket,
    header: Mapping[str, Any],
    payload: bytes = b"",
) -> None:
    sock.sendall(encode_frame(header, payload))


def sync_read_frame(sock: socket.socket, max_payload_bytes: int) -> Frame:
    prefix = _recv_exact(sock, HEADER.size)
    header_length, payload_length = HEADER.unpack(prefix)
    if header_length <= 0 or header_length > MAX_HEADER_BYTES:
        raise BusinessError(E_PROTOCOL, "invalid protocol header length")
    if payload_length < 0 or payload_length > max_payload_bytes:
        raise BusinessError(E_PROTOCOL, "protocol payload exceeds configured maximum")
    header_data = _recv_exact(sock, header_length)
    payload = _recv_exact(sock, payload_length) if payload_length else b""
    return Frame(header=decode_header(header_data), payload=payload)
