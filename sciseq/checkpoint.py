"""Atomic, serialized per-frame checkpoint storage."""

from __future__ import annotations

import json
import os
import threading
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import SciSeqError
from .models import ExtractedFeature, NoiseEstimate, array_sha256

SNAPSHOT_SCHEMA = "1.0"


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, sort_keys=True, separators=(",", ":"), allow_nan=False)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass


@dataclass
class SnapshotRecord:
    index: int
    offset: np.ndarray
    denoised: np.ndarray
    noise_sigma: float
    background: float
    stats: dict[str, Any]
    denoised_sha256: str
    content_sha256: str
    parameter_version: str
    algorithm_version: str
    sequence_fingerprint: str

    def features(self) -> tuple[ExtractedFeature, ...]:
        result: list[ExtractedFeature] = []
        for item in self.stats.get("features", []):
            result.append(
                ExtractedFeature(
                    frame=int(item["frame"]),
                    feature_id=str(item["feature_id"]),
                    kind=str(item["kind"]),
                    center=(float(item["center"][0]), float(item["center"][1])),
                    area=int(item["area"]),
                    peak=float(item["peak"]),
                    bbox=tuple(int(value) for value in item["bbox"]),
                    orientation_deg=float(item["orientation_deg"]),
                    elongation=float(item["elongation"]),
                    mean_intensity=float(item["mean_intensity"]),
                )
            )
        return tuple(result)


@dataclass
class SnapshotLoad:
    valid: bool
    records: list[SnapshotRecord]
    invalidated: bool
    reason: str
    quarantine_path: Path | None = None


