"""Sample-library persistence and atomic migration.

The library index and migration progress live in a JSON manifest written
via temp-file + atomic rename. Loading validates the library version and
content hash (E_LIB_MISMATCH on mismatch). Samples whose migration has not
completed keep their original version files; new files never overwrite
unmigrated data.
"""

from __future__ import annotations

import hashlib
import json
import os
import threading

from .errors import E_BUSY, E_LIB_MISMATCH, FpError
from .formats import load_fingerprint, save_fingerprint
from .migrate import migrate_fingerprint

LIB_VERSION = 3
MANIFEST_NAME = "library.json"


def _sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def _atomic_write_json(path, obj):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True)
        fh.write("\n")
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class Library:
    def __init__(self, root):
        self.root = root
        self.samples = []
        self.migration = {"state": "idle", "done": [], "pending": []}
        self._migrate_lock = threading.Lock()
        os.makedirs(os.path.join(root, "fingerprints"), exist_ok=True)

    @property
    def manifest_path(self):
        return os.path.join(self.root, MANIFEST_NAME)

    def _content_hash(self):
        h = hashlib.sha256()
        for s in sorted(self.samples, key=lambda s: s["sample_id"]):
            h.update(s["sample_id"].encode())
            h.update(s["path"].encode())
            h.update(str(s["version"]).encode())
            h.update(s["sha256"].encode())
        return h.hexdigest()

    def save(self):
        manifest = {
            "lib_version": LIB_VERSION,
            "samples": sorted(self.samples, key=lambda s: s["sample_id"]),
            "migration": self.migration,
        }
        manifest["content_hash"] = self._content_hash()
        _atomic_write_json(self.manifest_path, manifest)

    def load(self):
        try:
            with open(self.manifest_path, "r", encoding="utf-8") as fh:
                manifest = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            raise FpError(E_LIB_MISMATCH,
                          f"cannot read library manifest: {exc}",
                          file=self.manifest_path, field="$")
        version = manifest.get("lib_version")
        if version != LIB_VERSION:
            raise FpError(E_LIB_MISMATCH,
                          f"library version {version!r} != expected "
                          f"{LIB_VERSION}",
                          file=self.manifest_path, field="lib_version")
        self.samples = manifest.get("samples", [])
        self.migration = manifest.get(
            "migration", {"state": "idle", "done": [], "pending": []})
        expected = manifest.get("content_hash")
        actual = self._content_hash()
        if expected != actual:
            raise FpError(E_LIB_MISMATCH,
                          f"content hash mismatch: manifest={expected} "
                          f"computed={actual}",
                          file=self.manifest_path, field="content_hash")
        return self

    def add_fingerprint_file(self, path):
        fp = load_fingerprint(path)
        rel = os.path.relpath(path, self.root)
        entry = {
            "sample_id": fp.sample_id,
            "path": rel,
            "version": fp.source_version,
            "sha256": _sha256_file(path),
            "migrated": False,
            "original_path": None,
        }
        self.samples = [s for s in self.samples
                        if s["sample_id"] != fp.sample_id]
        self.samples.append(entry)
        return entry

    def load_fingerprints(self):
        fps = []
        for s in sorted(self.samples, key=lambda s: s["sample_id"]):
            fps.append(load_fingerprint(os.path.join(self.root, s["path"])))
        return fps

    def version_distribution(self):
        dist = {}
        for s in self.samples:
            dist[s["version"]] = dist.get(s["version"], 0) + 1
        return dict(sorted(dist.items()))

    def migrate_all(self, target_version=3, progress_cb=None):
        """Migrate every non-v3 sample. Only one migration may run at a
        time; a concurrent submission fails with E_BUSY."""
        if not self._migrate_lock.acquire(blocking=False):
            raise FpError(E_BUSY, "a migration task is already running "
                          "for this library")
        try:
            pending = [s for s in self.samples
                       if s["version"] != target_version]
            self.migration = {
                "state": "running",
                "done": [],
                "pending": [s["sample_id"] for s in pending],
            }
            self.save()
            reports = []
            for i, entry in enumerate(pending):
                src = os.path.join(self.root, entry["path"])
                fp = load_fingerprint(src)
                new_fp, report = migrate_fingerprint(fp, target_version)
                # new file alongside the original; original is never
                # overwritten, so an interrupted migration loses nothing
                dst = os.path.join(
                    self.root, "fingerprints",
                    f"{entry['sample_id']}.v{target_version}.json")
                save_fingerprint(new_fp, dst, version=target_version)
                entry["original_path"] = entry["path"]
                entry["path"] = os.path.relpath(dst, self.root)
                entry["version"] = target_version
                entry["sha256"] = _sha256_file(dst)
                entry["migrated"] = True
                self.migration["done"].append(entry["sample_id"])
                self.migration["pending"].remove(entry["sample_id"])
                self.save()  # atomic checkpoint after each sample
                reports.append(report)
                if progress_cb:
                    progress_cb(i + 1, len(pending), entry["sample_id"])
            self.migration["state"] = "done"
            self.save()
            return reports
        finally:
            self._migrate_lock.release()
