"""Crash-safe, all-or-nothing snapshot checkpoints for committed segments."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
import threading
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np

from .audio_io import AudioData
from .config import (
    CHECKPOINT_SCHEMA_VERSION,
    PIPELINE_VERSION,
    RepairConfig,
    SessionConfig,
)
from .errors import io_error, state_error
from .metrics import output_sha256
from .segments import SegmentSpec


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def _fsync_directory(directory: Path) -> None:
    if os.name == "nt":
        return
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


@dataclass(frozen=True)
class SnapshotIdentity:
    audio_sha256: str
    sample_rate: int
    channels: int
    total_samples: int
    segment_samples: int
    total_segments: int
    schema_version: int = CHECKPOINT_SCHEMA_VERSION
    pipeline_version: str = PIPELINE_VERSION

    @classmethod
    def from_audio(
        cls, audio: AudioData, session: SessionConfig, total_segments: int
    ) -> "SnapshotIdentity":
        return cls(
            audio_sha256=audio.source_sha256,
            sample_rate=audio.sample_rate,
            channels=audio.channels,
            total_samples=audio.total_samples,
            segment_samples=session.segment_samples,
            total_segments=total_segments,
        )

    def canonical_dict(self) -> dict[str, Any]:
        return {
            "audio_sha256": self.audio_sha256,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "total_samples": self.total_samples,
            "segment_samples": self.segment_samples,
            "total_segments": self.total_segments,
            "schema_version": self.schema_version,
            "pipeline_version": self.pipeline_version,
        }

    @property
    def snapshot_id(self) -> str:
        payload = json.dumps(
            self.canonical_dict(), sort_keys=True, separators=(",", ":")
        ).encode("ascii")
        return hashlib.sha256(payload).hexdigest()


@dataclass(frozen=True)
class LoadInfo:
    state: str
    valid: bool
    loaded_count: int = 0
    reason: str = ""
    invalidated_to: str | None = None
    committed_indices: tuple[int, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "state": self.state,
            "valid": self.valid,
            "loaded_count": self.loaded_count,
            "reason": self.reason,
            "invalidated_to": self.invalidated_to,
            "committed_indices": list(self.committed_indices),
        }


@dataclass(frozen=True)
class LoadedSegment:
    spec: SegmentSpec
    output: np.ndarray
    config: RepairConfig
    config_fingerprint: str
    output_sha256: str
    metadata: dict[str, Any] = field(default_factory=dict)


class CheckpointStore:
    """One active snapshot per store root.

    Writes are serialized by an in-process lock.  A segment's atomic NPZ replace
    is the durability point; the manifest is an index and may lag by one commit
    after a hard crash.  Loading validates every sidecar before returning any
    segment, so a mixed-version snapshot is never partially consumed.
    """

    def __init__(
        self,
        root: str | os.PathLike[str],
        identity: SnapshotIdentity,
        initial_config: RepairConfig,
    ) -> None:
        self.root = Path(root)
        self.snapshot_dir = self.root / "snapshot"
        self.segments_dir = self.snapshot_dir / "segments"
        self.manifest_path = self.snapshot_dir / "manifest.json"
        self.identity = identity
        self.initial_config = initial_config.validate()
        self._lock = threading.RLock()

    def reset(self) -> None:
        with self._lock:
            if self.root.exists():
                shutil.rmtree(self.root)
            self.segments_dir.mkdir(parents=True, exist_ok=True)

    def inspect_and_load(self) -> tuple[LoadInfo, dict[int, LoadedSegment]]:
        """Validate the entire snapshot before returning any committed output."""

        with self._lock:
            if not self.manifest_path.exists():
                sidecars = list(self.segments_dir.glob("*.npz")) if self.segments_dir.exists() else []
                if sidecars:
                    try:
                        recovered: dict[int, LoadedSegment] = {}
                        for path in sorted(sidecars):
                            index = int(path.stem)
                            recovered[index] = self._read_segment(path, index)
                        manifest = self._manifest_payload(sorted(recovered))
                        self._atomic_write_json(self.manifest_path, manifest)
                        return (
                            LoadInfo(
                                state="recovered",
                                valid=True,
                                loaded_count=len(recovered),
                                reason="manifest rebuilt from durable segment sidecars",
                                committed_indices=tuple(sorted(recovered)),
                            ),
                            recovered,
                        )
                    except Exception as exc:  # noqa: BLE001
                        reason = f"manifest missing and sidecar validation failed: {exc}"
                        moved = self._invalidate(reason)
                        return (
                            LoadInfo(
                                state="invalidated",
                                valid=False,
                                reason=reason,
                                invalidated_to=moved,
                            ),
                            {},
                        )
                return LoadInfo(state="new", valid=True), {}
            try:
                manifest = self._read_json(self.manifest_path)
            except Exception as exc:  # noqa: BLE001
                moved = self._invalidate(f"manifest unreadable: {exc}")
                return (
                    LoadInfo(
                        state="invalidated",
                        valid=False,
                        reason=f"manifest unreadable: {exc}",
                        invalidated_to=moved,
                    ),
                    {},
                )
            reason = self._identity_mismatch(manifest)
            if reason:
                moved = self._invalidate(reason)
                return (
                    LoadInfo(
                        state="invalidated",
                        valid=False,
                        reason=reason,
                        invalidated_to=moved,
                    ),
                    {},
                )

            files = sorted(self.segments_dir.glob("*.npz")) if self.segments_dir.exists() else []
            manifest_indices = set(int(i) for i in manifest.get("committed_indices", []))
            try:
                file_indices = {int(path.stem) for path in files}
            except ValueError as exc:
                reason = f"snapshot contains an invalid segment filename: {exc}"
                moved = self._invalidate(reason)
                return (
                    LoadInfo(
                        state="invalidated",
                        valid=False,
                        reason=reason,
                        invalidated_to=moved,
                    ),
                    {},
                )
            missing = sorted(manifest_indices - file_indices)
            if missing:
                reason = f"manifest references missing committed segments: {missing}"
                moved = self._invalidate(reason)
                return (
                    LoadInfo(
                        state="invalidated",
                        valid=False,
                        reason=reason,
                        invalidated_to=moved,
                    ),
                    {},
                )

            loaded: dict[int, LoadedSegment] = {}
            try:
                for path in files:
                    index = int(path.stem)
                    segment = self._read_segment(path, index)
                    loaded[index] = segment
            except Exception as exc:  # noqa: BLE001
                reason = f"snapshot segment validation failed: {exc}"
                moved = self._invalidate(reason)
                return (
                    LoadInfo(
                        state="invalidated",
                        valid=False,
                        reason=reason,
                        invalidated_to=moved,
                    ),
                    {},
                )

            # A crash may have happened after the NPZ replace but before the
            # manifest index update.  Those sidecars are still durable commits.
            if set(loaded) != manifest_indices:
                manifest["committed_indices"] = sorted(loaded)
                manifest["updated_at"] = _utc_now()
                try:
                    self._atomic_write_json(self.manifest_path, manifest)
                except Exception as exc:  # noqa: BLE001
                    reason = f"unable to normalize manifest after recovery: {exc}"
                    moved = self._invalidate(reason)
                    return (
                        LoadInfo(
                            state="invalidated",
                            valid=False,
                            reason=reason,
                            invalidated_to=moved,
                        ),
                        {},
                    )
            return (
                LoadInfo(
                    state="loaded" if loaded else "empty",
                    valid=True,
                    loaded_count=len(loaded),
                    committed_indices=tuple(sorted(loaded)),
                ),
                loaded,
            )

    def create(self) -> None:
        with self._lock:
            if self.manifest_path.exists():
                raise state_error("snapshot manifest already exists")
            self.segments_dir.mkdir(parents=True, exist_ok=True)
            manifest = self._manifest_payload([])
            self._atomic_write_json(self.manifest_path, manifest)

    def _manifest_payload(self, committed_indices: list[int]) -> dict[str, Any]:
        return {
                **self.identity.canonical_dict(),
                "snapshot_id": self.identity.snapshot_id,
                "created_at": _utc_now(),
                "updated_at": _utc_now(),
                "committed_indices": sorted(committed_indices),
                "initial_config": self.initial_config.canonical_dict(),
            }

    def save_segment(
        self,
        spec: SegmentSpec,
        output: np.ndarray,
        config: RepairConfig,
        dsp_details: dict[str, Any] | None = None,
    ) -> str:
        """Persist one committed boundary. Caller serializes commit ordering."""

        with self._lock:
            array = np.asarray(output, dtype=np.float64)
            if array.ndim == 1:
                array = array[:, None]
            if array.shape != (spec.sample_count, self.identity.channels):
                raise state_error(
                    "refusing to checkpoint an invalid segment shape",
                    segment_index=spec.index,
                    expected=[spec.sample_count, self.identity.channels],
                    actual=list(array.shape),
                )
            if not np.all(np.isfinite(array)):
                raise state_error(
                    "refusing to checkpoint non-finite samples",
                    segment_index=spec.index,
                )
            digest = output_sha256(array)
            metadata = {
                "schema_version": self.identity.schema_version,
                "pipeline_version": self.identity.pipeline_version,
                "snapshot_id": self.identity.snapshot_id,
                "audio_sha256": self.identity.audio_sha256,
                "sample_rate": self.identity.sample_rate,
                "channels": self.identity.channels,
                "total_samples": self.identity.total_samples,
                "segment_samples": self.identity.segment_samples,
                "total_segments": self.identity.total_segments,
                "segment_index": spec.index,
                "start_sample": spec.start_sample,
                "end_sample": spec.end_sample,
                "sample_count": spec.sample_count,
                "config": config.canonical_dict(),
                "config_fingerprint": config.fingerprint(),
                "output_sha256": digest,
                "committed_at": _utc_now(),
                "dsp": dsp_details or {},
            }
            self.segments_dir.mkdir(parents=True, exist_ok=True)
            target = self.segments_dir / f"{spec.index:08d}.npz"
            fd, temp_name = tempfile.mkstemp(
                prefix=f".{spec.index:08d}.", suffix=".tmp", dir=str(self.segments_dir)
            )
            os.close(fd)
            temp = Path(temp_name)
            try:
                with temp.open("wb") as handle:
                    np.savez(
                        handle,
                        output=array,
                        meta_json=np.asarray(
                            json.dumps(metadata, sort_keys=True, separators=(",", ":"))
                        ),
                    )
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp, target)
                _fsync_directory(self.segments_dir)
            except Exception as exc:
                try:
                    temp.unlink(missing_ok=True)
                except OSError:
                    pass
                raise io_error(
                    "failed to persist segment checkpoint",
                    segment_index=spec.index,
                    reason=str(exc),
                ) from exc

            try:
                manifest = self._read_json(self.manifest_path)
                indices = set(int(i) for i in manifest.get("committed_indices", []))
                indices.add(spec.index)
                manifest["committed_indices"] = sorted(indices)
                manifest["updated_at"] = _utc_now()
                self._atomic_write_json(self.manifest_path, manifest)
            except Exception as exc:  # noqa: BLE001
                # The NPZ is already the durable commit point.  Leaving the
                # manifest stale is recoverable by sidecar scan on restart.
                if not target.exists():
                    raise io_error(
                        "segment checkpoint was not published",
                        segment_index=spec.index,
                        reason=str(exc),
                    ) from exc
                try:
                    existing = sorted(
                        int(path.stem) for path in self.segments_dir.glob("*.npz")
                    )
                    self._atomic_write_json(
                        self.manifest_path, self._manifest_payload(existing)
                    )
                except Exception:
                    # A restart can still rebuild the index by scanning the
                    # validated NPZ metadata.
                    pass
            return digest

    def load_segment(self, index: int) -> LoadedSegment:
        with self._lock:
            path = self.segments_dir / f"{index:08d}.npz"
            return self._read_segment(path, index)

    def load_all_committed(self) -> dict[int, LoadedSegment]:
        with self._lock:
            loaded: dict[int, LoadedSegment] = {}
            for path in sorted(self.segments_dir.glob("*.npz")):
                index = int(path.stem)
                loaded[index] = self._read_segment(path, index)
            return loaded

    def invalidate_current(self, reason: str) -> str | None:
        with self._lock:
            return self._invalidate(reason)

    def _read_segment(self, path: Path, expected_index: int) -> LoadedSegment:
        if not path.exists():
            raise state_error("checkpoint segment file is missing", segment_index=expected_index)
        try:
            with np.load(path, allow_pickle=False) as archive:
                output = np.array(archive["output"], dtype=np.float64, copy=True)
                metadata = json.loads(str(archive["meta_json"].item()))
        except Exception as exc:  # noqa: BLE001
            raise state_error(
                "checkpoint segment is corrupt",
                segment_index=expected_index,
                reason=str(exc),
            ) from exc
        required = (
            "schema_version",
            "pipeline_version",
            "snapshot_id",
            "audio_sha256",
            "sample_rate",
            "channels",
            "total_samples",
            "segment_samples",
            "total_segments",
            "segment_index",
            "start_sample",
            "end_sample",
            "sample_count",
            "config",
            "config_fingerprint",
            "output_sha256",
        )
        missing = [key for key in required if key not in metadata]
        if missing:
            raise state_error(
                "checkpoint metadata is incomplete",
                segment_index=expected_index,
                missing=missing,
            )
        expected = self.identity.canonical_dict()
        for key, value in expected.items():
            if metadata[key] != value:
                raise state_error(
                    "checkpoint metadata version mismatch",
                    segment_index=expected_index,
                    field=key,
                    expected=value,
                    actual=metadata[key],
                )
        if metadata["snapshot_id"] != self.identity.snapshot_id:
            raise state_error(
                "checkpoint snapshot id mismatch",
                segment_index=expected_index,
            )
        if int(metadata["segment_index"]) != expected_index:
            raise state_error(
                "checkpoint segment index mismatch",
                expected=expected_index,
                actual=metadata["segment_index"],
            )
        if int(metadata["start_sample"]) < 0 or int(metadata["end_sample"]) <= int(
            metadata["start_sample"]
        ):
            raise state_error(
                "checkpoint segment boundary is invalid",
                segment_index=expected_index,
            )
        if int(metadata["end_sample"]) > self.identity.total_samples:
            raise state_error(
                "checkpoint segment exceeds source timeline",
                segment_index=expected_index,
            )
        if output.ndim != 2 or output.shape != (
            int(metadata["sample_count"]),
            self.identity.channels,
        ):
            raise state_error(
                "checkpoint output shape mismatch",
                segment_index=expected_index,
                shape=list(output.shape),
            )
        if output_sha256(output) != metadata["output_sha256"]:
            raise state_error(
                "checkpoint output checksum mismatch",
                segment_index=expected_index,
            )
        config = RepairConfig(**metadata["config"]).validate()
        if config.fingerprint() != metadata["config_fingerprint"]:
            raise state_error(
                "checkpoint config fingerprint mismatch",
                segment_index=expected_index,
            )
        spec = SegmentSpec(
            index=expected_index,
            start_sample=int(metadata["start_sample"]),
            end_sample=int(metadata["end_sample"]),
        )
        return LoadedSegment(
            spec=spec,
            output=output,
            config=config,
            config_fingerprint=metadata["config_fingerprint"],
            output_sha256=metadata["output_sha256"],
            metadata=metadata,
        )

    def _identity_mismatch(self, manifest: dict[str, Any]) -> str:
        expected = self.identity.canonical_dict()
        missing = [key for key in expected if key not in manifest]
        if missing:
            return f"snapshot manifest is missing fields: {missing}"
        for key, value in expected.items():
            if manifest[key] != value:
                return f"snapshot {key} mismatch: expected {value!r}, got {manifest[key]!r}"
        if manifest.get("snapshot_id") != self.identity.snapshot_id:
            return "snapshot id mismatch"
        return ""

    def _invalidate(self, reason: str) -> str | None:
        if not self.snapshot_dir.exists():
            return None
        suffix = uuid.uuid4().hex[:8]
        target = self.root / f"invalidated-{datetime.now().strftime('%Y%m%d-%H%M%S')}-{suffix}"
        try:
            os.replace(self.snapshot_dir, target)
            _fsync_directory(self.root)
            try:
                (target / "INVALIDATION_REASON.txt").write_text(reason, encoding="utf-8")
            except OSError:
                pass
            return str(target)
        except OSError as exc:
            raise io_error(
                "snapshot version mismatch and invalidation failed",
                reason=reason,
                error=str(exc),
            ) from exc

    @staticmethod
    def _read_json(path: Path) -> dict[str, Any]:
        with path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
        if not isinstance(data, dict):
            raise ValueError("JSON root must be an object")
        return data

    @staticmethod
    def _atomic_write_json(path: Path, data: dict[str, Any]) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, temp_name = tempfile.mkstemp(
            prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
        )
        os.close(fd)
        temp = Path(temp_name)
        try:
            with temp.open("w", encoding="utf-8") as handle:
                json.dump(data, handle, indent=2, sort_keys=True, ensure_ascii=False)
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp, path)
            _fsync_directory(path.parent)
        except Exception:
            try:
                temp.unlink(missing_ok=True)
            except OSError:
                pass
            raise

