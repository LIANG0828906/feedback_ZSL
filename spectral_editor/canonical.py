from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any, Mapping


FLOAT_DIGITS = 12


def normalize_value(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {str(key): normalize_value(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [normalize_value(item) for item in value]
    if isinstance(value, bool) or value is None or isinstance(value, str):
        return value
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("non-finite float cannot be canonicalized")
        rounded = round(value, FLOAT_DIGITS)
        return 0.0 if rounded == 0 else rounded
    raise TypeError(f"unsupported canonical value: {type(value)!r}")


def canonical_json(value: Any, *, pretty: bool = False) -> str:
    normalized = normalize_value(value)
    if pretty:
        return json.dumps(
            normalized,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            indent=2,
        ) + "\n"
    return json.dumps(
        normalized,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def canonical_bytes(value: Any, *, pretty: bool = False) -> bytes:
    return canonical_json(value, pretty=pretty).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_obj(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def atomic_write_bytes(path: str | Path, data: bytes) -> str:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=str(target.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
        try:
            dir_fd = os.open(target.parent, os.O_RDONLY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        except OSError:
            # Windows does not generally permit fsync on a directory handle.
            pass
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise
    return sha256_bytes(data)


def write_json_atomic(path: str | Path, value: Any, *, pretty: bool = True) -> str:
    return atomic_write_bytes(path, canonical_bytes(value, pretty=pretty))


def read_json(path: str | Path) -> Any:
    with open(path, "rb") as handle:
        return json.loads(handle.read().decode("utf-8"))
