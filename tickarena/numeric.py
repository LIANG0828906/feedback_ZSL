"""Integer-only numeric helpers and canonical fingerprints."""

from __future__ import annotations

import hashlib
import json
from typing import Any

from .codes import E_NUM, TickArenaError

BPS = 10_000


class NumericError(TickArenaError):
    def __init__(self, message: str) -> None:
        super().__init__(E_NUM, message)


def require_int(
    value: Any,
    field: str,
    *,
    minimum: int | None = None,
    maximum: int | None = None,
) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise NumericError(f"{field} must be an integer, got {type(value).__name__}")
    if minimum is not None and value < minimum:
        raise NumericError(f"{field} must be >= {minimum}, got {value}")
    if maximum is not None and value > maximum:
        raise NumericError(f"{field} must be <= {maximum}, got {value}")
    return value


def require_str(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        raise TickArenaError("E_ARG", f"{field} must be a non-empty string")
    return value


def assert_no_float(value: Any, path: str = "$") -> None:
    """Reject every float, including NaN/inf, before any rule evaluation."""

    if isinstance(value, float):
        raise NumericError(f"{path} contains forbidden floating-point value {value!r}")
    if isinstance(value, dict):
        for key, child in value.items():
            assert_no_float(child, f"{path}.{key}")
    elif isinstance(value, (list, tuple)):
        for index, child in enumerate(value):
            assert_no_float(child, f"{path}[{index}]")


def apply_bps(value: int, bps: int) -> int:
    """Apply basis points using deterministic round-half-up integer math."""

    require_int(value, "value")
    require_int(bps, "bps")
    if value >= 0:
        return (value * bps + BPS // 2) // BPS
    return -((-value * bps + BPS // 2) // BPS)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def canonical_bytes(value: Any) -> bytes:
    return canonical_json(value).encode("utf-8")


def fingerprint(value: Any, domain: str) -> str:
    hasher = hashlib.sha256()
    hasher.update(domain.encode("utf-8"))
    hasher.update(b"\x00")
    hasher.update(canonical_bytes(value))
    return hasher.hexdigest()


def state_size_estimate(unit_count: int, pending_event_bytes: int = 0) -> int:
    return 2_048 + max(0, unit_count) * 768 + max(0, pending_event_bytes)

