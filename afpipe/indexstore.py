"""Fingerprint index persistence, version gating and explicit upgrades.

V2 layout (current)::

    index/
      registry.json          # identity, entries, lineage, committed atomically
      postings/<entry>.json  # {hash_hex_int_string: [[frame_idx, t_s], ...]}

Every registry carries:

* ``index_format_version`` - layout version (load mismatch -> E_VER);
* ``parameter_version``     - config fingerprint (mismatch -> E_VER);
* ``algorithm_version``     - extraction algorithm version;
* ``manifest_fingerprint``  - manifest content identity;
* per-entry source content hashes and lineage.

Silent downgrade or mixing is never performed: callers must either build a
fresh store or run :func:`upgrade_legacy_store` explicitly.
"""

from __future__ import annotations
import os
from dataclasses import dataclass

from .config import (
    ALGORITHM_VERSION,
    INDEX_FORMAT_VERSION,
    LEGACY_INDEX_FORMAT_VERSION,
    Config,
)
from .errors import CorruptionError, VersionError
from .fingerprint import FingerprintTrack
from .hashing import (
    atomic_write_json,
    ensure_dir,
    read_json,
    stable_digest,
)
from .manifest import MediaFile
from .fingerprint import PREFIX_BITS, code_bit_count, coarse_key

REGISTRY_NAME = "registry.json"
POSTINGS_DIR = "postings"


@dataclass
class IndexEntry:
    entry_id: str
    file_id: str
    path: str
    format_kind: str           # pcm | lossy
    container: str
    codec: str
    group_id: str
    physical_key: str
    role: str
    content_hash: str
    frame_count: int
    duration_s: float
    lineage: str               # "native" | "upgraded-from-1.0.0"

    def to_dict(self) -> dict:
        return {
            "entry_id": self.entry_id,
            "file_id": self.file_id,
            "path": self.path,
            "format_kind": self.format_kind,
            "container": self.container,
            "codec": self.codec,
            "group_id": self.group_id,
            "physical_key": self.physical_key,
            "role": self.role,
            "content_hash": self.content_hash,
            "frame_count": self.frame_count,
            "duration_s": self.duration_s,
            "lineage": self.lineage,
        }

    @classmethod
    def from_dict(cls, d: dict) -> "IndexEntry":
        return cls(**{k: d[k] for k in cls.__dataclass_fields__})


def entry_id_for(file_id: str, content_hash: str) -> str:
    return f"{file_id}-{stable_digest(content_hash, length=10)}"


class IndexBuilder:
    """Serial, deterministic index writer (one builder per store)."""

    def __init__(self, index_dir: str, cfg: Config, manifest_fingerprint: str):
        self.dir = ensure_dir(index_dir)
        self.postings_dir = ensure_dir(os.path.join(self.dir, POSTINGS_DIR))
        self.cfg = cfg
        self.manifest_fingerprint = manifest_fingerprint
        self.entries: list[IndexEntry] = []
        self._entry_ids: set[str] = set()

    def add_track(self, media: MediaFile, track: FingerprintTrack,
                  content_hash: str, *, lineage: str = "native",
                  entry_id: str | None = None) -> IndexEntry:
        eid = entry_id or entry_id_for(media.file_id, content_hash)
        if eid in self._entry_ids:
            raise CorruptionError("duplicate entry_id in store",
                                  details={"entry_id": eid})
        info_bits = code_bit_count(self.cfg.quant_bits, self.cfg.bands)
        prefix_bits = PREFIX_BITS[self.cfg.quant_bits]
        postings: dict[str, list[list]] = {}
        for frame in track.frames:
            value = int(frame.hash_hex, 16)
            key = str(coarse_key(value, info_bits, prefix_bits))
            postings.setdefault(key, []).append(
                [frame.index, frame.time_s, value]
            )
        for key in postings:
            postings[key].sort()
        atomic_write_json(os.path.join(self.postings_dir, f"{eid}.json"),
                          {"entry_id": eid, "postings": postings})
        entry = IndexEntry(
            entry_id=eid,
            file_id=media.file_id,
            path=media.path,
            format_kind=media.format_kind,
            container=media.container,
            codec=media.codec,
            group_id=media.group_id,
            physical_key=media.physical_key,
            role=media.role,
            content_hash=content_hash,
            frame_count=len(track.frames),
            duration_s=track.duration_s,
            lineage=lineage,
        )
        self.entries.append(entry)
        self._entry_ids.add(eid)
        return entry

    def commit(self) -> dict:
        # Stable order = manifest order of insertion.
        registry = {
            "index_format_version": INDEX_FORMAT_VERSION,
            "parameter_version": self.cfg.parameter_version(),
            "algorithm_version": ALGORITHM_VERSION,
            "quant_bits": self.cfg.quant_bits,
            "resample": {
                "sample_rate": self.cfg.sample_rate,
                "channel_mode": self.cfg.channel_mode,
                "max_channels": self.cfg.max_channels,
            },
            "frame": {
                "frame_ms": self.cfg.frame_ms,
                "hop_ms": self.cfg.hop_ms,
                "bands": self.cfg.bands,
            },
            "manifest_fingerprint": self.manifest_fingerprint,
            "entries": [e.to_dict() for e in self.entries],
        }
        atomic_write_json(os.path.join(self.dir, REGISTRY_NAME), registry)
        return registry


