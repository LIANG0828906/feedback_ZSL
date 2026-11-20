from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from .errors import E_IO, HapticError

GENESIS_HASH = "0" * 64


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def finite_number(
    value: Any,
    field: str,
    *,
    segment_index: int | None = None,
) -> float:
    from .errors import E_WAVE_INVALID

    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise HapticError(
            E_WAVE_INVALID,
            f"{field} must be a number",
            segment_index=segment_index,
            details={"field": field},
        )
    number = float(value)
    if not math.isfinite(number):
        raise HapticError(
            E_WAVE_INVALID,
            f"{field} must be finite",
            segment_index=segment_index,
            details={"field": field},
        )
    return number


def merge_ranges(
    ranges: Iterable[tuple[int, int]],
    *,
    limit: int | None = None,
) -> tuple[tuple[int, int], ...]:
    normalized: list[tuple[int, int]] = []
    for start, end in ranges:
        start = max(0, int(start))
        end = int(end)
        if limit is not None:
            end = min(limit, end)
        if end > start:
            normalized.append((start, end))
    normalized.sort()
    if not normalized:
        return ()
    merged: list[list[int]] = [[normalized[0][0], normalized[0][1]]]
    for start, end in normalized[1:]:
        if start <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])
    return tuple((start, end) for start, end in merged)


def complement_ranges(
    ranges: Iterable[tuple[int, int]],
    total_samples: int,
) -> tuple[tuple[int, int], ...]:
    if total_samples <= 0:
        return ()
    merged = merge_ranges(ranges, limit=total_samples)
    result: list[tuple[int, int]] = []
    cursor = 0
    for start, end in merged:
        if cursor < start:
            result.append((cursor, start))
        cursor = max(cursor, end)
    if cursor < total_samples:
        result.append((cursor, total_samples))
    return tuple(result)


def range_sample_count(ranges: Iterable[tuple[int, int]]) -> int:
    return sum(max(0, end - start) for start, end in ranges)


def expand_ranges(
    ranges: Iterable[tuple[int, int]],
    *,
    padding: int,
    total_samples: int,
) -> tuple[tuple[int, int], ...]:
    return merge_ranges(
        (
            (max(0, start - padding), min(total_samples, end + padding))
            for start, end in ranges
        ),
        limit=total_samples,
    )


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    import os
    import tempfile

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(
        prefix=f".{target.name}.",
        suffix=".tmp",
        dir=str(target.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_name, target)
        try:
            dir_fd = os.open(target.parent, os.O_RDONLY)
        except OSError:
            dir_fd = -1
        if dir_fd >= 0:
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
    except Exception as exc:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        if isinstance(exc, HapticError):
            raise
        raise HapticError(
            E_IO,
            f"atomic write failed for {target}: {exc}",
        ) from exc
