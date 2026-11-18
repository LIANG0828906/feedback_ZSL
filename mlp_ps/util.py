from __future__ import annotations

import hashlib
import json
import os
import sys
import time
from pathlib import Path
from typing import Any, BinaryIO, Optional


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def pretty_json_text(value: Any) -> str:
    return (
        json.dumps(
            value,
            sort_keys=True,
            indent=2,
            ensure_ascii=True,
            allow_nan=False,
        )
        + "\n"
    )


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path, chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def atomic_write_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + f".tmp-{os.getpid()}")
    with temporary.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + f".tmp-{os.getpid()}")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def utc_timestamp() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def monotonic_millis() -> float:
    return time.perf_counter() * 1000.0


class BandwidthLimitedWriter:
    """A small deterministic local-disk bandwidth limiter."""

    def __init__(self, handle: BinaryIO, bytes_per_second: float, chunk_size: int = 64 * 1024) -> None:
        if bytes_per_second <= 0:
            raise ValueError("bytes_per_second must be positive")
        self.handle = handle
        self.bytes_per_second = float(bytes_per_second)
        self.chunk_size = max(1024, min(chunk_size, int(bytes_per_second)))
        self.bytes_written = 0
        self.started = time.perf_counter()

    def write(self, data: bytes) -> None:
        view = memoryview(data)
        while view:
            chunk = view[: self.chunk_size]
            self.handle.write(chunk)
            self.bytes_written += len(chunk)
            view = view[len(chunk) :]
            target_elapsed = self.bytes_written / self.bytes_per_second
            actual_elapsed = time.perf_counter() - self.started
            delay = target_elapsed - actual_elapsed
            if delay > 0:
                time.sleep(delay)

    def close(self) -> None:
        self.handle.flush()
        os.fsync(self.handle.fileno())
        self.handle.close()

    @property
    def elapsed_seconds(self) -> float:
        return time.perf_counter() - self.started

    @property
    def effective_bytes_per_second(self) -> float:
        elapsed = self.elapsed_seconds
        return self.bytes_written / elapsed if elapsed > 0 else 0.0


def process_memory_mb() -> Optional[float]:
    try:
        import resource

        usage = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        if sys.platform == "darwin":
            return usage / (1024 * 1024)
        return usage / 1024
    except (ImportError, OSError):
        return None
