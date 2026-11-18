"""Small shared helpers."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any


def stable_hash64(text: str) -> int:
    digest = hashlib.blake2b(text.encode("utf-8"), digest_size=8).digest()
    return int.from_bytes(digest, "big", signed=False)


def sha256_file(path: Path, chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        while True:
            chunk = fh.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON object key: {key}")
        result[key] = value
    return result


def load_json_strict(path: Path) -> Any:
    text = path.read_text(encoding="utf-8-sig")

    def bad_constant(value: str) -> None:
        raise ValueError(f"non-finite JSON number is not allowed: {value}")

    return json.loads(
        text,
        object_pairs_hook=reject_duplicate_pairs,
        parse_float=__import__("decimal").Decimal,
        parse_constant=bad_constant,
    )


def atomic_write_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(text)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_name, path)
    finally:
        if os.path.exists(tmp_name):
            os.unlink(tmp_name)


def write_json(path: Path, payload: Any) -> None:
    atomic_write_text(path, json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n")


def normalize_for_compare(value: Any, volatile_keys: set[str] | None = None) -> Any:
    """Test helper: recursively drop known volatile report fields."""

    volatile = volatile_keys or set()
    if isinstance(value, dict):
        return {
            key: normalize_for_compare(item, volatile)
            for key, item in value.items()
            if key not in volatile
        }
    if isinstance(value, list):
        return [normalize_for_compare(item, volatile) for item in value]
    return value
