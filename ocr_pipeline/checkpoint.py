"""Persistent, atomic, serialized interval checkpoint store.

Layout per image:
  <root>/<image_id>/manifest.json        image-level snapshot metadata
  <root>/<image_id>/intervals/NNNN.json  one record per committed interval

Every write goes to a temp file followed by os.replace (atomic on POSIX and
Windows) while holding a process-global lock, so writes never interleave and
commit order is stable. On load, any mismatch of content hash, source
version, parameter version or algorithm version invalidates the whole
snapshot.
"""

from __future__ import annotations

import json
import os
import shutil
import threading
from pathlib import Path
from typing import Dict, List, Optional, Tuple


class CheckpointStore:
    def __init__(self, root) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self.commit_log: List[Tuple[str, int]] = []

    def _image_dir(self, image_id: str) -> Path:
        return self.root / image_id

    def _manifest_path(self, image_id: str) -> Path:
        return self._image_dir(image_id) / "manifest.json"

    def _interval_path(self, image_id: str, interval_index: int) -> Path:
        return (
            self._image_dir(image_id) / "intervals" / f"{interval_index:04d}.json"
        )

    def _atomic_write(self, path: Path, payload: Dict) -> None:
        tmp = path.with_name(f"{path.name}.tmp.{os.getpid()}.{threading.get_ident()}")
        with self._lock:
            path.parent.mkdir(parents=True, exist_ok=True)
            tmp.write_text(
                json.dumps(payload, ensure_ascii=False, sort_keys=True,
                           separators=(",", ":")) + "\n",
                encoding="utf-8",
            )
            os.replace(tmp, path)

    def write_manifest(self, image_id: str, manifest: Dict) -> None:
        self._atomic_write(self._manifest_path(image_id), manifest)

    def commit_interval(self, image_id: str, interval_index: int,
                        record: Dict) -> None:
        """Atomically persist one committed interval, serialized."""
        self._atomic_write(self._interval_path(image_id, interval_index), record)
        with self._lock:
            self.commit_log.append((image_id, interval_index))

    def load(self, image_id: str) -> Optional[Tuple[Dict, List[Dict]]]:
        manifest_path = self._manifest_path(image_id)
        if not manifest_path.exists():
            return None
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        records: List[Dict] = []
        for index in range(manifest.get("committed_intervals", 0)):
            path = self._interval_path(image_id, index)
            if not path.exists():
                return None  # incomplete snapshot: treat as absent
            records.append(json.loads(path.read_text(encoding="utf-8")))
        return manifest, records

    def snapshot_matches(
        self,
        manifest: Dict,
        *,
        content_hash: str,
        source_version: str,
        param_version: str,
        algo_version: str,
    ) -> bool:
        return (
            manifest.get("content_hash") == content_hash
            and manifest.get("source_version") == source_version
            and manifest.get("param_version") == param_version
            and manifest.get("algo_version") == algo_version
        )

    def invalidate(self, image_id: str) -> None:
        with self._lock:
            shutil.rmtree(self._image_dir(image_id), ignore_errors=True)
