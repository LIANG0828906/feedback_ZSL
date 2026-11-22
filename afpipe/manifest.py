"""Sample manifest schema, validation and content fingerprinting."""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

from .errors import SchemaError
from .hashing import canonical_bytes, read_json, stable_digest

MANIFEST_SCHEMA_VERSION = "1.0"
REQUIRED_TOP_KEYS = {"schema_version", "seed", "sample_rate", "channels",
                     "groups", "queries"}
REQUIRED_FILE_KEYS = {"file_id", "path", "container", "codec", "format_kind",
                      "sample_rate", "channels", "duration_s", "sha256"}


@dataclass(frozen=True)
class MediaFile:
    file_id: str
    path: str
    container: str
    codec: str
    format_kind: str          # "pcm" | "lossy"
    sample_rate: int
    channels: int
    duration_s: float
    sha256: str
    group_id: str
    role: str                 # "index" | "query"
    physical_key: str         # identity of one physical segment
    markers: tuple[dict, ...]

    def to_dict(self) -> dict[str, Any]:
        out = {
            "file_id": self.file_id,
            "path": self.path,
            "container": self.container,
            "codec": self.codec,
            "format_kind": self.format_kind,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "duration_s": self.duration_s,
            "sha256": self.sha256,
            "group_id": self.group_id,
            "role": self.role,
            "physical_key": self.physical_key,
            "markers": list(self.markers),
        }
        return out


class Manifest:
    def __init__(self, raw: dict, base_dir: str):
        self.raw = raw
        self.base_dir = os.path.abspath(base_dir)
        self.groups: dict[str, dict] = raw["groups"]
        self.queries_meta: dict[str, dict] = raw.get("queries", {})
        self.files: list[MediaFile] = []
        for group in raw["groups"].values():
            physical = group["physical_key"]
            for f in group["files"]:
                self.files.append(
                    MediaFile(
                        file_id=f["file_id"], path=f["path"],
                        container=f["container"], codec=f["codec"],
                        format_kind=f["format_kind"],
                        sample_rate=f["sample_rate"], channels=f["channels"],
                        duration_s=float(f["duration_s"]), sha256=f["sha256"],
                        group_id=group["group_id"], role="index",
                        physical_key=physical,
                        markers=tuple(f.get("markers", [])),
                    )
                )
        for q in raw.get("queries", {}).values():
            for f in q["files"]:
                self.files.append(
                    MediaFile(
                        file_id=f["file_id"], path=f["path"],
                        container=f["container"], codec=f["codec"],
                        format_kind=f["format_kind"],
                        sample_rate=f["sample_rate"], channels=f["channels"],
                        duration_s=float(f["duration_s"]), sha256=f["sha256"],
                        group_id=q["group_id"], role="query",
                        physical_key=q["physical_key"],
                        markers=tuple(f.get("markers", [])),
                    )
                )

    @property
    def fingerprint(self) -> str:
        """Content fingerprint of the manifest itself (excludes file bytes)."""
        return stable_digest(self.raw, prefix="mf-", length=16)

    @property
    def seed(self) -> int:
        return int(self.raw["seed"])

    def file(self, file_id: str) -> MediaFile:
        for f in self.files:
            if f.file_id == file_id:
                return f
        raise SchemaError("unknown file_id", details={"file_id": file_id})

    def index_files(self) -> list[MediaFile]:
        return [f for f in self.files if f.role == "index"]

    def query_files(self) -> list[MediaFile]:
        return [f for f in self.files if f.role == "query"]

    def absolute_path(self, f: MediaFile) -> str:
        return os.path.join(self.base_dir, f.path)


def validate_schema(raw: Any) -> None:
    if not isinstance(raw, dict):
        raise SchemaError("manifest root must be an object")
    missing = REQUIRED_TOP_KEYS - raw.keys()
    if missing:
        raise SchemaError("manifest missing top-level keys",
                          details={"missing": sorted(missing)})
    if not isinstance(raw["groups"], dict) or not raw["groups"]:
        raise SchemaError("groups must be a non-empty object")
    seen_ids: set[str] = set()
    for gid, group in raw["groups"].items():
        for key in ("group_id", "physical_key", "files"):
            if key not in group:
                raise SchemaError("group missing key",
                                  details={"group": gid, "key": key})
        if not isinstance(group["files"], list) or not group["files"]:
            raise SchemaError("group files must be a non-empty list",
                              details={"group": gid})
        for f in group["files"]:
            _validate_file(f, gid, seen_ids)
    for qid, query in raw.get("queries", {}).items():
        for key in ("query_id", "group_id", "physical_key", "files"):
            if key not in query:
                raise SchemaError("query missing key",
                                  details={"query": qid, "key": key})
        for f in query["files"]:
            _validate_file(f, qid, seen_ids)


def _validate_file(f: Any, owner: str, seen_ids: set[str]) -> None:
    if not isinstance(f, dict):
        raise SchemaError("file entry must be an object", details={"owner": owner})
    missing = REQUIRED_FILE_KEYS - f.keys()
    if missing:
        raise SchemaError("file entry missing keys",
                          details={"owner": owner, "missing": sorted(missing)})
    if f["file_id"] in seen_ids:
        raise SchemaError("duplicate file_id", details={"file_id": f["file_id"]})
    seen_ids.add(f["file_id"])
    if f["format_kind"] not in ("pcm", "lossy"):
        raise SchemaError("format_kind must be pcm|lossy",
                          details={"file_id": f["file_id"]})
    for numeric in ("sample_rate", "channels", "duration_s"):
        if not isinstance(f[numeric], (int, float)) or f[numeric] <= 0:
            raise SchemaError(f"{numeric} must be a positive number",
                              details={"file_id": f["file_id"]})
    if not isinstance(f["sha256"], str) or len(f["sha256"]) != 64:
        raise SchemaError("sha256 must be a 64-char hex string",
                          details={"file_id": f["file_id"]})


def load_manifest(path: str) -> Manifest:
    try:
        raw = read_json(path)
    except FileNotFoundError as exc:
        raise SchemaError("manifest file not found",
                          details={"path": path}) from exc
    except ValueError as exc:
        raise SchemaError("manifest is not valid JSON",
                          details={"path": path, "reason": str(exc)}) from exc
    validate_schema(raw)
    return Manifest(raw, base_dir=os.path.dirname(os.path.abspath(path)))


def manifest_digest(raw: dict) -> str:
    return stable_digest(raw, prefix="mf-", length=16)


def dump_manifest(raw: dict, path: str) -> None:
    from .hashing import atomic_write_bytes

    atomic_write_bytes(path, canonical_bytes(raw))
