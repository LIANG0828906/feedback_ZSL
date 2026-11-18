"""Crash-safe whole-file snapshots with recoverable commit archives."""

from __future__ import annotations

import json
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .util import atomic_write_json, canonical_json, sha256_bytes, sha256_json

SNAPSHOT_SCHEMA = 1


@dataclass(slots=True)
class StateExpectation:
    state_dir: Path
    rule_hash: str
    input_digest: str
    codec: dict[str, int]
    chunk_frames: int
    total_frames: int
    total_blocks: int
    input_summary: list[dict[str, Any]]


@dataclass(slots=True)
class ResumeState:
    manager: SnapshotManager
    completed_blocks: int = 0
    completed_frames: int = 0
    sequence: int = 0
    source: str = "fresh"
    resumed_blocks: int = 0
    business_code: str | None = None
    snapshot_recovered: bool = False


class SnapshotManager:
    def __init__(self, expectation: StateExpectation) -> None:
        self.expectation = expectation
        self.state_dir = expectation.state_dir
        self.snapshot_path = self.state_dir / "snapshot.json"
        self.commits_dir = self.state_dir / "commits"
        self.pcm_path = self.state_dir / "render.pcm"
        self.sequence = 0
        self.completed_blocks = 0
        self.completed_frames = 0
        self.recovery_source = "fresh"
        self.resumed_blocks = 0
        self.business_code: str | None = None
        self.snapshot_recovered = False
        self._pcm_hasher = None

    def open(self) -> ResumeState:
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.commits_dir.mkdir(parents=True, exist_ok=True)
        candidates: list[tuple[Path, str]] = []
        if self.snapshot_path.exists():
            candidates.append((self.snapshot_path, "snapshot"))
        if self.commits_dir.exists():
            for path in sorted(
                self.commits_dir.glob("*.json"), reverse=True
            ):
                candidates.append((path, "commit"))

        current_corrupt = False
        mismatch_seen = False
        selected: dict[str, Any] | None = None
        selected_path: Path | None = None
        selected_kind: str | None = None
        valid_seen = False
        for path, kind in candidates:
            payload = _read_envelope(path)
            if payload is None:
                if kind == "snapshot":
                    current_corrupt = True
                continue
            valid_seen = True
            if not self._same_identity(payload):
                mismatch_seen = True
                if kind == "snapshot":
                    break
                continue
            if not self._pcm_prefix_valid(payload):
                if kind == "snapshot":
                    current_corrupt = True
                continue
            selected = payload
            selected_path = path
            selected_kind = kind
            break

        if mismatch_seen:
            self._discard()
            self.recovery_source = "restarted_after_mismatch"
            self.business_code = "E_RESUME_MISMATCH"
            return ResumeState(
                manager=self,
                source=self.recovery_source,
                business_code=self.business_code,
            )

        if selected is not None:
            committed_bytes = int(selected["pcm_bytes"])
            if self.pcm_path.exists():
                size = self.pcm_path.stat().st_size
                if size != committed_bytes:
                    with self.pcm_path.open("r+b") as handle:
                        handle.truncate(committed_bytes)
                        handle.flush()
                        os.fsync(handle.fileno())
            elif committed_bytes:
                # The selected record had a valid prefix at scan time, so this
                # can only happen if the file disappeared between checks.
                self._discard()
                self.recovery_source = "restarted_after_corrupt_state"
                return ResumeState(manager=self, source=self.recovery_source)

            self.sequence = int(selected.get("sequence", 0))
            self.completed_blocks = int(selected["completed_blocks"])
            self.completed_frames = int(selected["completed_frames"])
            self.resumed_blocks = self.completed_blocks
            self.snapshot_recovered = (
                current_corrupt
                or (
                    selected_kind == "commit"
                    and selected_path != self.snapshot_path
                )
            )
            if current_corrupt:
                self.recovery_source = "recovered_from_corrupt_snapshot"
            elif selected_kind == "commit" and selected_path != self.snapshot_path:
                self.recovery_source = "recovered_from_commit_archive"
            else:
                self.recovery_source = "resumed"
            # Re-establish a valid current snapshot immediately.  No manual
            # cleanup is needed after a damaged snapshot is detected.
            self._write_envelope(self.snapshot_path, selected)
            return ResumeState(
                manager=self,
                completed_blocks=self.completed_blocks,
                completed_frames=self.completed_frames,
                sequence=self.sequence,
                source=self.recovery_source,
                resumed_blocks=self.resumed_blocks,
                snapshot_recovered=self.snapshot_recovered,
            )

        if current_corrupt or valid_seen:
            self._discard()
            self.recovery_source = "restarted_after_corrupt_state"
        else:
            # A data file without any complete commit is an uncommitted tail.
            try:
                self.pcm_path.unlink()
            except FileNotFoundError:
                pass
            self.recovery_source = "fresh"
        return ResumeState(
            manager=self,
            source=self.recovery_source,
            snapshot_recovered=current_corrupt,
        )

    def _same_identity(self, payload: dict[str, Any]) -> bool:
        expected = self.expectation
        return (
            payload.get("rule_hash") == expected.rule_hash
            and payload.get("input_digest") == expected.input_digest
            and payload.get("codec") == expected.codec
            and payload.get("chunk_frames") == expected.chunk_frames
            and payload.get("total_frames") == expected.total_frames
            and payload.get("total_blocks") == expected.total_blocks
        )

    def _pcm_prefix_valid(self, payload: dict[str, Any]) -> bool:
        try:
            pcm_bytes = int(payload["pcm_bytes"])
            completed_frames = int(payload["completed_frames"])
            channels = int(self.expectation.codec["channels"])
            bit_depth = int(self.expectation.codec["bit_depth"])
            frame_bytes = channels * (bit_depth // 8)
            if pcm_bytes != completed_frames * frame_bytes:
                return False
            if pcm_bytes < 0 or pcm_bytes > self.expectation.total_frames * frame_bytes:
                return False
            if pcm_bytes == 0:
                return payload.get("pcm_sha256") == sha256_bytes(b"")
            if not self.pcm_path.exists():
                return False
            if self.pcm_path.stat().st_size < pcm_bytes:
                return False
            with self.pcm_path.open("rb") as handle:
                prefix = handle.read(pcm_bytes)
            return sha256_bytes(prefix) == payload.get("pcm_sha256")
        except (KeyError, TypeError, ValueError, OSError):
            return False

    def _discard(self) -> None:
        shutil.rmtree(self.state_dir, ignore_errors=True)
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.commits_dir.mkdir(parents=True, exist_ok=True)
        self.sequence = 0
        self.completed_blocks = 0
        self.completed_frames = 0

    def initialize_hasher(self, *, resume: bool) -> None:
        import hashlib

        digest = hashlib.sha256()
        if resume and self.pcm_path.exists() and self.completed_frames:
            frame_bytes = (
                int(self.expectation.codec["channels"])
                * (int(self.expectation.codec["bit_depth"]) // 8)
            )
            committed_bytes = self.completed_frames * frame_bytes
            with self.pcm_path.open("rb") as handle:
                remaining = committed_bytes
                while remaining:
                    chunk = handle.read(min(1024 * 1024, remaining))
                    if not chunk:
                        raise OSError("state PCM is shorter than the committed prefix")
                    digest.update(chunk)
                    remaining -= len(chunk)
        self._pcm_hasher = digest

    @property
    def pcm_sha256(self) -> str:
        if self._pcm_hasher is None:
            return sha256_bytes(b"")
        return self._pcm_hasher.hexdigest()

    def update_pcm_hash(self, chunk: bytes) -> None:
        if self._pcm_hasher is None:
            self.initialize_hasher(resume=False)
        self._pcm_hasher.update(chunk)

    def commit(
        self,
        *,
        completed_blocks: int,
        completed_frames: int,
        chapter_blocks: dict[str, int],
        chapter_frames: dict[str, int],
        status: str = "rendering",
    ) -> dict[str, Any]:
        self.sequence += 1
        frame_bytes = (
            int(self.expectation.codec["channels"])
            * (int(self.expectation.codec["bit_depth"]) // 8)
        )
        payload = {
            "schema": SNAPSHOT_SCHEMA,
            "sequence": self.sequence,
            "status": status,
            "rule_hash": self.expectation.rule_hash,
            "input_digest": self.expectation.input_digest,
            "input_summary": self.expectation.input_summary,
            "codec": self.expectation.codec,
            "chunk_frames": self.expectation.chunk_frames,
            "total_frames": self.expectation.total_frames,
            "total_blocks": self.expectation.total_blocks,
            "completed_blocks": completed_blocks,
            "completed_frames": completed_frames,
            "chapter_completed_blocks": chapter_blocks,
            "chapter_completed_frames": chapter_frames,
            "pcm_file": "render.pcm",
            "pcm_bytes": completed_frames * frame_bytes,
            "pcm_sha256": self.pcm_sha256,
        }
        archive = self.commits_dir / f"{self.sequence:012d}.json"
        self._write_envelope(archive, payload)
        self._write_envelope(self.snapshot_path, payload)
        self.completed_blocks = completed_blocks
        self.completed_frames = completed_frames
        return payload

    def _write_envelope(self, path: Path, payload: dict[str, Any]) -> None:
        envelope = {
            "checksum": sha256_json(payload),
            "payload": payload,
        }
        atomic_write_json(path, envelope)


def _read_envelope(path: Path) -> dict[str, Any] | None:
    try:
        data = path.read_bytes()
        envelope = json.loads(data)
        if not isinstance(envelope, dict):
            return None
        payload = envelope.get("payload")
        checksum = envelope.get("checksum")
        if not isinstance(payload, dict) or not isinstance(checksum, str):
            return None
        if sha256_json(payload) != checksum:
            return None
        required = {
            "sequence",
            "rule_hash",
            "input_digest",
            "codec",
            "chunk_frames",
            "total_frames",
            "total_blocks",
            "completed_blocks",
            "completed_frames",
            "pcm_bytes",
            "pcm_sha256",
        }
        if not required.issubset(payload):
            return None
        if not isinstance(payload["sequence"], int):
            return None
        if not isinstance(payload["input_summary"], list):
            return None
        if canonical_json(payload.get("codec")) == b"null":
            return None
        return payload
    except (OSError, ValueError, TypeError, json.JSONDecodeError):
        return None
