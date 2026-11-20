from __future__ import annotations

import threading
from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import E_VERSION_STALE, HapticError
from .model import ProjectConfig, WaveSegment
from .util import GENESIS_HASH, canonical_json_bytes, sha256_hex


@dataclass(frozen=True)
class SegmentRecord:
    index: int
    start_sample: int
    end_sample: int
    sample_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "sample_interval": [self.start_sample, self.end_sample],
            "sample_hash": self.sample_hash,
        }


@dataclass(frozen=True)
class ChunkDigest:
    start_sample: int
    end_sample: int
    signal_hash: str
    limited_samples: int
    out_of_range_samples: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_interval": [self.start_sample, self.end_sample],
            "signal_hash": self.signal_hash,
            "limited_samples": self.limited_samples,
            "out_of_range_samples": self.out_of_range_samples,
        }


@dataclass(frozen=True)
class VersionHistoryEntry:
    version: int
    parent_hash: str
    content_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "parent_hash": self.parent_hash,
            "content_hash": self.content_hash,
        }


def chunk_signal_hash(
    start_sample: int,
    end_sample: int,
    left: np.ndarray,
    right: np.ndarray,
) -> str:
    payload = bytearray(b"haptic-chunk-v1\0")
    payload.extend(int(start_sample).to_bytes(8, "little", signed=True))
    payload.extend(int(end_sample).to_bytes(8, "little", signed=True))
    payload.extend(np.asarray(left, dtype="<f8", order="C").tobytes())
    payload.extend(np.asarray(right, dtype="<f8", order="C").tobytes())
    return sha256_hex(bytes(payload))


def compute_content_hash(
    *,
    version: int,
    parent_hash: str,
    config: ProjectConfig,
    segments: Iterable[WaveSegment],
    segment_records: Iterable[SegmentRecord],
    chunks: Iterable[ChunkDigest],
    limited_samples: int,
    out_of_range_samples: int,
) -> str:
    payload = {
        "schema": "haptic-version-v1",
        "version": int(version),
        "parent_hash": parent_hash,
        "config": config.to_dict(),
        "segments": [segment.to_dict() for segment in segments],
        "segment_records": [record.to_dict() for record in segment_records],
        "chunks": [chunk.to_dict() for chunk in chunks],
        "limited_samples": int(limited_samples),
        "out_of_range_samples": int(out_of_range_samples),
    }
    return sha256_hex(canonical_json_bytes(payload))


@dataclass(frozen=True, eq=False)
class VersionedSignal:
    """An immutable, fully published preview version."""

    version: int
    parent_hash: str
    content_hash: str
    sample_rate: int
    left: np.ndarray
    right: np.ndarray
    limited_mask: np.ndarray
    out_of_range_mask: np.ndarray
    segments: tuple[WaveSegment, ...]
    config: ProjectConfig
    segment_records: tuple[SegmentRecord, ...]
    chunks: tuple[ChunkDigest, ...]
    limited_samples: int
    out_of_range_samples: int
    modified_indices: tuple[int, ...] = ()

    def __post_init__(self) -> None:
        left = np.asarray(self.left, dtype=np.float64)
        right = np.asarray(self.right, dtype=np.float64)
        limited = np.asarray(self.limited_mask, dtype=np.bool_)
        out_of_range = np.asarray(self.out_of_range_mask, dtype=np.bool_)
        if not (left.ndim == right.ndim == limited.ndim == out_of_range.ndim == 1):
            raise ValueError("published signal arrays must be one-dimensional")
        if not (left.shape == right.shape == limited.shape == out_of_range.shape):
            raise ValueError("published signal arrays must have equal length")
        left.setflags(write=False)
        right.setflags(write=False)
        limited.setflags(write=False)
        out_of_range.setflags(write=False)
        object.__setattr__(self, "left", left)
        object.__setattr__(self, "right", right)
        object.__setattr__(self, "limited_mask", limited)
        object.__setattr__(self, "out_of_range_mask", out_of_range)

    @property
    def sample_count(self) -> int:
        return int(self.left.shape[0])

    @property
    def duration_ms(self) -> float:
        return self.sample_count * 1000.0 / self.sample_rate

    def signal_bytes(self) -> bytes:
        return (
            np.asarray(self.left, dtype="<f8", order="C").tobytes()
            + np.asarray(self.right, dtype="<f8", order="C").tobytes()
        )

    def to_summary(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "parent_hash": self.parent_hash,
            "content_hash": self.content_hash,
            "sample_rate": self.sample_rate,
            "sample_count": self.sample_count,
            "segment_hashes": [
                {
                    "index": record.index,
                    "sample_interval": [
                        record.start_sample,
                        record.end_sample,
                    ],
                    "sample_hash": record.sample_hash,
                }
                for record in self.segment_records
            ],
            "limited_samples": self.limited_samples,
            "out_of_range_samples": self.out_of_range_samples,
            "modified_indices": list(self.modified_indices),
        }


