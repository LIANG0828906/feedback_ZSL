"""Small deterministic helpers shared by all stages."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import decode_error, parameter_error


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


def sha256_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def sha256_bytes(*chunks: bytes) -> str:
    digest = hashlib.sha256()
    for chunk in chunks:
        digest.update(chunk)
    return digest.hexdigest()


def read_json(path: str | Path, field: str = "file") -> dict[str, Any]:
    path = Path(path)
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise decode_error(field, f"cannot decode JSON file {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise decode_error(field, "top-level JSON value must be an object")
    return value


def write_json_atomic(path: str | Path, value: Any) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.tmp")
    with temp.open("w", encoding="utf-8") as handle:
        json.dump(value, handle, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False)
        handle.write("\n")
        handle.flush()
        import os

        os.fsync(handle.fileno())
    temp.replace(path)
    _fsync_dir(path.parent)


def _fsync_dir(path: Path) -> None:
    try:
        descriptor = __import__("os").open(path, __import__("os").O_RDONLY)
    except OSError:
        return
    try:
        __import__("os").fsync(descriptor)
    except OSError:
        pass
    finally:
        __import__("os").close(descriptor)


def vec3(value: Any, field: str) -> np.ndarray:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise parameter_error(field, "expected an array of three finite numbers")
    result = np.asarray(value, dtype=np.float64)
    if result.shape != (3,) or not np.all(np.isfinite(result)):
        raise parameter_error(field, "expected an array of three finite numbers")
    return result


def finite_float(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise parameter_error(field, "expected a finite number")
    result = float(value)
    if not math.isfinite(result):
        raise parameter_error(field, "expected a finite number")
    return result


def normalize(vector: np.ndarray, field: str) -> np.ndarray:
    length = float(np.linalg.norm(vector))
    if not math.isfinite(length) or length <= 0.0:
        raise parameter_error(field, "direction must have non-zero finite length")
    return vector / length


def finite_positive(value: Any, field: str) -> float:
    result = finite_float(value, field)
    if result <= 0.0:
        raise parameter_error(field, "expected a number greater than zero")
    return result

