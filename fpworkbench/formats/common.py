"""Shared helpers for fingerprint file formats: canonical JSON + checksum."""

from __future__ import annotations

import hashlib
import json

from ..errors import E_FMT_INVALID, FpError


def canonical_json(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def compute_checksum(payload: dict) -> str:
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()


def require(doc: dict, key: str, path: str, file: str):
    if key not in doc:
        raise FpError(E_FMT_INVALID, f"missing required field '{key}'",
                      file=file, field=path)
    return doc[key]


def require_type(doc: dict, key: str, types, path: str, file: str):
    val = require(doc, key, path, file)
    if not isinstance(val, tuple(types)):
        raise FpError(E_FMT_INVALID,
                      f"field '{key}' has wrong type: {type(val).__name__}",
                      file=file, field=path)
    # bool is a subclass of int; reject it where a real number is expected
    if isinstance(val, bool) and bool not in tuple(types):
        raise FpError(E_FMT_INVALID,
                      f"field '{key}' has wrong type: {type(val).__name__}",
                      file=file, field=path)
    return val


def verify_checksum(doc: dict, file: str):
    """doc = {"payload": {...}, "checksum": "..."} envelope validation."""
    payload = require(doc, "payload", "payload", file)
    checksum = require(doc, "checksum", "checksum", file)
    actual = compute_checksum(payload)
    if actual != checksum:
        raise FpError(E_FMT_INVALID,
                      f"checksum mismatch: expected {checksum}, computed {actual}",
                      file=file, field="checksum")
    return payload


def wrap_payload(payload: dict) -> dict:
    return {"payload": payload, "checksum": compute_checksum(payload)}
