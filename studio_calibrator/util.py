from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any, Iterable, Optional

import numpy as np

from .errors import E_CONFIG_INVALID, BusinessError


def _json_ready(value: Any) -> Any:
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return [_json_ready(item) for item in value.tolist()]
    if isinstance(value, dict):
        return {str(key): _json_ready(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_ready(item) for item in value]
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("non-finite float in canonical JSON")
        return round(value, 12)
    return value


def canonical_json_bytes(value: Any) -> bytes:
    payload = json.dumps(
        _json_ready(value),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )
    return (payload + "\n").encode("utf-8")


def content_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def file_hash(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_bytes_atomic(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        dir_fd = os.open(str(path.parent), os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if temp_path.exists():
            temp_path.unlink()


def write_json_atomic(path: Path, value: Any) -> None:
    write_bytes_atomic(path, canonical_json_bytes(value))


def load_json(path: Path) -> Any:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def require_keys(payload: dict[str, Any], keys: Iterable[str], context: str) -> None:
    missing = sorted(key for key in keys if key not in payload)
    if missing:
        raise BusinessError(
            E_CONFIG_INVALID,
            f"{context} missing required field(s): {', '.join(missing)}",
            details={"missing_fields": missing, "context": context},
        )


def finite_number(value: Any, name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise BusinessError(
            E_CONFIG_INVALID,
            f"{name} must be a number",
            details={"field": name, "value": repr(value)},
        ) from exc
    if not math.isfinite(number):
        raise BusinessError(
            E_CONFIG_INVALID,
            f"{name} must be finite",
            details={"field": name},
        )
    return number


def wrap_deg(value: float) -> float:
    return float((value + 180.0) % 360.0 - 180.0)


def db_to_linear(value_db: float) -> float:
    return float(10.0 ** (value_db / 20.0))


def linear_to_db(value: float, floor_db: float = -240.0) -> float:
    return float(max(floor_db, 20.0 * math.log10(max(abs(value), 1e-12))))


def relative_label(base: Path, path: Path) -> str:
    try:
        return path.resolve().relative_to(base.resolve()).as_posix()
    except ValueError:
        return path.name


def optional_cancel(cancel_event: Optional[object]) -> None:
    if cancel_event is not None and getattr(cancel_event, "is_set", lambda: False)():
        from .errors import E_CANCELLED

        raise BusinessError(E_CANCELLED, "recompute cancelled")
