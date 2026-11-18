from __future__ import annotations

import hashlib
import json
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import (
    EXIT_SNAPSHOT_CORRUPT,
    BusinessError,
)


SNAPSHOT_FORMAT = "theatre-binaural-snapshot"
SNAPSHOT_VERSION = 1


@dataclass
class SnapshotProgress:
    payload: dict[str, Any]
    completed_blocks: int
    total_blocks: int
    total_frames: int
    block_frames: int
    project_digest: str
    leakage_digest: str
    pan_digest: str
    blocks: list[dict[str, Any]]
    contribution_energy: list[float]
    leakage_estimates: list[dict[str, Any]]
    pan_summary: dict[str, Any]


def _canonical_json(payload: dict[str, Any]) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _snapshot_checksum(payload: dict[str, Any]) -> str:
    body = {key: value for key, value in payload.items() if key != "checksum"}
    return hashlib.sha256(_canonical_json(body)).hexdigest()


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


class SnapshotStore:
    def __init__(self, state_dir: str | Path) -> None:
        self.state_dir = Path(state_dir)
        self.snapshot_path = self.state_dir / "snapshot.json"
        self.blocks_dir = self.state_dir / "blocks"

    def has_snapshot(self) -> bool:
        return self.snapshot_path.is_file()

    def _corrupt(self, message: str, details: dict[str, Any] | None = None) -> BusinessError:
        return BusinessError(
            "E_SNAPSHOT_CORRUPT",
            "snapshot",
            message,
            EXIT_SNAPSHOT_CORRUPT,
            details or {},
        )

    def load(self) -> SnapshotProgress | None:
        if not self.snapshot_path.is_file():
            return None
        try:
            raw = self.snapshot_path.read_bytes()
            payload = json.loads(raw.decode("utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise self._corrupt(f"snapshot JSON is unreadable: {exc}") from exc
        if not isinstance(payload, dict):
            raise self._corrupt("snapshot JSON root must be an object")
        if payload.get("format") != SNAPSHOT_FORMAT:
            raise self._corrupt("snapshot format marker is invalid")
        if payload.get("version") != SNAPSHOT_VERSION:
            raise self._corrupt("unsupported snapshot version")
        checksum = payload.get("checksum")
        if not isinstance(checksum, str) or checksum != _snapshot_checksum(payload):
            raise self._corrupt("snapshot checksum mismatch")
        required = (
            "completed_blocks",
            "total_blocks",
            "total_frames",
            "block_frames",
            "project_digest",
            "leakage_digest",
            "pan_digest",
            "blocks",
            "contribution_energy",
            "leakage_estimates",
            "pan_summary",
        )
        missing = [key for key in required if key not in payload]
        if missing:
            raise self._corrupt(
                "snapshot fields are missing",
                {"missing_fields": missing},
            )
        numeric_fields = ("completed_blocks", "total_blocks", "total_frames", "block_frames")
        for field in numeric_fields:
            if isinstance(payload[field], bool) or not isinstance(payload[field], int):
                raise self._corrupt("snapshot numeric field is invalid", {"field": field})
        if (
            payload["total_blocks"] <= 0
            or payload["total_frames"] <= 0
            or payload["block_frames"] <= 0
        ):
            raise self._corrupt("snapshot block geometry is invalid")
        if not isinstance(payload["pan_summary"], dict):
            raise self._corrupt("snapshot pan summary must be an object")
        blocks = payload["blocks"]
        if not isinstance(blocks, list):
            raise self._corrupt("snapshot blocks must be an array")
        completed = payload["completed_blocks"]
        if not isinstance(completed, int) or completed < 0 or completed != len(blocks):
            raise self._corrupt("snapshot completed_blocks is inconsistent")
        if completed > payload["total_blocks"]:
            raise self._corrupt("snapshot completed_blocks exceeds total_blocks")
        expected_frames = 0
        for index, entry in enumerate(blocks):
            if not isinstance(entry, dict):
                raise self._corrupt("snapshot block entry is invalid")
            if entry.get("index") != index:
                raise self._corrupt(
                    "snapshot block indexes are not contiguous",
                    {"expected_block": index, "actual_block": entry.get("index")},
                )
            frames = entry.get("frames")
            channels = entry.get("channels")
            if not isinstance(frames, int) or frames <= 0 or channels != 2:
                raise self._corrupt("snapshot block shape is invalid", {"block": index})
            if index < payload["total_blocks"] - 1 and frames != payload["block_frames"]:
                raise self._corrupt(
                    "snapshot intermediate block has the wrong frame count",
                    {"block": index},
                )
            expected_frames += frames
            file_name = entry.get("file")
            digest = entry.get("sha256")
            if not isinstance(file_name, str) or not isinstance(digest, str):
                raise self._corrupt("snapshot block reference is invalid", {"block": index})
            block_path = self.state_dir / file_name
            try:
                block_path.resolve().relative_to(self.state_dir.resolve())
            except ValueError as exc:
                raise self._corrupt("snapshot block path escapes state directory", {"block": index}) from exc
            if not block_path.is_file():
                raise self._corrupt("snapshot block file is missing", {"block": index})
            raw_block = block_path.read_bytes()
            if _sha256_bytes(raw_block) != digest:
                raise self._corrupt("snapshot block checksum mismatch", {"block": index})
            expected_bytes = frames * channels * 8
            if len(raw_block) != expected_bytes:
                raise self._corrupt("snapshot block size mismatch", {"block": index})
        if expected_frames > payload["total_frames"]:
            raise self._corrupt("snapshot blocks exceed the declared bus length")
        contributions = payload["contribution_energy"]
        if not isinstance(contributions, list) or len(contributions) < 1:
            raise self._corrupt("snapshot contribution energy is invalid")
        if any(not isinstance(value, (int, float)) for value in contributions):
            raise self._corrupt("snapshot contribution energy values are invalid")
        self._remove_unreferenced_files(blocks)
        return SnapshotProgress(
            payload=payload,
            completed_blocks=completed,
            total_blocks=int(payload["total_blocks"]),
            total_frames=int(payload["total_frames"]),
            block_frames=int(payload["block_frames"]),
            project_digest=str(payload["project_digest"]),
            leakage_digest=str(payload["leakage_digest"]),
            pan_digest=str(payload["pan_digest"]),
            blocks=blocks,
            contribution_energy=[float(value) for value in contributions],
            leakage_estimates=list(payload["leakage_estimates"]),
            pan_summary=dict(payload["pan_summary"]),
        )

    def _remove_unreferenced_files(self, blocks: list[dict[str, Any]]) -> None:
        if not self.blocks_dir.is_dir():
            return
        referenced = {
            (self.state_dir / str(entry["file"])).resolve()
            for entry in blocks
        }
        for path in self.blocks_dir.iterdir():
            if path.is_file() and path.resolve() not in referenced:
                try:
                    path.unlink()
                except OSError:
                    pass

    def read_blocks(self, progress: SnapshotProgress) -> list[np.ndarray]:
        result: list[np.ndarray] = []
        for entry in progress.blocks:
            block_path = self.state_dir / str(entry["file"])
            raw = block_path.read_bytes()
            array = np.frombuffer(raw, dtype="<f8").reshape(
                int(entry["frames"]), int(entry["channels"])
            )
            result.append(array.astype(np.float64, copy=True))
        return result

    def partial_mix(self, progress: SnapshotProgress) -> np.ndarray:
        blocks = self.read_blocks(progress)
        if not blocks:
            return np.empty((0, 2), dtype=np.float64)
        return np.concatenate(blocks, axis=0)

    def _write_block_file(self, block_index: int, block: np.ndarray) -> tuple[str, str]:
        self.blocks_dir.mkdir(parents=True, exist_ok=True)
        relative = f"blocks/block_{block_index:06d}.f64"
        final_path = self.state_dir / relative
        temp_path = final_path.with_name(
            f".{final_path.name}.tmp-{os.getpid()}"
        )
        raw = np.asarray(block, dtype="<f8").tobytes(order="C")
        with temp_path.open("wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, final_path)
        _fsync_directory(self.blocks_dir)
        return relative, _sha256_bytes(raw)

    def commit_block(
        self,
        *,
        block_index: int,
        block: np.ndarray,
        total_blocks: int,
        total_frames: int,
        block_frames: int,
        project_digest: str,
        leakage_digest: str,
        pan_digest: str,
        contribution_energy: list[float],
        leakage_estimates: list[dict[str, Any]],
        pan_summary: dict[str, Any],
        previous_blocks: list[dict[str, Any]],
    ) -> SnapshotProgress:
        if block_index != len(previous_blocks):
            raise BusinessError(
                "E_SNAPSHOT_ORDER",
                "snapshot",
                "block commit order is not contiguous",
                EXIT_SNAPSHOT_CORRUPT,
            )
        relative, digest = self._write_block_file(block_index, block)
        entry = {
            "index": block_index,
            "frames": int(block.shape[0]),
            "channels": int(block.shape[1]),
            "file": relative,
            "sha256": digest,
        }
        blocks = [*previous_blocks, entry]
        payload: dict[str, Any] = {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "completed_blocks": len(blocks),
            "total_blocks": int(total_blocks),
            "total_frames": int(total_frames),
            "block_frames": int(block_frames),
            "project_digest": project_digest,
            "leakage_digest": leakage_digest,
            "pan_digest": pan_digest,
            "blocks": blocks,
            "contribution_energy": [float(value) for value in contribution_energy],
            "leakage_estimates": leakage_estimates,
            "pan_summary": pan_summary,
        }
        payload["checksum"] = _snapshot_checksum(payload)
        self.state_dir.mkdir(parents=True, exist_ok=True)
        temp_path = self.snapshot_path.with_name(
            f".snapshot.json.tmp-{os.getpid()}"
        )
        encoded = (
            json.dumps(payload, ensure_ascii=False, indent=2) + "\n"
        ).encode("utf-8")
        with temp_path.open("wb") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, self.snapshot_path)
        _fsync_directory(self.state_dir)
        return SnapshotProgress(
            payload=payload,
            completed_blocks=len(blocks),
            total_blocks=int(total_blocks),
            total_frames=int(total_frames),
            block_frames=int(block_frames),
            project_digest=project_digest,
            leakage_digest=leakage_digest,
            pan_digest=pan_digest,
            blocks=blocks,
            contribution_energy=[float(value) for value in contribution_energy],
            leakage_estimates=leakage_estimates,
            pan_summary=pan_summary,
        )

    def clear(self) -> None:
        try:
            self.snapshot_path.unlink()
        except FileNotFoundError:
            pass
        if self.blocks_dir.is_dir():
            shutil.rmtree(self.blocks_dir, ignore_errors=True)
        if self.state_dir.is_dir():
            for path in self.state_dir.glob(".snapshot.json.tmp-*"):
                try:
                    path.unlink()
                except OSError:
                    pass

