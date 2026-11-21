"""Canonical serialization, deterministic fixed-point conversion, and file helpers."""

from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from pathlib import Path
from typing import Any

from .errors import BusinessError


def _normalize_json(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _normalize_json(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_normalize_json(item) for item in value]
    return value


def canonical_json(value: Any) -> str:
    return json.dumps(
        _normalize_json(value),
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def canonical_bytes(value: Any) -> bytes:
    return canonical_json(value).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_obj(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def load_json(path: str | Path) -> Any:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
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
            return
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(value) + b"\n")


def _invalid_decimal(value: Any, field: str) -> BusinessError:
    return BusinessError(
        "E_RECORD_INVALID",
        f"{field} must be a finite number",
        field=field,
    )


def as_decimal(value: Any, field: str) -> Decimal:
    if isinstance(value, bool) or value is None:
        raise _invalid_decimal(value, field)
    try:
        decimal = Decimal(str(value))
    except (InvalidOperation, ValueError, TypeError):
        raise _invalid_decimal(value, field) from None
    if not decimal.is_finite():
        raise _invalid_decimal(value, field)
    return decimal


def to_units(value: Any, fractional_digits: int, field: str = "value") -> int:
    decimal = as_decimal(value, field)
    scale = 10 ** fractional_digits
    return int((decimal * scale).quantize(Decimal("1"), rounding=ROUND_HALF_UP))


def from_units(units: int, fractional_digits: int) -> str:
    scale = 10 ** fractional_digits
    if fractional_digits == 0:
        return str(units // scale)
    sign = "-" if units < 0 else ""
    absolute = abs(units)
    whole, part = divmod(absolute, scale)
    return f"{sign}{whole}.{part:0{fractional_digits}d}"


def nearest_integer_ratio(numerator: int, denominator: int) -> int:
    if denominator <= 0:
        raise ValueError("denominator must be positive")
    if numerator >= 0:
        return (numerator + denominator // 2) // denominator
    return -((-numerator + denominator // 2) // denominator)


def cosine_units(angle_degrees_units: int, fractional_digits: int) -> int:
    angle = angle_degrees_units / (10 ** fractional_digits)
    cosine = math.cos(math.radians(angle))
    if cosine <= 0:
        return 0
    return int(
        (Decimal(str(cosine)) * (10 ** fractional_digits)).quantize(
            Decimal("1"), rounding=ROUND_HALF_UP
        )
    )


def first_difference(
    baseline: Any,
    replay: Any,
    *,
    path: str = "$",
) -> tuple[str, Any, Any] | None:
    if type(baseline) is not type(replay) and not (
        isinstance(baseline, (int, float)) and isinstance(replay, (int, float))
    ):
        return path, baseline, replay
    if isinstance(baseline, dict):
        for key in sorted(set(baseline) | set(replay)):
            if key not in baseline:
                return f"{path}.{key}", "<missing>", replay[key]
            if key not in replay:
                return f"{path}.{key}", baseline[key], "<missing>"
            difference = first_difference(
                baseline[key], replay[key], path=f"{path}.{key}"
            )
            if difference is not None:
                return difference
        return None
    if isinstance(baseline, list):
        for index in range(max(len(baseline), len(replay))):
            if index >= len(baseline):
                return f"{path}[{index}]", "<missing>", replay[index]
            if index >= len(replay):
                return f"{path}[{index}]", baseline[index], "<missing>"
            difference = first_difference(
                baseline[index], replay[index], path=f"{path}[{index}]"
            )
            if difference is not None:
                return difference
        return None
    if baseline != replay:
        return path, baseline, replay
    return None


def tree_digest(root: str | Path) -> str:
    root_path = Path(root)
    records: list[dict[str, str]] = []
    for path in sorted(item for item in root_path.rglob("*") if item.is_file()):
        relative = path.relative_to(root_path).as_posix()
        records.append({"path": relative, "sha256": sha256_bytes(path.read_bytes())})
    return sha256_obj(records)
