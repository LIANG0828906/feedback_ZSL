from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .wavio import atomic_write_bytes


SNAPSHOT_SCHEMA = "uid211.mastering.snapshot.v1"
RECOVERY_SCHEMA = "uid211.mastering.recovery.v1"
_SNAPSHOT_RE = re.compile(r"^snapshot_v(\d{8})\.json$")


def canonical_json_bytes(payload: Any) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=True,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def stable_json_bytes(payload: dict[str, Any]) -> bytes:
    return (
        json.dumps(payload, ensure_ascii=False, indent=2, allow_nan=False) + "\n"
    ).encode("utf-8")


def _snapshot_body(snapshot: dict[str, Any]) -> dict[str, Any]:
    body = dict(snapshot)
    body.pop("integrity", None)
    return body


def _snapshot_integrity(snapshot: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(_snapshot_body(snapshot))).hexdigest()


@dataclass(frozen=True)
class StateLoad:
    snapshot: dict[str, Any] | None
    discarded_versions: tuple[Any, ...]
    resumed: bool


class StateStore:
    def __init__(self, output_dir: Path, state_dir: Path | None = None):
        self.output_dir = output_dir.resolve()
        self.root = (state_dir or (self.output_dir / ".mastering-state")).resolve()
        self.work_dir = self.root / "work"

    def _snapshot_files(self) -> list[tuple[int, Path]]:
        if not self.root.is_dir():
            return []
        found: list[tuple[int, Path]] = []
        for path in self.root.iterdir():
            match = _SNAPSHOT_RE.match(path.name)
            if match:
                found.append((int(match.group(1)), path))
        found.sort(key=lambda item: item[0])
        return found

    def _discard(self, versions: list[Any]) -> None:
        if self.root.exists():
            shutil.rmtree(self.root)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        self.discarded = tuple(versions)

    def _read_snapshot(self, path: Path, file_version: int) -> dict[str, Any]:
        raw = path.read_bytes()
        snapshot = json.loads(raw.decode("utf-8"))
        if not isinstance(snapshot, dict):
            raise ValueError("snapshot must be an object")
        if snapshot.get("schema") != SNAPSHOT_SCHEMA:
            raise ValueError("unknown snapshot schema")
        if snapshot.get("version") != file_version:
            raise ValueError("snapshot version does not match its filename")
        if snapshot.get("integrity") != _snapshot_integrity(snapshot):
            raise ValueError("snapshot integrity check failed")
        return snapshot

    def load(
        self,
        project_digest: str,
        block_frames: int,
        total_frames: int,
        languages: list[str],
    ) -> StateLoad:
        self.discarded: tuple[Any, ...] = tuple()
        files = self._snapshot_files()
        if not files:
            return StateLoad(None, tuple(), False)
        version, path = files[-1]
        try:
            snapshot = self._read_snapshot(path, version)
            self._validate_snapshot(snapshot, project_digest, block_frames, total_frames, languages)
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as exc:
            del exc
            self._discard([item_version for item_version, _ in files])
            return StateLoad(None, self.discarded, False)
        return StateLoad(snapshot, tuple(), True)

    def _validate_snapshot(
        self,
        snapshot: dict[str, Any],
        project_digest: str,
        block_frames: int,
        total_frames: int,
        languages: list[str],
    ) -> None:
        if snapshot.get("project_digest") != project_digest:
            raise ValueError("project digest differs")
        if snapshot.get("block_frames") != block_frames:
            raise ValueError("block size differs")
        if snapshot.get("total_frames") != total_frames:
            raise ValueError("frame count differs")
        total_blocks = (total_frames + block_frames - 1) // block_frames
        if snapshot.get("total_blocks") != total_blocks:
            raise ValueError("block count differs")
        completed = snapshot.get("completed_blocks")
        if not isinstance(completed, int) or not 0 <= completed <= total_blocks:
            raise ValueError("invalid completed block count")
        if not isinstance(snapshot.get("recovery_count"), int) or snapshot["recovery_count"] < 0:
            raise ValueError("invalid recovery count")
        if not isinstance(snapshot.get("discarded_snapshot_versions"), list):
            raise ValueError("invalid discarded version list")
        analysis = snapshot.get("analysis")
        if not isinstance(analysis, dict) or not isinstance(analysis.get("languages"), dict):
            raise ValueError("analysis parameters are missing")
        if sorted(analysis["languages"]) != sorted(languages):
            raise ValueError("analysis language set differs")
        for language in languages:
            params = analysis["languages"][language]
            if not isinstance(params, dict):
                raise ValueError(f"analysis for {language} is invalid")
            for field in ("voice_gain_db", "music_gain_db", "sfx_gain_db", "program_gain_db"):
                value = params.get(field)
                if isinstance(value, bool) or not isinstance(value, (int, float)):
                    raise ValueError(f"analysis field {field} is invalid")
            if not isinstance(params.get("ducking"), dict):
                raise ValueError(f"ducking parameters for {language} are missing")
        stage_files = snapshot.get("stage_files")
        if not isinstance(stage_files, dict) or sorted(stage_files) != sorted(languages):
            raise ValueError("stage file map differs")
        for language in languages:
            entry = stage_files[language]
            if not isinstance(entry, dict):
                raise ValueError("invalid stage file entry")
            name = entry.get("name")
            committed = entry.get("committed_bytes")
            total_bytes = entry.get("total_bytes")
            if not isinstance(name, str) or Path(name).is_absolute() or ".." in Path(name).parts:
                raise ValueError("invalid stage file name")
            if not isinstance(committed, int) or not isinstance(total_bytes, int):
                raise ValueError("invalid stage file sizes")
            if not 0 <= committed <= total_bytes:
                raise ValueError("invalid committed byte count")
            header_sha256 = entry.get("header_sha256")
            if not isinstance(header_sha256, str):
                raise ValueError("invalid stage header checksum")
            path = self.root / name
            if not path.is_file() or path.stat().st_size < committed:
                raise ValueError("stage file is missing or shorter than committed prefix")
            with path.open("rb") as handle:
                header = handle.read(44)
            if hashlib.sha256(header).hexdigest() != header_sha256:
                raise ValueError("stage WAV header checksum failed")

    def prepare_resume(self, snapshot: dict[str, Any], languages: list[str]) -> None:
        self.work_dir.mkdir(parents=True, exist_ok=True)
        for language in languages:
            entry = snapshot["stage_files"][language]
            path = self.root / entry["name"]
            with path.open("r+b") as handle:
                handle.truncate(entry["committed_bytes"])
                handle.flush()
                os.fsync(handle.fileno())

    def initial_snapshot(
        self,
        project_digest: str,
        block_frames: int,
        total_frames: int,
        analysis: dict[str, Any],
        stage_files: dict[str, dict[str, Any]],
        discarded_versions: list[Any],
    ) -> dict[str, Any]:
        total_blocks = (total_frames + block_frames - 1) // block_frames
        snapshot = {
            "schema": SNAPSHOT_SCHEMA,
            "version": 1,
            "project_digest": project_digest,
            "block_frames": block_frames,
            "total_frames": total_frames,
            "total_blocks": total_blocks,
            "completed_blocks": 0,
            "recovery_count": 0,
            "discarded_snapshot_versions": list(discarded_versions),
            "analysis": analysis,
            "stage_files": stage_files,
        }
        return snapshot

    def commit(self, snapshot: dict[str, Any]) -> dict[str, Any]:
        version = int(snapshot["version"])
        body = dict(snapshot)
        body["integrity"] = _snapshot_integrity(body)
        path = self.root / f"snapshot_v{version:08d}.json"
        self.root.mkdir(parents=True, exist_ok=True)
        atomic_write_bytes(path, stable_json_bytes(body))
        for old_version, old_path in self._snapshot_files():
            if old_version < version:
                try:
                    old_path.unlink()
                except FileNotFoundError:
                    pass
        return body

    def cleanup(self) -> None:
        if self.root.exists():
            shutil.rmtree(self.root)

    def stage_path(self, relative_name: str) -> Path:
        return self.root / relative_name

    def write_recovery_report(self, payload: dict[str, Any]) -> None:
        report = {
            "schema": RECOVERY_SCHEMA,
            "status": payload["status"],
            "recovery_count": payload["recovery_count"],
            "discarded_snapshot_versions": payload["discarded_snapshot_versions"],
            "completed_blocks": payload["completed_blocks"],
            "total_blocks": payload["total_blocks"],
            "total_frames": payload["total_frames"],
            "outputs": payload.get("outputs", []),
        }
        atomic_write_bytes(self.output_dir / "recovery_report.json", stable_json_bytes(report))

