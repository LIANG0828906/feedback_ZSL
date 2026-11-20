from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .hashing import decode_payload, encode_payload, stable_hash


@dataclass
class CacheEntry:
    content_hash: str
    payload: Any
    hit: bool = False


class ContentAddressedCache:
    """Small persistent cache. Keys and content hashes are derived from content only."""

    def __init__(self, root: str | Path, enabled: bool = True) -> None:
        self.root = Path(root)
        self.enabled = enabled
        self.root.mkdir(parents=True, exist_ok=True)

    @staticmethod
    def key(stage: str, identity: Any) -> str:
        return stable_hash({"stage": stage, "identity": identity})

    def _path(self, key: str) -> Path:
        return self.root / f"{key}.json"

    def get(self, key: str) -> CacheEntry | None:
        if not self.enabled:
            return None
        path = self._path(key)
        if not path.is_file():
            return None
        try:
            with open(path, "r", encoding="utf-8") as handle:
                data = json.load(handle)
            content_hash = data["content_hash"]
            payload = decode_payload(data["payload"])
            if stable_hash(payload) != content_hash:
                return None
            return CacheEntry(content_hash=content_hash, payload=payload, hit=True)
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
            return None

    def put(self, key: str, payload: Any) -> CacheEntry:
        encoded = encode_payload(payload)
        content_hash = stable_hash(encoded)
        if self.enabled:
            path = self._path(key)
            temp = path.with_suffix(".json.tmp")
            document = {"content_hash": content_hash, "payload": encoded}
            try:
                with open(temp, "w", encoding="utf-8") as handle:
                    json.dump(document, handle, sort_keys=True, separators=(",", ":"))
                    handle.write("\n")
                os.replace(temp, path)
            finally:
                if temp.exists():
                    temp.unlink(missing_ok=True)
        return CacheEntry(content_hash=content_hash, payload=payload, hit=False)

    def get_or_compute(self, stage: str, identity: Any, compute: Any) -> CacheEntry:
        key = self.key(stage, identity)
        hit = self.get(key)
        if hit is not None:
            return hit
        return self.put(key, compute())

