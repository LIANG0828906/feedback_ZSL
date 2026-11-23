"""Atomic, content-addressed snapshot persistence.

Layout under ``<workdir>/snapshots``::

    stage_<stage_id>__<cache_key>.npy   immutable stage output blobs
    manifest.json                        commit point (atomic os.replace)

A snapshot is accepted only when every stage key, parameter version, input
content hash and output blob hash verifies. Any single mismatch invalidates
the whole snapshot; partial reuse of old stage results is never allowed.
"""

from __future__ import annotations

import json
import os
import tempfile
import threading

import numpy as np

from .hashing import image_hash
from .stages import stage_cache_key


class SnapshotStore:
    def __init__(self, workdir: str, schema_version: str):
        self.dir = os.path.join(workdir, "snapshots")
        os.makedirs(self.dir, exist_ok=True)
        self.schema_version = schema_version
        self._lock = threading.Lock()

    # -- writing -----------------------------------------------------------

    def write_snapshot(self, source_hash: str, entries: list) -> str:
        """Serially write stage blobs and atomically commit the manifest.

        entries: list of dicts with stage_id, stage object, output,
        input_hash, upstream_key, cache_key, param_version.
        """
        with self._lock:
            manifest = {
                "schema_version": self.schema_version,
                "source_hash": source_hash,
                "stages": [],
            }
            blob_paths = []
            try:
                for entry in entries:
                    blob = f"stage_{entry['stage_id']}__{entry['cache_key']}.npy"
                    blob_path = os.path.join(self.dir, blob)
                    tmp_fd, tmp_name = tempfile.mkstemp(
                        suffix=".npy.tmp", dir=self.dir)
                    with os.fdopen(tmp_fd, "wb") as fh:
                        np.save(fh, np.ascontiguousarray(entry["output"]))
                    os.replace(tmp_name, blob_path)
                    blob_paths.append(blob_path)
                    manifest["stages"].append({
                        "stage_id": entry["stage_id"],
                        "cache_key": entry["cache_key"],
                        "param_version": entry["param_version"],
                        "input_hash": entry["input_hash"],
                        "upstream_key": entry["upstream_key"],
                        "output_hash": image_hash(entry["output"]),
                        "blob": blob,
                    })
                manifest_path = os.path.join(self.dir, "manifest.json")
                tmp_fd, tmp_name = tempfile.mkstemp(
                    suffix=".json.tmp", dir=self.dir)
                with os.fdopen(tmp_fd, "w", encoding="utf-8") as fh:
                    json.dump(manifest, fh, indent=2, sort_keys=True)
                os.replace(tmp_name, manifest_path)
                live = {rec["blob"] for rec in manifest["stages"]}
                for name in os.listdir(self.dir):
                    if name.endswith(".npy") and name not in live:
                        os.remove(os.path.join(self.dir, name))
            except BaseException:
                raise
            return manifest_path

    # -- reading -----------------------------------------------------------

    def load_snapshot(self, source_hash: str, stages: list,
                      source_shape: tuple):
        """Return verified entries or None when the snapshot is wholly stale."""
        manifest_path = os.path.join(self.dir, "manifest.json")
        if not os.path.exists(manifest_path):
            return None
        try:
            with open(manifest_path, "r", encoding="utf-8") as fh:
                manifest = json.load(fh)
        except (json.JSONDecodeError, OSError):
            return None
        if manifest.get("schema_version") != self.schema_version:
            return None
        if manifest.get("source_hash") != source_hash:
            return None
        recorded = manifest.get("stages", [])
        if len(recorded) != len(stages):
            return None
        verified = []
        upstream_key = None
        input_hash = source_hash
        for stage, rec in zip(stages, recorded):
            expected_key = stage_cache_key(stage, input_hash, upstream_key)
            # Any mismatch -> whole snapshot is rejected, no partial reuse.
            if rec.get("stage_id") != stage.stage_id:
                return None
            if rec.get("param_version") != stage.param_version:
                return None
            if rec.get("cache_key") != expected_key:
                return None
            if rec.get("input_hash") != input_hash:
                return None
            if rec.get("upstream_key") != upstream_key:
                return None
            blob_path = os.path.join(self.dir, rec["blob"])
            if not os.path.exists(blob_path):
                return None
            try:
                output = np.load(blob_path, allow_pickle=False)
            except (OSError, ValueError):
                return None
            if output.shape[:2] != tuple(stage.output_size):
                return None
            if output.ndim != len(source_shape):
                return None
            if output.ndim == 3 and output.shape[2] != source_shape[2]:
                return None
            if image_hash(output) != rec.get("output_hash"):
                return None
            verified.append({
                "stage_id": stage.stage_id,
                "cache_key": expected_key,
                "param_version": stage.param_version,
                "input_hash": input_hash,
                "upstream_key": upstream_key,
                "output": output,
            })
            upstream_key = expected_key
            input_hash = image_hash(output)
        return verified

    def reset(self) -> None:
        with self._lock:
            for name in os.listdir(self.dir):
                os.remove(os.path.join(self.dir, name))

    def manifest_path(self) -> str:
        return os.path.join(self.dir, "manifest.json")
