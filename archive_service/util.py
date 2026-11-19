from __future__ import annotations

import hashlib
import os
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import BinaryIO


UTC = timezone.utc
ISO_FORMAT = "%Y-%m-%dT%H:%M:%S.%fZ"


def utc_now() -> datetime:
    return datetime.now(UTC)


def format_time(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    return value.astimezone(UTC).strftime(ISO_FORMAT)


def parse_time(value: str | datetime | None, *, default: datetime | None = None) -> datetime:
    if value is None:
        if default is not None:
            return default
        raise ValueError("timestamp is required")
    if isinstance(value, datetime):
        result = value
    else:
        text = str(value).strip()
        if not text:
            raise ValueError("timestamp must not be empty")
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        result = datetime.fromisoformat(text)
    if result.tzinfo is None:
        result = result.replace(tzinfo=UTC)
    return result.astimezone(UTC)


def add_days(value: datetime, days: int) -> datetime:
    return value + timedelta(days=days)


def hash_file(path: os.PathLike[str] | str, *, chunk_size: int = 1024 * 1024) -> tuple[int, str]:
    digest = hashlib.sha256()
    length = 0
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
            length += len(chunk)
    return length, digest.hexdigest()


def copy_file_atomic(
    source: str | os.PathLike[str],
    destination: str | os.PathLike[str],
    *,
    chunk_size: int = 1024 * 1024,
    fsync: bool = True,
) -> tuple[int, str]:
    destination_path = Path(destination)
    destination_path.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination_path.with_name(f".tmp-{os.getpid()}-{uuid.uuid4().hex}")
    digest = hashlib.sha256()
    length = 0
    try:
        with open(source, "rb") as source_handle, open(temporary, "wb") as destination_handle:
            while True:
                chunk = source_handle.read(chunk_size)
                if not chunk:
                    break
                destination_handle.write(chunk)
                digest.update(chunk)
                length += len(chunk)
            destination_handle.flush()
            if fsync:
                os.fsync(destination_handle.fileno())
        os.replace(temporary, destination_path)
    except BaseException:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise
    return length, digest.hexdigest()


def spool_stream(
    source: BinaryIO,
    destination: str | os.PathLike[str],
    length: int,
    *,
    chunk_size: int = 1024 * 1024,
) -> tuple[int, str]:
    digest = hashlib.sha256()
    copied = 0
    with open(destination, "wb") as handle:
        while copied < length:
            chunk = source.read(min(chunk_size, length - copied))
            if not chunk:
                raise ValueError("request body ended before Content-Length")
            handle.write(chunk)
            digest.update(chunk)
            copied += len(chunk)
        handle.flush()
        os.fsync(handle.fileno())
    return copied, digest.hexdigest()


def remove_quietly(path: str | os.PathLike[str]) -> None:
    try:
        Path(path).unlink()
    except FileNotFoundError:
        pass
