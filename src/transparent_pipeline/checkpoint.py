"""Atomic, serialized stage snapshots and checkpoint compatibility checks."""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator

import numpy as np

from .errors import decode_error
from .util import canonical_json, sha256_json


@dataclass
class Snapshot:
    sequence: int
    scene_hash: str
    media_versions: dict[str, int]
    light_versions: dict[str, int]
    algorithm_version: str
    config_hash: str
    actual_step: float
    preview_error_bound: float
    transmission: np.ndarray
    scattering: np.ndarray
    path_units: np.ndarray
    optical_units: np.ndarray
    hit_mask: np.ndarray
    committed_mask: np.ndarray
    block_records: list[dict[str, Any]] = field(default_factory=list)
    commit_order: list[int] = field(default_factory=list)
    complete: bool = False
    width: int = 0
    height: int = 0
    band_count: int = 0
    block_width: int = 0
    block_height: int = 0
    render_mode: str = "full"
    timestamp_unix: float = 0.0

    @property
    def committed_count(self) -> int:
        return int(np.count_nonzero(self.committed_mask))

    def metadata(self) -> dict[str, Any]:
        return {
            "schema_version": "1.0",
            "sequence": self.sequence,
            "scene_hash": self.scene_hash,
            "media_versions": self.media_versions,
            "light_versions": self.light_versions,
            "algorithm_version": self.algorithm_version,
            "config_hash": self.config_hash,
            "actual_step": self.actual_step,
            "preview_error_bound": self.preview_error_bound,
            "block_records": self.block_records,
            "commit_order": self.commit_order,
            "complete": self.complete,
            "width": self.width,
            "height": self.height,
            "band_count": self.band_count,
            "block_width": self.block_width,
            "block_height": self.block_height,
            "render_mode": self.render_mode,
            "committed_count": self.committed_count,
            "timestamp_unix": self.timestamp_unix,
        }


