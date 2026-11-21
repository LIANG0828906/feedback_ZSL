"""Per-channel atomic checkpoints and validated crash recovery."""

from __future__ import annotations

import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Sequence

from .errors import CalibrationError, E_RECOVERY_INVALID
from .hashing import sha256_obj
from .models import BandSpec, CalibrationParams, ChannelComputation
from .storage import atomic_write_json, read_json

GENESIS_HASH = "GENESIS"
REQUIRED_MANIFEST_FIELDS = {
    "format_version",
    "run_id",
    "created_ns",
    "base_version",
    "base_hash",
    "params",
    "source_hash",
    "sample_rate",
    "channel_names",
    "bands",
    "segments",
    "chain_head",
}


def _invalid(message: str, **details: object) -> CalibrationError:
    return CalibrationError(E_RECOVERY_INVALID, message, dict(details))


@dataclass(frozen=True)
class SnapshotSegment:
    index: int
    channel_index: int
    band_start: int
    band_end: int
    file: str
    payload_hash: str
    previous_hash: str
    channel: ChannelComputation

    def to_manifest(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "channel_index": self.channel_index,
            "band_start": self.band_start,
            "band_end": self.band_end,
            "file": self.file,
            "payload_hash": self.payload_hash,
            "previous_hash": self.previous_hash,
        }


@dataclass(frozen=True)
class RecoverySnapshot:
    run_id: str
    manifest_path: Path
    params: CalibrationParams
    source_hash: str
    sample_rate: int
    channel_names: tuple[str, ...]
    bands: tuple[BandSpec, ...]
    base_version: int
    base_hash: str | None
    segments: tuple[SnapshotSegment, ...]
    chain_head: str
    status: str

    @property
    def completed_channels(self) -> dict[int, ChannelComputation]:
        return {segment.channel_index: segment.channel for segment in self.segments}

    @property
    def completed_channel_indices(self) -> tuple[int, ...]:
        return tuple(sorted(self.completed_channels))


@dataclass
class SnapshotRun:
    run_id: str
    directory: Path
    manifest_path: Path
    base_version: int
    base_hash: str | None
    params: CalibrationParams
    source_hash: str
    sample_rate: int
    channel_names: tuple[str, ...]
    bands: tuple[BandSpec, ...]
    segments: list[dict[str, Any]] = field(default_factory=list)
    chain_head: str = GENESIS_HASH
    created_ns: int = field(default_factory=time.time_ns)
    status: str = "running"

    @property
    def completed_channels(self) -> set[int]:
        return {int(segment["channel_index"]) for segment in self.segments}

    def manifest(self) -> dict[str, Any]:
        return {
            "format_version": 1,
            "run_id": self.run_id,
            "created_ns": self.created_ns,
            "base_version": self.base_version,
            "base_hash": self.base_hash,
            "params": self.params.to_dict(),
            "source_hash": self.source_hash,
            "sample_rate": self.sample_rate,
            "channel_names": list(self.channel_names),
            "bands": [band.to_dict() for band in self.bands],
            "segments": list(self.segments),
            "chain_head": self.chain_head,
            "status": self.status,
        }


