from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from fractions import Fraction
from pathlib import Path
from typing import Any


def as_fraction(
    value: Any,
    *,
    field: str,
    note_id: str | None = None,
    part_id: str | None = None,
) -> Fraction:
    from .errors import score_invalid

    if isinstance(value, bool) or value is None:
        raise score_invalid(
            f"字段 {field} 缺失或不是有效数值",
            note_id=note_id,
            field=field,
            part_id=part_id,
        )
    try:
        if isinstance(value, Fraction):
            return value
        if isinstance(value, float):
            if not math.isfinite(value):
                raise ValueError("non-finite")
            return Fraction(str(value))
        if isinstance(value, int):
            return Fraction(value)
        if isinstance(value, str):
            return Fraction(value.strip())
    except (ValueError, ZeroDivisionError) as exc:
        raise score_invalid(
            f"字段 {field} 的数值格式无效: {value!r}",
            note_id=note_id,
            field=field,
            part_id=part_id,
        ) from exc
    raise score_invalid(
        f"字段 {field} 的类型无效: {type(value).__name__}",
        note_id=note_id,
        field=field,
        part_id=part_id,
    )


def fraction_text(value: Fraction | int) -> str:
    fraction = value if isinstance(value, Fraction) else Fraction(value)
    return f"{fraction.numerator}/{fraction.denominator}"


def rounded_float(value: Fraction | float, digits: int = 9) -> float:
    numeric = float(value)
    result = round(numeric, digits)
    return 0.0 if result == 0 else result


def clamp_int(value: int, low: int, high: int) -> int:
    return max(low, min(high, value))


def round_half_up(value: Fraction) -> int:
    if value >= 0:
        return (value.numerator * 2 + value.denominator) // (2 * value.denominator)
    positive = -value
    return -((positive.numerator * 2 + positive.denominator) // (2 * positive.denominator))


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8") + "\n"


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temporary_path = Path(temporary)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
        _fsync_dir(path.parent)
    finally:
        if temporary_path.exists():
            temporary_path.unlink()


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value) + b"\n")


def atomic_write_text(path: Path, text: str) -> None:
    atomic_write_bytes(path, text.encode("utf-8"))


def _fsync_dir(path: Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def read_json(path: Path) -> Any:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def jsonl_bytes(records: list[dict[str, Any]]) -> bytes:
    return b"".join(canonical_json_bytes(record) + b"\n" for record in records)