class CheckpointStore:
    """One process-wide serialized writer with atomic replace semantics."""

    def __init__(self, directory: str | Path) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.checkpoint_path = self.directory / "checkpoint.npz"
        self.log_path = self.directory / "commit_log.jsonl"
        self.decision_path = self.directory / "resume_decision.json"
        self.decision_log_path = self.directory / "decision_log.jsonl"
        self.resource_versions_path = self.directory / "resource_versions.json"
        self._thread_lock = threading.RLock()
        self._file_lock_path = self.directory / ".checkpoint.lock"
        self._sequence = 0
        if self.checkpoint_path.exists():
            try:
                snapshot = self.load()
                self._sequence = snapshot.sequence
            except Exception:
                try:
                    self._sequence = max(
                        (int(event.get("sequence", 0)) for event in self.read_commit_log()),
                        default=0,
                    )
                except Exception:
                    self._sequence = 0
        elif self.log_path.exists():
            try:
                self._sequence = max(
                    (int(event.get("sequence", 0)) for event in self.read_commit_log()),
                    default=0,
                )
            except Exception:
                self._sequence = 0

    @contextlib.contextmanager
    def _locked(self) -> Iterator[None]:
        with self._thread_lock:
            handle = self._file_lock_path.open("a+b")
            try:
                try:
                    import fcntl

                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
                except (ImportError, OSError):
                    pass
                yield
            finally:
                try:
                    import fcntl

                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
                except (ImportError, OSError):
                    pass
                handle.close()

    def next_sequence(self) -> int:
        with self._thread_lock:
            self._sequence += 1
            return self._sequence

    def write(
        self,
        snapshot: Snapshot,
        *,
        commit_event: dict[str, Any] | None = None,
        assign_sequence: bool = True,
    ) -> Snapshot:
        with self._locked():
            if assign_sequence:
                snapshot.sequence = self.next_sequence()
            snapshot.timestamp_unix = time.time()
            metadata = snapshot.metadata()
            temp_path = self.directory / f".checkpoint-{os.getpid()}-{threading.get_ident()}.tmp"
            try:
                with temp_path.open("wb") as handle:
                    np.savez_compressed(
                        handle,
                        meta_json=np.asarray(canonical_json(metadata)),
                        transmission=snapshot.transmission,
                        scattering=snapshot.scattering,
                        path_units=snapshot.path_units,
                        optical_units=snapshot.optical_units,
                        hit_mask=snapshot.hit_mask,
                        committed_mask=snapshot.committed_mask,
                    )
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp_path, self.checkpoint_path)
                self._fsync_directory()
                self._write_resource_versions(snapshot)
            finally:
                if temp_path.exists():
                    temp_path.unlink(missing_ok=True)
            if commit_event is not None:
                event = dict(commit_event)
                event["sequence"] = snapshot.sequence
                event["timestamp_unix"] = snapshot.timestamp_unix
                self._append_log(event)
            return snapshot

    def _append_log(self, event: dict[str, Any]) -> None:
        encoded = canonical_json(event).encode("utf-8") + b"\n"
        with self.log_path.open("ab") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())

    def _fsync_directory(self) -> None:
        try:
            descriptor = os.open(self.directory, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(descriptor)
        except OSError:
            pass
        finally:
            os.close(descriptor)

    def load(self) -> Snapshot | None:
        if not self.checkpoint_path.exists():
            return None
        with self._locked():
            try:
                with np.load(self.checkpoint_path, allow_pickle=False) as archive:
                    metadata = json.loads(str(archive["meta_json"].item()))
                    snapshot = Snapshot(
                        sequence=int(metadata["sequence"]),
                        scene_hash=str(metadata["scene_hash"]),
                        media_versions={str(k): int(v) for k, v in metadata["media_versions"].items()},
                        light_versions={str(k): int(v) for k, v in metadata["light_versions"].items()},
                        algorithm_version=str(metadata["algorithm_version"]),
                        config_hash=str(metadata["config_hash"]),
                        actual_step=float(metadata["actual_step"]),
                        preview_error_bound=float(metadata["preview_error_bound"]),
                        transmission=np.array(archive["transmission"], dtype=np.float64, copy=True),
                        scattering=np.array(archive["scattering"], dtype=np.float64, copy=True),
                        path_units=np.array(archive["path_units"], dtype=np.int64, copy=True),
                        optical_units=np.array(archive["optical_units"], dtype=np.int64, copy=True),
                        hit_mask=np.array(archive["hit_mask"], dtype=np.bool_, copy=True),
                        committed_mask=np.array(archive["committed_mask"], dtype=np.bool_, copy=True),
                        block_records=list(metadata.get("block_records", [])),
                        commit_order=[int(value) for value in metadata.get("commit_order", [])],
                        complete=bool(metadata.get("complete", False)),
                        width=int(metadata.get("width", 0)),
                        height=int(metadata.get("height", 0)),
                        band_count=int(metadata.get("band_count", 0)),
                        block_width=int(metadata.get("block_width", 0)),
                        block_height=int(metadata.get("block_height", 0)),
                        render_mode=str(metadata.get("render_mode", "full")),
                        timestamp_unix=float(metadata.get("timestamp_unix", 0.0)),
                    )
            except Exception as exc:
                raise decode_error("checkpoint", f"cannot decode checkpoint {self.checkpoint_path}: {exc}") from exc
            self._validate_arrays(snapshot)
            self._sequence = max(self._sequence, snapshot.sequence)
            return snapshot

    @staticmethod
    def _validate_arrays(snapshot: Snapshot) -> None:
        expected_3d = (snapshot.height, snapshot.width, snapshot.band_count)
        if snapshot.transmission.shape != expected_3d:
            raise decode_error("checkpoint.transmission", "checkpoint buffer shape does not match metadata")
        for name, array in (
            ("scattering", snapshot.scattering),
            ("path_units", snapshot.path_units),
            ("optical_units", snapshot.optical_units),
        ):
            if array.shape != expected_3d:
                raise decode_error(f"checkpoint.{name}", "checkpoint buffer shape does not match metadata")
        if not np.all(np.isfinite(snapshot.transmission)) or not np.all(np.isfinite(snapshot.scattering)):
            raise decode_error("checkpoint.buffers", "checkpoint contains non-finite accumulated values")
        if snapshot.hit_mask.shape != (snapshot.height, snapshot.width):
            raise decode_error("checkpoint.hit_mask", "checkpoint hit mask shape does not match metadata")
        if snapshot.committed_mask.ndim != 1:
            raise decode_error("checkpoint.committed_mask", "committed mask must be one-dimensional")

    def compatibility_reasons(
        self,
        snapshot: Snapshot,
        *,
        scene_hash: str,
        media_versions: dict[str, int],
        light_versions: dict[str, int],
        algorithm_version: str,
        config_hash: str,
        width: int,
        height: int,
        band_count: int,
        block_width: int,
        block_height: int,
        block_count: int,
    ) -> list[str]:
        reasons: list[str] = []
        if snapshot.scene_hash != scene_hash:
            reasons.append("scene_hash")
        if snapshot.media_versions != media_versions:
            reasons.append("media_versions")
        if snapshot.light_versions != light_versions:
            reasons.append("light_versions")
        if snapshot.algorithm_version != algorithm_version:
            reasons.append("algorithm_version")
        if snapshot.config_hash != config_hash:
            reasons.append("config_hash")
        if (
            snapshot.width,
            snapshot.height,
            snapshot.band_count,
            snapshot.block_width,
            snapshot.block_height,
        ) != (width, height, band_count, block_width, block_height):
            reasons.append("render_layout")
        if snapshot.committed_mask.shape != (block_count,):
            reasons.append("committed_mask")
        return reasons

    def discard(self, reasons: list[str]) -> None:
        with self._locked():
            decision = {
                "action": "discard_full",
                "reasons": reasons,
                "previous_sequence": self._sequence,
                "at_unix": time.time(),
            }
            self._append_decision(decision)
            self._write_latest_decision(decision)
            if self.checkpoint_path.exists():
                rejected_dir = self.directory / "rejected"
                rejected_dir.mkdir(parents=True, exist_ok=True)
                target = rejected_dir / f"checkpoint-{self._sequence}-{int(time.time() * 1000)}.npz"
                os.replace(self.checkpoint_path, target)

    def read_commit_log(self) -> list[dict[str, Any]]:
        if not self.log_path.exists():
            return []
        events: list[dict[str, Any]] = []
        with self.log_path.open("r", encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                try:
                    events.append(json.loads(line))
                except json.JSONDecodeError as exc:
                    raise decode_error("checkpoint.commit_log", f"corrupt commit log line: {exc}") from exc
        return events

    def load_resource_versions(self) -> dict[str, Any] | None:
        if not self.resource_versions_path.exists():
            return None
        with self._locked():
            try:
                with self.resource_versions_path.open("r", encoding="utf-8") as handle:
                    value = json.load(handle)
            except (OSError, UnicodeError, json.JSONDecodeError) as exc:
                raise decode_error(
                    "checkpoint.resource_versions",
                    f"cannot decode resource version sidecar: {exc}",
                ) from exc
            if not isinstance(value, dict):
                raise decode_error("checkpoint.resource_versions", "sidecar must be an object")
            return value

    def _write_resource_versions(self, snapshot: Snapshot) -> None:
        from .util import write_json_atomic

        write_json_atomic(
            self.resource_versions_path,
            {
                "scene_hash": snapshot.scene_hash,
                "media_versions": snapshot.media_versions,
                "light_versions": snapshot.light_versions,
                "sequence": snapshot.sequence,
            },
        )

    def write_decision(self, decision: dict[str, Any]) -> None:
        self._append_decision(decision)
        self._write_latest_decision(decision)

    def _append_decision(self, decision: dict[str, Any]) -> None:
        encoded = canonical_json(decision).encode("utf-8") + b"\n"
        with self.decision_log_path.open("ab") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())

    def _write_latest_decision(self, decision: dict[str, Any]) -> None:
        temp = self.decision_path.with_name(f".{self.decision_path.name}.tmp")
        with temp.open("w", encoding="utf-8") as handle:
            handle.write(json.dumps(decision, indent=2, sort_keys=True, ensure_ascii=False) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, self.decision_path)


__all__ = ["CheckpointStore", "Snapshot"]
