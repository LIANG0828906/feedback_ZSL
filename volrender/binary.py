"""Deterministic binary scene container.

Layout (little endian):
  magic            8 bytes  b"VOLSCN1\\0"
  header_length    u32
  payload_length   u32
  header           UTF-8 canonical JSON
  density_payload  contiguous float32 values
  checksum         SHA-256(header || payload), 32 bytes
"""

from __future__ import annotations

import hashlib
import json
import struct
from pathlib import Path
from typing import Any

from .errors import AppError, ErrorCode, scene_error
from .scene import Scene, build_scene


MAGIC = b"VOLSCN1\0"
FORMAT_VERSION = 1
_PREFIX = struct.Struct("<8sII")
_CHECKSUM_SIZE = 32


def encode_scene(header: dict[str, Any], payload: bytes) -> bytes:
    canonical = json.dumps(
        header,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    prefix = _PREFIX.pack(MAGIC, len(canonical), len(payload))
    body = prefix + canonical + payload
    return body + hashlib.sha256(body).digest()


def decode_scene(raw: bytes) -> Scene:
    if len(raw) < _PREFIX.size + _CHECKSUM_SIZE:
        raise scene_error("scene binary is truncated", field_name="binary.length")
    magic, header_length, payload_length = _PREFIX.unpack_from(raw, 0)
    if magic != MAGIC:
        raise scene_error("invalid scene magic", field_name="binary.magic")
    expected_size = _PREFIX.size + header_length + payload_length + _CHECKSUM_SIZE
    if len(raw) != expected_size:
        raise scene_error(
            f"scene binary length mismatch: expected {expected_size}, got {len(raw)}",
            field_name="binary.length",
        )
    header_end = _PREFIX.size + header_length
    payload_end = header_end + payload_length
    header_bytes = raw[_PREFIX.size : header_end]
    payload = raw[header_end:payload_end]
    stored_checksum = raw[payload_end:]
    checksum = hashlib.sha256(raw[:payload_end]).digest()
    if stored_checksum != checksum:
        raise scene_error("scene checksum mismatch", field_name="binary.checksum")
    try:
        header = json.loads(header_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise scene_error(
            f"scene header is not valid UTF-8 JSON: {exc}",
            field_name="header",
        ) from exc
    if not isinstance(header, dict):
        raise scene_error("scene header must be an object", field_name="header")
    if header.get("format") != "volrender-scene":
        raise scene_error("unsupported scene format", field_name="format")
    if header.get("format_version") != FORMAT_VERSION:
        raise scene_error(
            "unsupported scene format version", field_name="format_version"
        )
    return build_scene(header, payload)


def load_scene(path: str | Path) -> Scene:
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise AppError(
            ErrorCode.E_IO,
            f"cannot read scene binary: {exc}",
            field="scene.path",
        ) from exc
    return decode_scene(raw)
