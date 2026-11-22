"""内容哈希：用于检查点片段的完整性校验."""

from __future__ import annotations

import hashlib
import json


def canonical_json(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def content_hash(obj) -> str:
    """对任意 JSON 可序列化对象产出稳定 SHA-256 摘要."""
    return hashlib.sha256(canonical_json(obj).encode("utf-8")).hexdigest()