class CheckpointStore:
    def __init__(
        self,
        root: Path | str,
        *,
        sequence_fingerprint: str,
        parameter_version: str,
        algorithm_version: str,
        sequence_id: str,
        frame_count: int,
        shape: tuple[int, int],
    ) -> None:
        self.root = Path(root)
        self.sequence_fingerprint = sequence_fingerprint
        self.parameter_version = parameter_version
        self.algorithm_version = algorithm_version
        self.sequence_id = sequence_id
        self.frame_count = int(frame_count)
        self.shape = (int(shape[0]), int(shape[1]))
        self._lock = threading.Lock()
        self.root.mkdir(parents=True, exist_ok=True)
        self.header_path = self.root / "snapshot.json"

    def _header(self, committed_count: int) -> dict[str, Any]:
        return {
            "schema": SNAPSHOT_SCHEMA,
            "sequence_id": self.sequence_id,
            "sequence_fingerprint": self.sequence_fingerprint,
            "parameter_version": self.parameter_version,
            "algorithm_version": self.algorithm_version,
            "frame_count": self.frame_count,
            "shape": list(self.shape),
            "committed_count": int(committed_count),
        }

    def _record_path(self, index: int) -> Path:
        return self.root / f"frame_{index:06d}.npz"

    def commit(
        self,
        index: int,
        *,
        offset: Iterable[float],
        denoised: np.ndarray,
        noise_sigma: float,
        background: float,
        stats: dict[str, Any],
        content_sha256: str,
    ) -> str:
        denoised_array = np.ascontiguousarray(denoised, dtype=np.float32)
        denoised_digest = array_sha256(denoised_array)
        stats_payload = json.dumps(stats, sort_keys=True, separators=(",", ":"), allow_nan=False)
        offset_array = np.asarray(tuple(offset), dtype=np.float64)
        with self._lock:
            existing = sorted(self.root.glob("frame_*.npz"))
            if int(index) != len(existing):
                raise SciSeqError(
                    "E_CHECKPOINT",
                    "checkpoint commits must be contiguous and serial",
                    frame=int(index),
                    field="commit_order",
                    details={"expected": len(existing), "actual": int(index)},
                )
            path = self._record_path(int(index))
            temporary = path.with_name(f".{path.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
            with temporary.open("wb") as handle:
                np.savez(
                    handle,
                    index=np.asarray(int(index), dtype=np.int64),
                    offset=offset_array,
                    denoised=denoised_array,
                    noise_sigma=np.asarray(float(noise_sigma), dtype=np.float64),
                    background=np.asarray(float(background), dtype=np.float64),
                    stats_json=np.asarray(stats_payload),
                    denoised_sha256=np.asarray(denoised_digest),
                    content_sha256=np.asarray(str(content_sha256)),
                    parameter_version=np.asarray(self.parameter_version),
                    algorithm_version=np.asarray(self.algorithm_version),
                    sequence_fingerprint=np.asarray(self.sequence_fingerprint),
                    sequence_id=np.asarray(self.sequence_id),
                    frame_count=np.asarray(self.frame_count, dtype=np.int64),
                )
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, path)
            try:
                directory_fd = os.open(self.root, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
            _atomic_json(self.header_path, self._header(len(existing) + 1))
        return denoised_digest

    def _invalidate(self, reason: str) -> SnapshotLoad:
        quarantine: Path | None = None
        if self.root.exists() and any(self.root.iterdir()):
            quarantine = self.root.with_name(
                f"{self.root.name}.invalid-{uuid.uuid4().hex[:12]}"
            )
            os.replace(self.root, quarantine)
        self.root.mkdir(parents=True, exist_ok=True)
        return SnapshotLoad(
            valid=False,
            records=[],
            invalidated=quarantine is not None,
            reason=reason,
            quarantine_path=quarantine,
        )

    def reset(self, reason: str = "fresh run requested") -> SnapshotLoad:
        with self._lock:
            return self._invalidate(reason)

    def load_and_validate(self, expected_content_hashes: list[str] | tuple[str, ...]) -> SnapshotLoad:
        with self._lock:
            if not self.root.exists():
                return SnapshotLoad(False, [], False, "snapshot directory does not exist")
            files = sorted(self.root.glob("frame_*.npz"))
            if not files:
                return SnapshotLoad(False, [], False, "no committed frame records")
            header: dict[str, Any] | None = None
            if self.header_path.exists():
                try:
                    header = json.loads(self.header_path.read_text(encoding="utf-8"))
                except (OSError, json.JSONDecodeError) as exc:
                    return self._invalidate(f"snapshot header cannot be decoded: {exc}")
                expected_header = self._header(int(header.get("committed_count", -1)))
                for key in (
                    "schema",
                    "sequence_id",
                    "sequence_fingerprint",
                    "parameter_version",
                    "algorithm_version",
                    "frame_count",
                    "shape",
                ):
                    if header.get(key) != expected_header.get(key):
                        return self._invalidate(
                            f"snapshot header mismatch in {key}"
                        )
            records: list[SnapshotRecord] = []
            contiguous = 0
            for offset, path in enumerate(files):
                expected_name = self._record_path(offset).name
                if path.name != expected_name:
                    return self._invalidate(f"non-contiguous checkpoint record: {path.name}")
                try:
                    with np.load(path, allow_pickle=False) as payload:
                        index = int(payload["index"])
                        offset_array = np.asarray(payload["offset"], dtype=np.float64)
                        denoised = np.ascontiguousarray(payload["denoised"], dtype=np.float32)
                        noise_sigma = float(payload["noise_sigma"])
                        background = float(payload["background"])
                        stats = json.loads(str(payload["stats_json"]))
                        denoised_sha = str(payload["denoised_sha256"])
                        content_sha = str(payload["content_sha256"])
                        parameter_version = str(payload["parameter_version"])
                        algorithm_version = str(payload["algorithm_version"])
                        sequence_fingerprint = str(payload["sequence_fingerprint"])
                        sequence_id = str(payload["sequence_id"])
                        frame_count = int(payload["frame_count"])
                except (OSError, ValueError, KeyError, json.JSONDecodeError) as exc:
                    return self._invalidate(f"checkpoint record decode failure at {path.name}: {exc}")
                if (
                    index != offset
                    or index >= len(expected_content_hashes)
                    or offset_array.shape != (2,)
                    or denoised.shape != self.shape
                    or parameter_version != self.parameter_version
                    or algorithm_version != self.algorithm_version
                    or sequence_fingerprint != self.sequence_fingerprint
                    or sequence_id != self.sequence_id
                    or frame_count != self.frame_count
                ):
                    return self._invalidate(
                        f"checkpoint record metadata mismatch at frame {index}"
                    )
                if denoised_sha != array_sha256(denoised):
                    return self._invalidate(
                        f"denoised digest mismatch at frame {index}"
                    )
                if content_sha != str(expected_content_hashes[index]):
                    return self._invalidate(
                        f"source content hash mismatch at frame {index}"
                    )
                records.append(
                    SnapshotRecord(
                        index=index,
                        offset=offset_array,
                        denoised=denoised,
                        noise_sigma=noise_sigma,
                        background=background,
                        stats=stats,
                        denoised_sha256=denoised_sha,
                        content_sha256=content_sha,
                        parameter_version=parameter_version,
                        algorithm_version=algorithm_version,
                        sequence_fingerprint=sequence_fingerprint,
                    )
                )
                contiguous += 1
            if header is not None and int(header.get("committed_count", -1)) > contiguous:
                return self._invalidate("snapshot header references missing frame records")
            return SnapshotLoad(True, records, False, "snapshot validated")

