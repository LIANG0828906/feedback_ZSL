"""Canonical serialisation and hashing helpers."""

from __future__ import annotations

import hashlib
import json
from typing import Any

import numpy as np


def canonical_json_bytes(value: Any) -> bytes:
    return (
        json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_array(array: np.ndarray) -> str:
    arr = np.asarray(array)
    digest = hashlib.sha256()
    digest.update(str(arr.dtype.str).encode("ascii"))
    digest.update(b":")
    digest.update(",".join(str(int(item)) for item in arr.shape).encode("ascii"))
    digest.update(b":")
    if arr.dtype == np.bool_:
        payload = np.ascontiguousarray(arr, dtype=np.uint8).tobytes(order="C")
    elif np.issubdtype(arr.dtype, np.floating):
        payload = np.ascontiguousarray(arr, dtype="<f8").tobytes(order="C")
    elif np.issubdtype(arr.dtype, np.unsignedinteger):
        payload = np.ascontiguousarray(arr, dtype="<u8").tobytes(order="C")
    elif np.issubdtype(arr.dtype, np.signedinteger):
        payload = np.ascontiguousarray(arr, dtype="<i8").tobytes(order="C")
    else:
        payload = arr.tobytes(order="C")
    digest.update(payload)
    return digest.hexdigest()


def write_bytes_atomic(path, data: bytes) -> None:
    import os
    import tempfile
    from pathlib import Path

    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=".{}.".format(destination.name),
        suffix=".tmp",
        dir=str(destination.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, destination)
        dir_fd = os.open(str(destination.parent), os.O_DIRECTORY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def write_text_atomic(path, text: str) -> None:
    write_bytes_atomic(path, text.encode("utf-8"))
