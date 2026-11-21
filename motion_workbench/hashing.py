from __future__ import annotations

import hashlib
import json
from typing import Any


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_text(text: str) -> str:
    return sha256_bytes(text.encode("utf-8"))


def content_hash(value: Any) -> str:
    return sha256_text(canonical_json(value))


def chain_hash(
    previous_hash: str,
    interval_content_hash: str,
    parameters_version: str,
    table_version: int,
    start: int,
    end: int,
) -> str:
    return content_hash(
        {
            "previous_hash": previous_hash,
            "content_hash": interval_content_hash,
            "parameters_version": parameters_version,
            "table_version": table_version,
            "start": start,
            "end": end,
        }
    )
