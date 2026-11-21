from __future__ import annotations

import threading
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from .canonical import content_hash
from .errors import ErrorCode, HapticError
from .models import DeviceConfig, WaveSequence
from .sampler import LimitStats, SamplingResult


@dataclass(frozen=True, slots=True)
class ConfigVersion:
    version: int
    config: DeviceConfig
    config_hash: str
    parent_hash: str | None
    source_version: int | None = None

    @property
    def version_hash(self) -> str:
        return content_hash(
            {
                "version": self.version,
                "config_hash": self.config_hash,
                "config": self.config.to_dict(),
                "parent_hash": self.parent_hash,
                "source_version": self.source_version,
            }
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "config": self.config.to_dict(),
            "config_hash": self.config_hash,
            "parent_hash": self.parent_hash,
            "source_version": self.source_version,
            "version_hash": self.version_hash,
        }

    @classmethod
    def from_payload(cls, payload: Mapping[str, Any]) -> ConfigVersion:
        try:
            config = DeviceConfig.from_mapping(payload["config"])
            version = cls(
                version=int(payload["version"]),
                config=config,
                config_hash=str(payload["config_hash"]),
                parent_hash=(
                    None
                    if payload["parent_hash"] is None
                    else str(payload["parent_hash"])
                ),
                source_version=(
                    None
                    if payload.get("source_version") is None
                    else int(payload["source_version"])
                ),
            )
        except HapticError as exc:
            if exc.code == ErrorCode.RECOVERY_INVALID:
                raise
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "invalid config version record",
                field=exc.field,
                details={"cause": exc.to_dict()},
            ) from exc
        except (KeyError, TypeError, ValueError) as exc:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "invalid config version record",
                details={"error": str(exc)},
            ) from exc
        if config.content_hash != version.config_hash:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "config version hash mismatch",
                version=version.version,
            )
        return version


class ConfigVersionStore:
    def __init__(self, *, initial: DeviceConfig | None = None) -> None:
        self._lock = threading.RLock()
        self._versions: dict[int, ConfigVersion] = {}
        if initial is not None:
            self.commit(initial)

    def commit(
        self, config: DeviceConfig, *, source_version: int | None = None
    ) -> ConfigVersion:
        if source_version is not None and source_version not in self._versions:
            raise HapticError(
                ErrorCode.NOT_FOUND,
                f"config version {source_version} does not exist",
                version=source_version,
            )
        with self._lock:
            version = max(self._versions, default=0) + 1
            parent_hash = (
                self._versions[max(self._versions)].version_hash
                if self._versions
                else None
            )
            record = ConfigVersion(
                version=version,
                config=config,
                config_hash=config.content_hash,
                parent_hash=parent_hash,
                source_version=source_version,
            )
            self._versions[version] = record
            return record

    def restore(self, versions: tuple[ConfigVersion, ...]) -> None:
        ordered = tuple(sorted(versions, key=lambda item: item.version))
        if any(item.version < 1 for item in ordered):
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "config versions must be positive",
                version=1,
                field="version",
            )
        with self._lock:
            self._versions = {item.version: item for item in ordered}

    def rollback(self, version: int) -> ConfigVersion:
        target = self.get(version)
        return self.commit(target.config, source_version=version)

    def get(self, version: int) -> ConfigVersion:
        with self._lock:
            try:
                return self._versions[version]
            except KeyError as exc:
                raise HapticError(
                    ErrorCode.NOT_FOUND,
                    f"config version {version} does not exist",
                    version=version,
                ) from exc

    @property
    def latest(self) -> ConfigVersion:
        with self._lock:
            if not self._versions:
                raise HapticError(ErrorCode.NOT_FOUND, "no config versions exist")
            return self._versions[max(self._versions)]

    @property
    def versions(self) -> tuple[ConfigVersion, ...]:
        with self._lock:
            return tuple(self._versions[index] for index in sorted(self._versions))

    def diff(self, older: int, newer: int) -> dict[str, Any]:
        left = self.get(older).config.to_dict()
        right = self.get(newer).config.to_dict()
        changes: list[dict[str, Any]] = []
        for key in sorted(set(left) | set(right)):
            if left.get(key) != right.get(key):
                changes.append(
                    {"field": key, "old": left.get(key), "new": right.get(key)}
                )
        return {
            "older_version": older,
            "newer_version": newer,
            "older_hash": self.get(older).config_hash,
            "newer_hash": self.get(newer).config_hash,
            "changes": changes,
        }


