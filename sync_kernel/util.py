from __future__ import annotations

import hashlib
import json
import os
import select
import socket
import tempfile
import time
from pathlib import Path
from typing import Any

from .errors import E_DECODE, E_RESOURCE, KernelError


MAX_FRAME_BYTES = 1_048_576


def canonical_json_text(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
        sort_keys=True,
    )


def canonical_json_bytes(value: Any) -> bytes:
    return canonical_json_text(value).encode("utf-8")


def sha256_hex(data: bytes | str) -> str:
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


def digest_json(value: Any) -> str:
    return sha256_hex(canonical_json_bytes(value))


def parse_json_payload(payload: bytes | str | dict[str, Any]) -> dict[str, Any]:
    if isinstance(payload, dict):
        return payload
    try:
        if isinstance(payload, bytes):
            if len(payload) > MAX_FRAME_BYTES:
                raise KernelError(
                    E_RESOURCE,
                    f"frame exceeds {MAX_FRAME_BYTES} byte buffer limit",
                    path="$",
                    details={"size": len(payload), "limit": MAX_FRAME_BYTES},
                )
            text = payload.decode("utf-8")
        else:
            text = payload
        value = json.loads(text)
    except KernelError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise KernelError(
            E_DECODE,
            f"invalid JSON payload: {exc}",
            path="$",
        ) from exc
    if not isinstance(value, dict):
        raise KernelError(
            E_DECODE,
            "sync frame root must be a JSON object",
            path="$",
        )
    return value


def is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=destination.parent,
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        try:
            directory_fd = os.open(destination.parent, os.O_RDONLY)
        except OSError:
            # Directory fsync is not available on Windows. File contents were
            # already fsynced and atomically replaced, so this is a safe boundary.
            directory_fd = None
        if directory_fd is not None:
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def wait_until(predicate: Any, timeout: float = 5.0, interval: float = 0.01) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return bool(predicate())


def recv_line(
    sock: socket.socket,
    buffer: bytearray,
    *,
    timeout: float,
    max_bytes: int = MAX_FRAME_BYTES,
) -> tuple[bytes | None, str]:
    """Read one newline-delimited message without makefile timeout pitfalls."""
    while True:
        newline = buffer.find(b"\n")
        if newline >= 0:
            line = bytes(buffer[:newline])
            del buffer[: newline + 1]
            if len(line) > max_bytes:
                return None, "overflow"
            return line, "ok"
        if len(buffer) > max_bytes:
            return None, "overflow"
        readable, _, _ = select.select([sock], [], [], timeout)
        if not readable:
            return None, "timeout"
        try:
            chunk = sock.recv(min(65_536, max_bytes - len(buffer) + 1))
        except (TimeoutError, socket.timeout):
            return None, "timeout"
        if not chunk:
            return None, "closed"
        buffer.extend(chunk)

