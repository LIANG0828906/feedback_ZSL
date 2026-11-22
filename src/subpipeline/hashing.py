"""Stable content hashing helpers used for snapshot versioning."""

import hashlib
import json


def canonical_json(obj):
    """Deterministic JSON text independent of dict insertion order."""
    return json.dumps(obj, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def content_hash(obj):
    return hashlib.sha256(canonical_json(obj).encode("utf-8")).hexdigest()


def short_hash(obj):
    return content_hash(obj)[:16]
