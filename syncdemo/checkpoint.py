"""Atomic, serialised checkpoint persistence.

Each commit window writes one record containing the authoritative state,
sub-RNG cursor (the tick, since the RNG is derived per tick), the input
consumption offset, per-client confirmed frame numbers, a content hash of
the snapshot and the algorithm version.  Writes go through a single lock
and tmp+os.replace so they are serialised and atomic.

On load, if the match seed, input fingerprint or algorithm version differ
from the caller's, *all* snapshots are discarded together — partial mixing
is forbidden.  Re-committing an already committed window with different
content raises instead of silently rewriting history.
"""

from __future__ import annotations

import hashlib
import json
import os
import threading
from pathlib import Path

from .errors import SyncError


def canonical(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def content_hash(obj) -> str:
    return hashlib.sha256(canonical(obj).encode("utf-8")).hexdigest()


class CheckpointStore:
    def __init__(self, path, *, seed, input_fingerprint, algo_version):
        self.path = Path(path)
        self.meta = {
            "seed": seed,
            "input_fingerprint": input_fingerprint,
            "algo_version": algo_version,
        }
        self._lock = threading.Lock()

    def commit(self, window: int, snapshot: dict):
        """Atomically and serially persist one commit window."""
        record = {
            "window": window,
            "meta": dict(self.meta),
            "hash": content_hash(snapshot),
            "snapshot": snapshot,
        }
        with self._lock:
            self.path.mkdir(parents=True, exist_ok=True)
            final = self.path / f"window_{window}.json"
            if final.exists():
                prior = json.loads(final.read_text(encoding="utf-8"))
                if prior.get("hash") != record["hash"]:
                    raise SyncError(
                        f"committed window {window} must not be silently rewritten"
                    )
                return  # idempotent re-commit of identical content
            tmp = self.path / f"window_{window}.json.tmp"
            tmp.write_text(json.dumps(record), encoding="utf-8")
            os.replace(tmp, final)
            self._update_manifest(window)

    def _update_manifest(self, window):
        mpath = self.path / "manifest.json"
        manifest = {"meta": dict(self.meta), "windows": []}
        if mpath.exists():
            manifest = json.loads(mpath.read_text(encoding="utf-8"))
        if window not in manifest["windows"]:
            manifest["windows"].append(window)
            manifest["windows"].sort()
        tmp = self.path / "manifest.json.tmp"
        tmp.write_text(json.dumps(manifest), encoding="utf-8")
        os.replace(tmp, mpath)

    def load(self):
        """Return ordered snapshots, or None if the store is unusable.

        Any metadata mismatch or corrupted record invalidates the *whole*
        store; callers must then recompute from scratch.
        """
        mpath = self.path / "manifest.json"
        if not mpath.exists():
            return None
        try:
            manifest = json.loads(mpath.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            return None
        if manifest.get("meta") != self.meta:
            return None
        snapshots = []
        for window in sorted(manifest.get("windows", [])):
            final = self.path / f"window_{window}.json"
            if not final.exists():
                return None
            try:
                record = json.loads(final.read_text(encoding="utf-8"))
            except json.JSONDecodeError:
                return None
            if record.get("meta") != self.meta:
                return None
            if content_hash(record.get("snapshot")) != record.get("hash"):
                return None
            snapshots.append(record["snapshot"])
        return snapshots