class IndexStore:
    """Read-side loader with strict version/parameter gating."""

    def __init__(self, index_dir: str):
        self.dir = os.path.abspath(index_dir)
        self.registry_path = os.path.join(self.dir, REGISTRY_NAME)
        self.registry: dict = {}
        self.entries: list[IndexEntry] = []
        self._postings_cache: dict[str, dict] = {}

    def exists(self) -> bool:
        return os.path.isfile(self.registry_path)

    def load(self, cfg: Config, *, manifest_fingerprint: str | None = None,
             expected_format: str = INDEX_FORMAT_VERSION) -> "IndexStore":
        if not self.exists():
            raise VersionError("index registry missing",
                               details={"path": self.registry_path})
        try:
            registry = read_json(self.registry_path)
        except ValueError as exc:
            raise VersionError("index registry unreadable",
                               details={"reason": str(exc)}) from exc

        fmt = registry.get("index_format_version")
        if fmt != expected_format:
            raise VersionError(
                "index format version mismatch; query refused",
                details={"field": "index_format_version",
                         "expected": expected_format, "actual": fmt,
                         "hint": "run explicit upgrade instead of silent mix"},
            )
        if registry.get("parameter_version") != cfg.parameter_version():
            raise VersionError(
                "index parameter version mismatch; query refused",
                details={"field": "parameter_version",
                         "expected": cfg.parameter_version(),
                         "actual": registry.get("parameter_version")},
            )
        if registry.get("algorithm_version") != ALGORITHM_VERSION:
            raise VersionError(
                "index algorithm version mismatch; query refused",
                details={"field": "algorithm_version",
                         "expected": ALGORITHM_VERSION,
                         "actual": registry.get("algorithm_version")},
            )
        if (manifest_fingerprint is not None
                and registry.get("manifest_fingerprint") != manifest_fingerprint):
            raise VersionError(
                "manifest fingerprint mismatch; index built from another corpus",
                details={"field": "manifest_fingerprint",
                         "expected": manifest_fingerprint,
                         "actual": registry.get("manifest_fingerprint")},
            )
        self.registry = registry
        self.entries = [IndexEntry.from_dict(e) for e in registry["entries"]]
        return self

    def postings(self, entry_id: str) -> dict[str, list[list]]:
        if entry_id not in self._postings_cache:
            doc = read_json(os.path.join(
                self.postings_path(entry_id)))
            self._postings_cache[entry_id] = doc["postings"]
        return self._postings_cache[entry_id]

    def postings_path(self, entry_id: str) -> str:
        return os.path.join(self.dir, POSTINGS_DIR, f"{entry_id}.json")

    def entry_by_file(self, file_id: str) -> IndexEntry | None:
        for e in self.entries:
            if e.file_id == file_id:
                return e
        return None

    def identity(self) -> dict:
        return {
            "index_format_version": self.registry.get("index_format_version"),
            "parameter_version": self.registry.get("parameter_version"),
            "algorithm_version": self.registry.get("algorithm_version"),
            "manifest_fingerprint": self.registry.get("manifest_fingerprint"),
            "entry_count": len(self.entries),
        }