class Publisher:
    """Atomically swaps a complete immutable version under one lock."""

    def __init__(
        self,
        initial: VersionedSignal | None = None,
        history: Iterable[VersionHistoryEntry] | None = None,
    ) -> None:
        self._lock = threading.RLock()
        self._current = initial
        if history is not None:
            self._history = list(history)
        elif initial is None:
            self._history = []
        else:
            self._history = [
                VersionHistoryEntry(
                    version=initial.version,
                    parent_hash=initial.parent_hash,
                    content_hash=initial.content_hash,
                )
            ]
        if initial is not None and (
            not self._history or self._history[-1].content_hash != initial.content_hash
        ):
            raise ValueError("initial publisher history does not match version")

    def current(self) -> VersionedSignal | None:
        with self._lock:
            return self._current

    def require_current(self) -> VersionedSignal:
        with self._lock:
            if self._current is None:
                raise HapticError(
                    E_VERSION_STALE,
                    "no version has been published",
                )
            return self._current

    def get_version(
        self,
        version: int,
        content_hash: str | None = None,
    ) -> VersionedSignal:
        with self._lock:
            current = self._current
            if current is None:
                raise HapticError(
                    E_VERSION_STALE,
                    "no version has been published",
                    details={"requested_version": version},
                )
            if current.version != version:
                raise HapticError(
                    E_VERSION_STALE,
                    f"version {version} is not the current published version",
                    details={
                        "requested_version": version,
                        "current_version": current.version,
                    },
                )
            if content_hash is not None and current.content_hash != content_hash:
                raise HapticError(
                    E_VERSION_STALE,
                    "version number and content hash do not match",
                    details={
                        "requested_version": version,
                        "requested_hash": content_hash,
                        "current_hash": current.content_hash,
                    },
                )
            return current

    def require_current_match(
        self,
        version: int,
        content_hash: str,
    ) -> VersionedSignal:
        return self.get_version(version, content_hash)

    def publish(
        self,
        candidate: VersionedSignal,
        *,
        expected_parent_hash: str | None = None,
    ) -> VersionedSignal:
        with self._lock:
            if self._current is None:
                if candidate.parent_hash != GENESIS_HASH:
                    raise HapticError(
                        E_VERSION_STALE,
                        "first version must descend from the genesis hash",
                    )
            else:
                parent_hash = self._current.content_hash
                if (
                    expected_parent_hash is not None
                    and expected_parent_hash != parent_hash
                ):
                    raise HapticError(
                        E_VERSION_STALE,
                        "publish parent differs from current published version",
                        details={
                            "expected_parent_hash": expected_parent_hash,
                            "current_hash": parent_hash,
                        },
                    )
                if candidate.parent_hash != parent_hash:
                    raise HapticError(
                        E_VERSION_STALE,
                        "candidate does not descend from current version",
                        details={
                            "candidate_parent_hash": candidate.parent_hash,
                            "current_hash": parent_hash,
                        },
                    )
                if candidate.version != self._current.version + 1:
                    raise HapticError(
                        E_VERSION_STALE,
                        "candidate version is not the next version",
                        details={
                            "candidate_version": candidate.version,
                            "current_version": self._current.version,
                        },
                    )
            self._current = candidate
            self._history.append(
                VersionHistoryEntry(
                    version=candidate.version,
                    parent_hash=candidate.parent_hash,
                    content_hash=candidate.content_hash,
                )
            )
            return candidate

    def history(self) -> tuple[VersionHistoryEntry, ...]:
        with self._lock:
            return tuple(self._history)

    def snapshot(self) -> VersionedSignal:
        """Return a coherent immutable version reference safe for offline reads."""
        return self.require_current()
