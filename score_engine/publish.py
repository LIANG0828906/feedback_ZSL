from __future__ import annotations

import json
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import version_stale
from .solver import sequence_content_hash
from .util import atomic_write_json, canonical_json_bytes


@dataclass(frozen=True)
class PublishedSnapshot:
    version: int
    content_hash: str
    score_hash: str
    payload_bytes: bytes

    def payload(self) -> dict[str, Any]:
        return json.loads(self.payload_bytes)

    def notes(self) -> tuple[dict[str, Any], ...]:
        payload = self.payload()
        return tuple(
            note
            for part in payload.get("parts", [])
            for note in part.get("notes", [])
        )

    def part_ids(self) -> tuple[str, ...]:
        return tuple(part["part_id"] for part in self.payload().get("parts", []))


class VersionedPublisher:
    """Publishes complete performance sequences with an atomic reference swap."""

    def __init__(self, manifest_directory: str | Path | None = None) -> None:
        self._lock = threading.RLock()
        self._snapshot: PublishedSnapshot | None = None
        self._timeline: list[dict[str, Any]] = []
        self._manifest_directory = Path(manifest_directory) if manifest_directory else None
        if self._manifest_directory:
            self._manifest_directory.mkdir(parents=True, exist_ok=True)

    @property
    def current_version(self) -> int:
        with self._lock:
            return 0 if self._snapshot is None else self._snapshot.version

    @property
    def current_hash(self) -> str | None:
        with self._lock:
            return None if self._snapshot is None else self._snapshot.content_hash

    def publish(
        self,
        sequence: dict[str, Any],
        *,
        version: int | None = None,
        expected_current_hash: str | None = None,
    ) -> PublishedSnapshot:
        content_hash = sequence_content_hash(sequence)
        normalised = dict(sequence)
        normalised["content_hash"] = content_hash
        payload_bytes = canonical_json_bytes(normalised)
        packaged_bytes = payload_bytes + b"\n"
        with self._lock:
            if expected_current_hash is not None and self.current_hash != expected_current_hash:
                raise version_stale(
                    "当前发布内容哈希与调用方预期不一致",
                    expected=expected_current_hash,
                    actual=self.current_hash or "",
                )
            published_version = version if version is not None else self.current_version + 1
            snapshot = PublishedSnapshot(
                version=published_version,
                content_hash=content_hash,
                score_hash=str(normalised.get("score_hash", "")),
                payload_bytes=packaged_bytes,
            )
            manifest = {
                "schema": "publish-manifest-v1",
                "sequence_version": snapshot.version,
                "content_hash": snapshot.content_hash,
                "score_hash": snapshot.score_hash,
                "source_version": normalised.get("source_version"),
                "parameters": normalised.get("parameters", {}),
                "part_ids": [part.get("part_id") for part in normalised.get("parts", [])],
                "note_count": sum(len(part.get("notes", [])) for part in normalised.get("parts", [])),
                "payload_bytes": len(packaged_bytes),
            }
            if self._snapshot is not None and snapshot.version <= self._snapshot.version:
                raise version_stale(
                    "发布版本号必须递增",
                    expected=f">{self._snapshot.version}",
                    actual=str(snapshot.version),
                )
            if self._manifest_directory is not None:
                manifest_path = self._manifest_directory / f"manifest-v{snapshot.version:06d}.json"
                atomic_write_json(manifest_path, manifest)
                latest_path = self._manifest_directory / "latest-manifest.json"
                atomic_write_json(latest_path, manifest)
            self._snapshot = snapshot
            self._timeline.append(manifest)
            return snapshot

    def get_snapshot(
        self,
        *,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> PublishedSnapshot:
        with self._lock:
            snapshot = self._snapshot
            if snapshot is None:
                raise version_stale(
                    "尚无已发布的表现序列",
                    expected=expected_hash or f"version:{expected_version}",
                    actual="none",
                )
            if expected_version is not None and snapshot.version != expected_version:
                raise version_stale(
                    "发布版本号不匹配",
                    expected=f"version:{expected_version}",
                    actual=f"version:{snapshot.version}",
                )
            if expected_hash is not None and snapshot.content_hash != expected_hash:
                raise version_stale(
                    "发布内容哈希不匹配",
                    expected=expected_hash,
                    actual=snapshot.content_hash,
                )
            return snapshot

    def notes_at(
        self,
        *,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> tuple[dict[str, Any], ...]:
        return self.get_snapshot(
            expected_version=expected_version,
            expected_hash=expected_hash,
        ).notes()

    def timeline(self) -> tuple[dict[str, Any], ...]:
        with self._lock:
            return tuple(dict(item) for item in self._timeline)

