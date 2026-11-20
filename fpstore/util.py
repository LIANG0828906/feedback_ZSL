"""Deterministic serialization, hashing, atomic file writes and trace logging."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import threading
from pathlib import Path
from typing import Any, Iterable


GENESIS_HASH = "GENESIS"


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def content_hash(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def hash_label(digest: str) -> str:
    return digest if digest.startswith("sha256:") else f"sha256:{digest}"


def fsync_dir(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_name, path)
        fsync_dir(path.parent)
    except BaseException:
        try:
            os.unlink(tmp_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(value))


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def manifest_hash(manifest_core: dict[str, Any]) -> str:
    return hash_label(content_hash(manifest_core))


def chain_hash(previous: str, entry_core: dict[str, Any]) -> str:
    return hash_label(sha256_bytes(previous.encode("utf-8") + canonical_bytes(entry_core)))


def make_chain(entries: Iterable[dict[str, Any]], seed: str) -> tuple[list[dict[str, Any]], str]:
    previous = seed
    chained: list[dict[str, Any]] = []
    for entry in entries:
        core = {key: value for key, value in entry.items() if key not in {"prev_chain_hash", "chain_hash"}}
        core["prev_chain_hash"] = previous
        current = chain_hash(previous, core)
        item = dict(core)
        item["chain_hash"] = current
        chained.append(item)
        previous = current
    return chained, previous


def verify_chain(entries: Iterable[dict[str, Any]], seed: str) -> tuple[bool, str | None]:
    previous = seed
    for entry in entries:
        if entry.get("prev_chain_hash") != previous:
            return False, str(entry.get("key") or entry.get("track_id") or "?")
        core = {key: value for key, value in entry.items() if key not in {"prev_chain_hash", "chain_hash"}}
        core["prev_chain_hash"] = previous
        expected = chain_hash(previous, core)
        if entry.get("chain_hash") != expected:
            return False, str(entry.get("key") or entry.get("track_id") or "?")
        previous = expected
    return True, None


def stable_version_id(operation: str, parent_hash: str, plan_core: Any) -> str:
    digest = content_hash(
        {
            "operation": operation,
            "parent_hash": parent_hash,
            "plan": plan_core,
        }
    )
    return f"{operation}-{digest[:20]}"


class TraceLogger:
    """Canonical JSONL trace without clocks, paths, PIDs or random temp names."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.sequence = 0
        self._guard = threading.Lock()
        if self.path.exists():
            with self.path.open("r", encoding="utf-8") as handle:
                self.sequence = sum(1 for _ in handle)

    def emit(self, event: str, **fields: Any) -> dict[str, Any]:
        with self._guard:
            if self.path.exists():
                with self.path.open("r", encoding="utf-8") as handle:
                    self.sequence = sum(1 for _ in handle)
            row = {"seq": self.sequence, "event": event, **fields}
            data = canonical_bytes(row) + b"\n"
            with self.path.open("ab") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            self.sequence += 1
            return row
