"""Crash-safe, serialized per-window checkpoint storage."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import PipelineError
from .frame import FrameResult
from .serialization import (
    InterProcessFileLock,
    atomic_write_bytes,
    bytes_sha256,
    canonical_sha256,
    pretty_json_bytes,
)
from .version import ALGORITHM_VERSION, CHECKPOINT_FORMAT


@dataclass(frozen=True)
class RestoredWindow:
    index: int
    start: int
    end_exclusive: int
    snapshot: dict[str, Any]
    frames: tuple[FrameResult, ...]
    content_hash: str
    commit_sequence: int


@dataclass(frozen=True)
class RecoveryResult:
    restored: dict[int, RestoredWindow]
    discarded: bool
    reasons: tuple[str, ...]
    content_hash: str | None = None


class CheckpointStore:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.manifest_path = self.root / "checkpoints.json"
        self.lock_path = self.root / ".checkpoint.lock"

    def _lock(self) -> InterProcessFileLock:
        return InterProcessFileLock(self.lock_path)

    def _read_manifest(self) -> dict[str, Any] | None:
        if not self.manifest_path.exists():
            return None
        try:
            return json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            return None

    def _validate_identity(
        self,
        manifest: dict[str, Any],
        identity: dict[str, Any],
    ) -> list[str]:
        reasons = []
        if manifest.get("format") != CHECKPOINT_FORMAT:
            reasons.append("checkpoint format mismatch")
        checks = (
            ("algorithm_version", ALGORITHM_VERSION),
            ("source_version", identity["source_version"]),
            ("scene_id", identity["scene_id"]),
        )
        for key, expected in checks:
            if manifest.get(key) != expected:
                reasons.append(
                    f"{key} mismatch: checkpoint={manifest.get(key)!r} expected={expected!r}"
                )
        return reasons

    def load(self, identity: dict[str, Any]) -> RecoveryResult:
        with self._lock():
            if not self.manifest_path.exists():
                return RecoveryResult({}, False, ())
            manifest = self._read_manifest()
            if manifest is None:
                return RecoveryResult(
                    {}, True, ("checkpoint manifest is unreadable",)
                )
            reasons = self._validate_identity(manifest, identity)
            if reasons:
                return RecoveryResult({}, True, tuple(reasons))
            expected_hash = manifest.get("content_hash")
            payload = {
                key: value
                for key, value in manifest.items()
                if key != "content_hash"
            }
            actual_hash = canonical_sha256(payload)
            if not expected_hash or expected_hash != actual_hash:
                return RecoveryResult(
                    {},
                    True,
                    ("checkpoint content hash mismatch",),
                )
            raw_windows = manifest.get("windows", {})
            if not isinstance(raw_windows, dict):
                return RecoveryResult(
                    {}, True, ("checkpoint windows are malformed",)
                )
            restored: dict[int, RestoredWindow] = {}
            try:
                for raw_index in sorted(raw_windows, key=lambda value: int(value)):
                    record = raw_windows[raw_index]
                    restored_window = self._restore_window(record)
                    if restored_window.index in restored:
                        raise ValueError("duplicate window index")
                    restored[restored_window.index] = restored_window
            except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
                return RecoveryResult(
                    {},
                    True,
                    (f"checkpoint window validation failed: {exc}",),
                )
            return RecoveryResult(restored, False, (), actual_hash)

    def _restore_window(self, record: dict[str, Any]) -> RestoredWindow:
        index = int(record["index"])
        start = int(record["start"])
        end_exclusive = int(record["end_exclusive"])
        snapshot = record["snapshot"]
        source_version = record["source_version"]
        algorithm_version = record["algorithm_version"]
        if algorithm_version != ALGORITHM_VERSION:
            raise ValueError("window algorithm version mismatch")
        if snapshot.get("source_version") != source_version:
            raise ValueError("window source version mismatch")
        frames_record = record["frames"]
        if not isinstance(frames_record, list) or not frames_record:
            raise ValueError("window contains no frames")
        file_path = self.root / record["file"]
        data = file_path.read_bytes()
        frames: list[FrameResult] = []
        cursor = 0
        for frame_record in frames_record:
            offset = int(frame_record["offset"])
            size = int(frame_record["size"])
            if offset != cursor or size <= 0 or offset + size > len(data):
                raise ValueError("invalid frame byte range")
            raw = data[offset : offset + size]
            if bytes_sha256(raw) != frame_record["pixel_sha256"]:
                raise ValueError("frame pixel hash mismatch")
            stats = frame_record["stats"]
            expected_channels = int(stats["channels"])
            expected_width = int(stats["width"])
            expected_height = int(stats["height"])
            expected_size = expected_width * expected_height * expected_channels
            if size != expected_size:
                raise ValueError("frame byte size does not match dimensions")
            pixels = np.frombuffer(raw, dtype=np.uint8).reshape(
                expected_height, expected_width, expected_channels
            )
            frame = FrameResult.from_pixels(pixels.copy(), stats)
            if frame.content_hash != stats["content_hash"]:
                raise ValueError("frame content hash mismatch")
            if frame.frame_index < start or frame.frame_index >= end_exclusive:
                raise ValueError("frame lies outside its window")
            frames.append(frame)
            cursor += size
        if cursor != len(data):
            raise ValueError("window file contains trailing bytes")
        expected_frame_count = end_exclusive - start
        if len(frames) != expected_frame_count:
            raise ValueError("window frame count mismatch")
        if [frame.frame_index for frame in frames] != list(range(start, end_exclusive)):
            raise ValueError("window frames are not contiguous")
        window_content_hash = record["content_hash"]
        payload = {
            key: value for key, value in record.items() if key != "content_hash"
        }
        if canonical_sha256(payload) != window_content_hash:
            raise ValueError("window content hash mismatch")
        return RestoredWindow(
            index=index,
            start=start,
            end_exclusive=end_exclusive,
            snapshot=snapshot,
            frames=tuple(frames),
            content_hash=window_content_hash,
            commit_sequence=int(record["commit_sequence"]),
        )

    def commit_window(
        self,
        *,
        identity: dict[str, Any],
        index: int,
        start: int,
        end_exclusive: int,
        snapshot: dict[str, Any],
        frames: tuple[FrameResult, ...],
        source_version: str,
    ) -> str:
        if not frames:
            raise ValueError("cannot checkpoint an empty window")
        with self._lock():
            manifest = self._read_manifest()
            if manifest is None:
                manifest = {
                    "format": CHECKPOINT_FORMAT,
                    "algorithm_version": ALGORITHM_VERSION,
                    "source_version": identity["source_version"],
                    "model_fingerprint": identity["model_fingerprint"],
                    "scene_id": identity["scene_id"],
                    "next_commit_sequence": 1,
                    "windows": {},
                }
            reasons = self._validate_identity(manifest, identity)
            if reasons:
                raise ValueError("checkpoint identity mismatch: " + "; ".join(reasons))
            frame_bytes = bytearray()
            frame_records = []
            for frame in frames:
                raw = frame.pixel_bytes
                offset = len(frame_bytes)
                frame_bytes.extend(raw)
                frame_records.append(
                    {
                        "frame": frame.frame_index,
                        "offset": offset,
                        "size": len(raw),
                        "pixel_sha256": bytes_sha256(raw),
                        "stats": frame.stats_dict(),
                    }
                )
            file_name = f"window_{index:04d}.bin"
            file_path = self.root / file_name
            atomic_write_bytes(file_path, bytes(frame_bytes))
            sequence = int(manifest.get("next_commit_sequence", 1))
            record: dict[str, Any] = {
                "index": index,
                "start": start,
                "end_exclusive": end_exclusive,
                "commit_sequence": sequence,
                "snapshot": snapshot,
                "frames": frame_records,
                "source_version": source_version,
                "algorithm_version": ALGORITHM_VERSION,
                "file": file_name,
            }
            record["content_hash"] = canonical_sha256(record)
            manifest.setdefault("windows", {})[str(index)] = record
            manifest["next_commit_sequence"] = sequence + 1
            payload = {
                key: value
                for key, value in manifest.items()
                if key != "content_hash"
            }
            manifest["content_hash"] = canonical_sha256(payload)
            atomic_write_bytes(
                self.manifest_path, pretty_json_bytes(manifest)
            )
            return record["content_hash"]

    def discard(self) -> None:
        with self._lock():
            for path in list(self.root.glob("window_*.bin")):
                try:
                    path.unlink()
                except OSError:
                    pass
            try:
                self.manifest_path.unlink(missing_ok=True)
            except OSError:
                pass