class SnapshotManager:
    """Crash-safe per-channel snapshot lanes under ``snapshots/``."""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.snapshots_dir = self.root / "snapshots"
        self.snapshots_dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()

    def begin_run(
        self,
        params: CalibrationParams,
        source_hash: str,
        sample_rate: int,
        channel_names: Sequence[str],
        bands: Sequence[BandSpec],
        base_version: int = 0,
        base_hash: str | None = None,
    ) -> SnapshotRun:
        created_ns = time.time_ns()
        run_id = f"run-{created_ns}-{uuid.uuid4().hex[:8]}"
        directory = self.snapshots_dir / run_id
        directory.mkdir(parents=True, exist_ok=False)
        run = SnapshotRun(
            run_id=run_id,
            directory=directory,
            manifest_path=directory / "manifest.json",
            base_version=int(base_version),
            base_hash=base_hash,
            params=params,
            source_hash=source_hash,
            sample_rate=int(sample_rate),
            channel_names=tuple(channel_names),
            bands=tuple(bands),
            chain_head=GENESIS_HASH,
            created_ns=created_ns,
        )
        atomic_write_json(run.manifest_path, run.manifest(), mode=0o444)
        return run

    def write_channel(
        self,
        run: SnapshotRun,
        channel: ChannelComputation,
    ) -> bool:
        """Atomically append one completed channel segment.

        Returns ``False`` when the channel was already committed, preventing
        duplicate writes after a resumed run.
        """

        with self._lock:
            channel_index = channel.alignment.channel_index
            if channel_index in run.completed_channels:
                return False
            segment_index = len(run.segments)
            band_start = 0
            band_end = len(run.bands) - 1
            payload = {
                "run_id": run.run_id,
                "segment_index": segment_index,
                "channel_index": channel_index,
                "band_start": band_start,
                "band_end": band_end,
                "params_hash": sha256_obj(run.params.to_dict()),
                "source_hash": run.source_hash,
                "previous_hash": run.chain_head,
                "channel": channel.to_dict(),
            }
            payload_hash = sha256_obj(payload)
            file_name = f"segment-{segment_index:03d}-ch{channel_index:02d}.json"
            segment_path = run.directory / file_name
            atomic_write_json(
                segment_path,
                {
                    "payload": payload,
                    "payload_hash": payload_hash,
                },
                mode=0o444,
            )
            manifest_entry = {
                "index": segment_index,
                "channel_index": channel_index,
                "band_start": band_start,
                "band_end": band_end,
                "file": file_name,
                "payload_hash": payload_hash,
                "previous_hash": run.chain_head,
            }
            run.segments.append(manifest_entry)
            run.chain_head = payload_hash
            atomic_write_json(run.manifest_path, run.manifest(), mode=0o444)
            return True

    def mark(
        self,
        run: SnapshotRun,
        status: str,
        published_version: int | None = None,
        published_hash: str | None = None,
    ) -> None:
        with self._lock:
            run.status = status
            payload = run.manifest()
            payload["published_version"] = published_version
            payload["published_hash"] = published_hash
            atomic_write_json(run.manifest_path, payload, mode=0o444)

    def _run_directories(self) -> list[Path]:
        return sorted(
            [path for path in self.snapshots_dir.iterdir() if path.is_dir() and (path / "manifest.json").exists()],
            key=lambda path: path.name,
            reverse=True,
        )

    def latest_manifest_path(self) -> Path | None:
        directories = self._run_directories()
        return None if not directories else directories[0] / "manifest.json"

    @staticmethod
    def _interval(data: dict[str, Any], segment_index: int | None = None) -> dict[str, Any]:
        return {
            "segment_index": segment_index,
            "channel_index": data.get("channel_index"),
            "band_start": data.get("band_start"),
            "band_end": data.get("band_end"),
        }

    def validate_and_load(self, manifest_path: str | Path) -> RecoverySnapshot:
        path = Path(manifest_path)
        try:
            manifest = read_json(path)
        except Exception as exc:
            raise _invalid(
                "snapshot manifest cannot be read",
                manifest=str(path),
                first_broken_interval=None,
                error=str(exc),
            ) from exc
        missing = sorted(REQUIRED_MANIFEST_FIELDS - set(manifest))
        if missing:
            raise _invalid(
                "snapshot manifest is missing required fields",
                manifest=str(path),
                missing_fields=missing,
                first_broken_interval=None,
            )

        try:
            params = CalibrationParams.from_dict(manifest["params"])
            channel_names = tuple(str(item) for item in manifest["channel_names"])
            bands = tuple(BandSpec.from_dict(item) for item in manifest["bands"])
            base_version = int(manifest["base_version"])
            base_hash = None if manifest["base_hash"] is None else str(manifest["base_hash"])
            sample_rate = int(manifest["sample_rate"])
            source_hash = str(manifest["source_hash"])
            run_id = str(manifest["run_id"])
            status = str(manifest.get("status", "running"))
        except Exception as exc:
            raise _invalid(
                "snapshot metadata is invalid",
                manifest=str(path),
                first_broken_interval=None,
                error=str(exc),
            ) from exc

        if len(channel_names) not in (1, 6) or sample_rate != 48000:
            raise _invalid(
                "snapshot source metadata is invalid",
                channel_count=len(channel_names),
                sample_rate=sample_rate,
                first_broken_interval=None,
            )
        if not isinstance(manifest["segments"], list):
            raise _invalid("snapshot segments field must be a list", first_broken_interval=None)

        segment_entries = manifest["segments"]
        expected_previous = GENESIS_HASH
        validated: list[SnapshotSegment] = []
        seen_channels: set[int] = set()
        expected_segment_index = 0
        for entry in segment_entries:
            if not isinstance(entry, dict):
                raise _invalid("snapshot segment entry is malformed", first_broken_interval=None)
            interval = self._interval(entry, entry.get("index"))
            required = {"index", "channel_index", "band_start", "band_end", "file", "payload_hash", "previous_hash"}
            missing_segment_fields = sorted(required - set(entry))
            if missing_segment_fields:
                raise _invalid(
                    "snapshot segment is missing required fields",
                    missing_fields=missing_segment_fields,
                    first_broken_interval=interval,
                )
            try:
                index = int(entry["index"])
                channel_index = int(entry["channel_index"])
                band_start = int(entry["band_start"])
                band_end = int(entry["band_end"])
                file_name = str(entry["file"])
                expected_payload_hash = str(entry["payload_hash"])
                previous_hash = str(entry["previous_hash"])
            except Exception as exc:
                raise _invalid("snapshot segment fields are invalid", first_broken_interval=interval, error=str(exc)) from exc
            interval = self._interval(entry, index)
            if index != expected_segment_index:
                raise _invalid(
                    "snapshot segment index chain is broken",
                    expected_index=expected_segment_index,
                    first_broken_interval=interval,
                )
            if channel_index < 0 or channel_index >= len(channel_names):
                raise _invalid("snapshot channel index is out of range", first_broken_interval=interval)
            if band_start < 0 or band_end < band_start or band_end >= len(bands):
                raise _invalid("snapshot band interval is invalid", first_broken_interval=interval)
            if band_start != 0 or band_end != len(bands) - 1:
                raise _invalid(
                    "snapshot channel interval is incomplete",
                    expected_band_start=0,
                    expected_band_end=len(bands) - 1,
                    first_broken_interval=interval,
                )
            if channel_index in seen_channels:
                raise _invalid("snapshot contains a duplicate channel interval", first_broken_interval=interval)
            if previous_hash != expected_previous:
                raise _invalid(
                    "snapshot hash chain is broken",
                    expected_previous_hash=expected_previous,
                    actual_previous_hash=previous_hash,
                    first_broken_interval=interval,
                )
            segment_path = path.parent / file_name
            if not segment_path.exists():
                raise _invalid(
                    "snapshot segment file is missing",
                    file=file_name,
                    first_broken_interval=interval,
                )
            try:
                wrapped = read_json(segment_path)
                payload = wrapped["payload"]
                stored_hash = str(wrapped["payload_hash"])
                actual_hash = sha256_obj(payload)
            except Exception as exc:
                raise _invalid(
                    "snapshot segment cannot be decoded",
                    file=file_name,
                    first_broken_interval=interval,
                    error=str(exc),
                ) from exc
            if stored_hash != expected_payload_hash or actual_hash != expected_payload_hash:
                raise _invalid(
                    "snapshot segment content hash mismatch",
                    file=file_name,
                    expected_hash=expected_payload_hash,
                    stored_hash=stored_hash,
                    actual_hash=actual_hash,
                    first_broken_interval=interval,
                )
            if previous_hash != payload.get("previous_hash"):
                raise _invalid(
                    "snapshot segment previous hash mismatch",
                    file=file_name,
                    first_broken_interval=interval,
                )
            if int(payload.get("channel_index", -1)) != channel_index:
                raise _invalid(
                    "snapshot segment channel differs from manifest",
                    file=file_name,
                    first_broken_interval=interval,
                )
            if str(payload.get("params_hash")) != sha256_obj(params.to_dict()):
                raise _invalid(
                    "snapshot segment parameters differ from manifest",
                    file=file_name,
                    first_broken_interval=interval,
                )
            try:
                channel = ChannelComputation.from_dict(payload["channel"])
            except Exception as exc:
                raise _invalid(
                    "snapshot channel payload is malformed",
                    file=file_name,
                    first_broken_interval=interval,
                    error=str(exc),
                ) from exc
            if channel.alignment.channel_index != channel_index:
                raise _invalid(
                    "snapshot channel payload index mismatch",
                    file=file_name,
                    first_broken_interval=interval,
                )
            validated.append(
                SnapshotSegment(
                    index=index,
                    channel_index=channel_index,
                    band_start=band_start,
                    band_end=band_end,
                    file=file_name,
                    payload_hash=actual_hash,
                    previous_hash=previous_hash,
                    channel=channel,
                )
            )
            expected_previous = actual_hash
            expected_segment_index += 1
            seen_channels.add(channel_index)

        chain_head = str(manifest["chain_head"])
        if chain_head != expected_previous:
            raise _invalid(
                "snapshot chain head does not match the last segment",
                expected_chain_head=expected_previous,
                actual_chain_head=chain_head,
                first_broken_interval=None if not validated else self._interval(validated[-1].to_manifest(), validated[-1].index),
            )
        return RecoverySnapshot(
            run_id=run_id,
            manifest_path=path,
            params=params,
            source_hash=source_hash,
            sample_rate=sample_rate,
            channel_names=channel_names,
            bands=bands,
            base_version=base_version,
            base_hash=base_hash,
            segments=tuple(validated),
            chain_head=chain_head,
            status=status,
        )

    def load_latest_valid(self) -> RecoverySnapshot | None:
        path = self.latest_manifest_path()
        if path is None:
            return None
        return self.validate_and_load(path)

    def load_run(self, run_id: str) -> RecoverySnapshot:
        path = self.snapshots_dir / run_id / "manifest.json"
        if not path.exists():
            raise _invalid("snapshot run does not exist", run_id=run_id, first_broken_interval=None)
        return self.validate_and_load(path)