@dataclass(frozen=True, slots=True)
class PreviewHandle:
    generation: int
    publication_version: int
    signal_hash: str


@dataclass(frozen=True, slots=True)
class PublishedSignal:
    publication_version: int
    config_version: int
    sequence: WaveSequence
    config: DeviceConfig
    sampling: SamplingResult
    affected_segments: tuple[int, ...]
    recomputed_segments: tuple[int, ...]
    restored_from_checkpoint: tuple[int, ...]
    reused_segments: tuple[int, ...]
    affected_ranges: tuple[tuple[int, int], ...]
    reused_ranges: tuple[tuple[int, int], ...]
    recomputed_sample_count: int
    reused_sample_count: int
    full_recompute: bool
    full_reason: str | None
    reasons: tuple[str, ...]
    parent_version_hash: str | None

    @property
    def signal_hash(self) -> str:
        return self.sampling.signal_hash

    @property
    def version_hash(self) -> str:
        return content_hash(self.version_payload())

    @property
    def limit_stats(self) -> LimitStats:
        return self.sampling.limit_stats

    def version_payload(self) -> dict[str, Any]:
        return {
            "publication_version": self.publication_version,
            "parent_version_hash": self.parent_version_hash,
            "config_version": self.config_version,
            "config_hash": self.config.content_hash,
            "sequence_hash": self.sequence.content_hash,
            "signal_hash": self.signal_hash,
            "total_frames": self.sampling.total_frames,
            "limit_stats": self.sampling.limit_stats.to_dict(),
            "segment_metadata": [
                signal.to_metadata() for signal in self.sampling.segment_signals
            ],
            "affected_segments": list(self.affected_segments),
            "recomputed_segments": list(self.recomputed_segments),
            "restored_from_checkpoint": list(self.restored_from_checkpoint),
            "reused_segments": list(self.reused_segments),
            "recomputed_segment_count": len(self.recomputed_segments),
            "restored_checkpoint_segment_count": len(self.restored_from_checkpoint),
            "reused_segment_count": len(self.reused_segments),
            "affected_ranges": [list(item) for item in self.affected_ranges],
            "reused_ranges": [list(item) for item in self.reused_ranges],
            "recomputed_sample_count": self.recomputed_sample_count,
            "reused_sample_count": self.reused_sample_count,
            "full_recompute": self.full_recompute,
            "full_reason": self.full_reason,
            "reasons": list(self.reasons),
        }

    def trace_record(self) -> dict[str, Any]:
        return {
            "event": "publish",
            "version": self.publication_version,
            "config_version": self.config_version,
            "config_hash": self.config.content_hash,
            "sequence_hash": self.sequence.content_hash,
            "signal_hash": self.signal_hash,
            "version_hash": self.version_hash,
            "affected_segments": list(self.affected_segments),
            "recomputed_segments": list(self.recomputed_segments),
            "restored_from_checkpoint": list(self.restored_from_checkpoint),
            "reused_segments": list(self.reused_segments),
            "recomputed_segment_count": len(self.recomputed_segments),
            "restored_checkpoint_segment_count": len(self.restored_from_checkpoint),
            "reused_segment_count": len(self.reused_segments),
            "affected_ranges": [
                {"start_sample": start, "end_sample": end, "length": end - start}
                for start, end in self.affected_ranges
            ],
            "reused_ranges": [
                {"start_sample": start, "end_sample": end, "length": end - start}
                for start, end in self.reused_ranges
            ],
            "recomputed_interval_count": len(self.affected_ranges),
            "reused_interval_count": len(self.reused_ranges),
            "recomputed_sample_count": self.recomputed_sample_count,
            "reused_sample_count": self.reused_sample_count,
            "full_recompute": self.full_recompute,
            "full_reason": self.full_reason,
            "reasons": list(self.reasons),
            "limited_sample_count": self.limit_stats.limited_sample_count,
            "first_limited_sample": self.limit_stats.first_limited_sample,
        }


