from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, is_dataclass
from typing import Any

import numpy as np


def _normalize_float(value: float) -> str:
    if not math.isfinite(value):
        raise ValueError("non-finite values cannot be hashed")
    if value == 0.0:
        return "0x0.0p+0"
    return float(value).hex()


def _canonical(value: Any) -> Any:
    if isinstance(value, np.ndarray):
        normalized = np.ascontiguousarray(value)
        return {
            "__ndarray__": {
                "dtype": str(normalized.dtype),
                "shape": list(normalized.shape),
                "data": [_canonical(v) for v in normalized.ravel().tolist()],
            }
        }
    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, float):
        return {"__float__": _normalize_float(value)}
    if isinstance(value, (str, int, bool)) or value is None:
        return value
    if is_dataclass(value):
        return _canonical(asdict(value))
    if isinstance(value, dict):
        return {str(k): _canonical(v) for k, v in sorted(value.items(), key=lambda kv: str(kv[0]))}
    if isinstance(value, (list, tuple)):
        return [_canonical(v) for v in value]
    raise TypeError(f"unsupported value for stable hashing: {type(value)!r}")


def stable_json(value: Any) -> str:
    return json.dumps(
        _canonical(value), sort_keys=True, separators=(",", ":"), ensure_ascii=True
    )


def stable_hash(value: Any) -> str:
    return hashlib.sha256(stable_json(value).encode("ascii")).hexdigest()


def file_sha256(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def encode_payload(value: Any) -> Any:
    if isinstance(value, np.ndarray):
        return {
            "__ndarray__": {
                "dtype": str(value.dtype),
                "shape": list(value.shape),
                "data": value.ravel().tolist(),
            }
        }
    if isinstance(value, np.generic):
        return value.item()
    if is_dataclass(value):
        return encode_payload(asdict(value))
    if isinstance(value, dict):
        return {str(k): encode_payload(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [encode_payload(v) for v in value]
    if isinstance(value, float):
        if not math.isfinite(value):
            return {"__float__": repr(value)}
        return value
    return value


def decode_payload(value: Any) -> Any:
    if isinstance(value, dict) and set(value) == {"__ndarray__"}:
        spec = value["__ndarray__"]
        return np.asarray(spec["data"], dtype=spec["dtype"]).reshape(spec["shape"])
    if isinstance(value, dict) and set(value) == {"__float__"}:
        return float(value["__float__"])
    if isinstance(value, dict):
        return {k: decode_payload(v) for k, v in value.items()}
    if isinstance(value, list):
        return [decode_payload(v) for v in value]
    return value


def short_hash(value: str, length: int = 16) -> str:
    return value[:length]

