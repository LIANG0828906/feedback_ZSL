"""Canonical JSON, numeric encoding, hashing and atomic file writes."""

from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from decimal import Decimal
from pathlib import Path
from typing import Any

import numpy as np

from .errors import KernelError


Number = Decimal | np.float64


def _reject_constant(token: str) -> None:
    raise ValueError(f"non-finite JSON constant: {token}")


def parse_json_bytes(raw: bytes) -> Any:
    text = raw.decode("utf-8")
    return json.loads(text, parse_constant=_reject_constant)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def canonical_bytes(value: Any) -> bytes:
    return canonical_json(value).encode("utf-8")


def canonical_decimal(value: Decimal) -> str:
    if not value.is_finite():
        raise ValueError("non-finite Decimal")
    if value == 0:
        return "0"
    text = format(value, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text


def encode_number(value: Number) -> dict[str, str]:
    if isinstance(value, Decimal):
        return {"format": "fixed", "value": canonical_decimal(value)}
    number = float(value)
    if not math.isfinite(number):
        raise ValueError("non-finite float64")
    return {
        "format": "float64",
        "hex": number.hex(),
        "value": repr(number),
    }


def decode_number(value: dict[str, str], mode: str) -> Number:
    if value.get("format") == "fixed":
        return Decimal(value["value"])
    if value.get("format") == "float64":
        return np.float64(float.fromhex(value["hex"]))
    raise ValueError("unknown encoded number")


def number_sort_key(value: Number) -> str:
    return canonical_json(encode_number(value))


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_object(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def seal_object(value: dict[str, Any], field_name: str) -> dict[str, Any]:
    sealed = dict(value)
    sealed.pop(field_name, None)
    sealed[field_name] = hash_object(sealed)
    return sealed


def verify_seal(value: dict[str, Any], field_name: str, code: str, path: str) -> None:
    expected = value.get(field_name)
    if not isinstance(expected, str):
        raise KernelError(code, f"missing {field_name}", {"path": path})
    body = dict(value)
    body.pop(field_name, None)
    actual = hash_object(body)
    if actual != expected:
        raise KernelError(
            code,
            f"{field_name} mismatch",
            {"path": path, "expected": expected, "actual": actual},
        )


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.",
        suffix=".tmp",
        dir=str(target.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
        dir_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(value) + b"\n")


def read_json_file(path: str | Path) -> Any:
    return parse_json_bytes(Path(path).read_bytes())
