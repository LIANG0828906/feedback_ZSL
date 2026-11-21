"""Byte-stable canonical serialisation and hashing helpers."""

from __future__ import annotations

import hashlib
import json
import math
import struct
from typing import Any, Iterable, Sequence

import numpy as np


def canonical_json_bytes(value: Any) -> bytes:
    """Serialise without depending on dict insertion or locale.

    Nan/Inf are rejected because they are not reproducible content values.
    Floats use Python's shortest-roundtrip representation, which is stable for
    a given CPython version and is moved to ``float.hex`` in hash payloads.
    """

    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def float_hex_array(array: np.ndarray) -> list[str]:
    """Encode float64 values as hex strings, avoiding platform repr drift."""

    values = np.asarray(array, dtype=np.float64).ravel(order="C")
    result: list[str] = []
    for value in values:
        if not math.isfinite(float(value)):
            raise ValueError("non-finite value in hash payload")
        result.append(float(value).hex())
    return result


def pose_hash(joint_transforms: np.ndarray) -> str:
    array = np.asarray(joint_transforms, dtype=np.float64)
    payload = {
        "dtype": "float64",
        "shape": list(array.shape),
        "values_hex": float_hex_array(array),
    }
    return sha256_json(payload)


def frame_content_hash(
    *,
    frame: int,
    transforms: np.ndarray,
    residuals: Sequence[dict[str, Any]],
    parameter_version: str,
) -> str:
    return sha256_json(
        {
            "frame": frame,
            "parameter_version": parameter_version,
            "pose_hash": pose_hash(transforms),
            "residuals": list(residuals),
        }
    )


def ordered_float_sum(values: Iterable[float]) -> float:
    """Reduce in caller-provided order (never NumPy's pairwise reduction)."""

    total = 0.0
    for value in values:
        total += float(value)
    return total


def pack_float64(value: float) -> bytes:
    return struct.pack("<d", float(value))