# --------------------------------------------------------------- legacy v1
def write_legacy_v1_store(legacy_dir: str, cfg: Config,
                          manifest_fingerprint: str,
                          records: list[tuple[MediaFile, FingerprintTrack, str]]
                          ) -> str:
    """Emit a deliberately old-format (1.0.0) index for upgrade tests."""
    ensure_dir(legacy_dir)
    flat: list[dict] = []
    for media, track, content_hash in records:
        rows = [
            {"frame": f.index, "time": round(f.time_s, 9), "hash": f.hash_hex}
            for f in track.frames
        ]
        flat.append({
            "id": media.file_id,
            "src": media.path,
            "kind": media.format_kind,
            "container": media.container,
            "codec": media.codec,
            "group": media.group_id,
            "physical": media.physical_key,
            "role": media.role,
            "sha256": content_hash,
            "frames": rows,
        })
    registry = {
        "index_format_version": LEGACY_INDEX_FORMAT_VERSION,
        # Legacy parameter tag intentionally uses a different scheme.
        "param_tag": f"legacy|{cfg.short_label()}",
        "algorithm_version": ALGORITHM_VERSION,
        "manifest_fingerprint": manifest_fingerprint,
        "records": flat,
    }
    path = os.path.join(legacy_dir, REGISTRY_NAME)
    atomic_write_json(path, registry)
    return path


def read_legacy_v1(legacy_dir: str) -> dict:
    path = os.path.join(legacy_dir, REGISTRY_NAME)
    registry = read_json(path)
    if registry.get("index_format_version") != LEGACY_INDEX_FORMAT_VERSION:
        raise VersionError("not a legacy 1.0.0 store",
                           details={"actual": registry.get("index_format_version")})
    return registry


def upgrade_legacy_store(legacy_dir: str, target_dir: str, cfg: Config,
                         *, manifest_fingerprint: str) -> dict:
    """Explicit 1.0.0 -> 2.0.0 upgrade with a recorded mapping trail."""
    legacy = read_legacy_v1(legacy_dir)
    if legacy.get("manifest_fingerprint") != manifest_fingerprint:
        raise VersionError(
            "legacy store belongs to another manifest; upgrade refused",
            details={"field": "manifest_fingerprint"},
        )
    builder = IndexBuilder(target_dir, cfg, manifest_fingerprint)
    mapping: list[dict] = []
    for rec in legacy["records"]:
        from .fingerprint import FrameFingerprint

        frames = [
            FrameFingerprint(index=r["frame"], time_s=r["time"],
                             hash_hex=r["hash"])
            for r in rec["frames"]
        ]
        media = MediaFile(
            file_id=rec["id"], path=rec["src"],
            container=rec["container"], codec=rec["codec"],
            format_kind=rec["kind"], sample_rate=cfg.sample_rate,
            channels=1, duration_s=(frames[-1].time_s if frames else 0.0),
            sha256=rec["sha256"], group_id=rec["group"], role=rec["role"],
            physical_key=rec["physical"], markers=(),
        )
        new_id = entry_id_for(media.file_id, media.sha256)
        builder.add_track(media,
                          FingerprintTrack(media.file_id, frames, 0,
                                           media.duration_s),
                          media.sha256,
                          lineage=f"upgraded-from-{LEGACY_INDEX_FORMAT_VERSION}",
                          entry_id=new_id)
        mapping.append({
            "legacy_entry_id": rec["id"],
            "new_entry_id": new_id,
            "legacy_format": LEGACY_INDEX_FORMAT_VERSION,
            "new_format": INDEX_FORMAT_VERSION,
            "frame_count": len(frames),
            "content_hash": rec["sha256"],
        })
    registry = builder.commit()
    trail = {
        "upgrade": {
            "from": LEGACY_INDEX_FORMAT_VERSION,
            "to": INDEX_FORMAT_VERSION,
            "parameter_version": cfg.parameter_version(),
            "manifest_fingerprint": manifest_fingerprint,
            "mappings": mapping,
        }
    }
    atomic_write_json(os.path.join(target_dir, "upgrade_trail.json"), trail)
    return {"registry": registry, "trail": trail}