@dataclass(frozen=True, slots=True)
class QueryResult:
    publication_version: int
    config_version: int
    hit_version: int
    signal_hash: str
    version_hash: str
    generation: int
    left: Any
    right: Any
    limited_mask: Any
    limit_stats: LimitStats
    published: PublishedSignal


class AtomicPublisher:
    """Reference-swap publication with generation-scoped preview handles."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._generation = 0
        self._current: PublishedSignal | None = None
        self._history: dict[int, PublishedSignal] = {}

    @property
    def generation(self) -> int:
        with self._lock:
            return self._generation

    @property
    def current(self) -> PublishedSignal:
        with self._lock:
            if self._current is None:
                raise HapticError(ErrorCode.NOT_FOUND, "no published signal")
            return self._current

    def issue_handle(self) -> PreviewHandle:
        with self._lock:
            if self._current is None:
                raise HapticError(ErrorCode.NOT_FOUND, "no published signal")
            return PreviewHandle(
                generation=self._generation,
                publication_version=self._current.publication_version,
                signal_hash=self._current.signal_hash,
            )

    def publish(self, signal: PublishedSignal) -> PreviewHandle:
        with self._lock:
            if signal.publication_version in self._history:
                raise HapticError(
                    ErrorCode.IO,
                    f"publication version {signal.publication_version} already exists",
                    version=signal.publication_version,
                )
            self._generation += 1
            self._current = signal
            self._history[signal.publication_version] = signal
            return PreviewHandle(
                generation=self._generation,
                publication_version=signal.publication_version,
                signal_hash=signal.signal_hash,
            )

    def restore(self, signal: PublishedSignal) -> None:
        """Restore a persisted publication without changing its version number."""
        with self._lock:
            self._history[signal.publication_version] = signal
            if (
                self._current is None
                or signal.publication_version >= self._current.publication_version
            ):
                self._generation += 1
                self._current = signal

    def query(self, handle: PreviewHandle) -> QueryResult:
        with self._lock:
            current = self._current
            if current is None:
                raise HapticError(ErrorCode.NOT_FOUND, "no published signal")
            if (
                handle.generation != self._generation
                or handle.publication_version != current.publication_version
                or handle.signal_hash != current.signal_hash
            ):
                raise HapticError(
                    ErrorCode.STALE_HANDLE,
                    "preview handle refers to a superseded publication",
                    version=handle.publication_version,
                    details={
                        "handle_generation": handle.generation,
                        "current_generation": self._generation,
                    },
                )
            return self._query_result(current, handle.generation)

    def query_version(self, version: int) -> QueryResult:
        with self._lock:
            try:
                published = self._history[version]
            except KeyError as exc:
                raise HapticError(
                    ErrorCode.NOT_FOUND,
                    f"publication version {version} does not exist",
                    version=version,
                ) from exc
            return self._query_result(published, self._generation)

    def history(self) -> tuple[PublishedSignal, ...]:
        with self._lock:
            return tuple(self._history[version] for version in sorted(self._history))

    @staticmethod
    def _query_result(published: PublishedSignal, generation: int) -> QueryResult:
        return QueryResult(
            publication_version=published.publication_version,
            config_version=published.config_version,
            hit_version=published.publication_version,
            signal_hash=published.signal_hash,
            version_hash=published.version_hash,
            generation=generation,
            left=published.sampling.left_array,
            right=published.sampling.right_array,
            limited_mask=published.sampling.limited_mask,
            limit_stats=published.sampling.limit_stats,
            published=published,
        )
