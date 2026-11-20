"""Online meter session, immutable snapshots, and incremental parameters."""

from __future__ import annotations

import threading
from collections.abc import Mapping
from dataclasses import dataclass, replace
from typing import Any

from .analyzer import (
    AnalysisCache,
    BlockMeasurement,
    BlockResult,
    IntegratedTracker,
    block_result_from_measurement,
    integrated_series,
)
from .errors import BusyError, InvalidParameterError
from .params import AnalysisParameters
from .wavio import AudioBuffer


@dataclass(frozen=True)
class ParameterHistoryEntry:
    version: int
    previous_version: int | None
    reason: str
    changed_fields: dict[str, Any]
    affected_blocks: tuple[int, ...]
    committed_blocks_at_change: int
    params: dict[str, Any]

    def as_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "previous_version": self.previous_version,
            "reason": self.reason,
            "changed_fields": self.changed_fields,
            "affected_blocks": list(self.affected_blocks),
            "committed_blocks_at_change": self.committed_blocks_at_change,
            "params": self.params,
        }


@dataclass(frozen=True)
class MeterSnapshot:
    version: int
    parameter_version: int
    params: AnalysisParameters
    blocks: tuple[BlockResult, ...]
    committed_until_sample: int
    parameter_history: tuple[ParameterHistoryEntry, ...]
    cache_stats: dict[str, Any]
    affected_blocks: tuple[int, ...]
    publish_reason: str

    @property
    def committed_count(self) -> int:
        return len(self.blocks)

    @property
    def integrated_lufs(self) -> float | None:
        return self.blocks[-1].integrated_lufs if self.blocks else None

    @property
    def true_peak_dbtp(self) -> float | None:
        values = [
            block.true_peak_dbtp
            for block in self.blocks
            if block.true_peak_dbtp is not None
        ]
        return max(values) if values else None

    @property
    def true_peak_position_sample(self) -> float | None:
        candidates = [
            block for block in self.blocks if block.true_peak_dbtp is not None
        ]
        if not candidates:
            return None
        peak = max(candidates, key=lambda block: block.true_peak_dbtp or -999.0)
        return peak.true_peak_position_sample


@dataclass(frozen=True)
class MeterView:
    snapshot: MeterSnapshot
    playback_position_sample: int
    current_block_index: int
    playing: bool


class MeterSession:
    def __init__(
        self,
        audio: AudioBuffer,
        params: AnalysisParameters,
        manager: SessionManager,
    ) -> None:
        params.validate(audio.channels)
        self.audio = audio
        self._manager = manager
        self._lock = threading.RLock()
        self.params = params
        self.parameter_version = 1
        self._cache = AnalysisCache(audio)
        self._integrated = IntegratedTracker(
            params.absolute_gate_lufs,
            params.relative_gate_lu,
        )
        self._measurements: list[BlockMeasurement] = []
        self._blocks: tuple[BlockResult, ...] = ()
        self._position_sample = 0
        self._playing = False
        self._closed = False
        self._snapshot_version = 1
        self._history: tuple[ParameterHistoryEntry, ...] = (
            ParameterHistoryEntry(
                version=1,
                previous_version=None,
                reason="initial",
                changed_fields={},
                affected_blocks=(),
                committed_blocks_at_change=0,
                params=params.as_dict(),
            ),
        )
        self._snapshot = MeterSnapshot(
            version=1,
            parameter_version=1,
            params=params,
            blocks=(),
            committed_until_sample=0,
            parameter_history=self._history,
            cache_stats={
                "measurement_hits": 0,
                "measurement_misses": 0,
                "recomputed_measurement_blocks": 0,
                "reused_measurement_blocks": 0,
                "affected_blocks": 0,
            },
            affected_blocks=(),
            publish_reason="initial",
        )

    def snapshot(self) -> MeterSnapshot:
        with self._lock:
            return self._snapshot

    def view(self, position_sample: int | None = None) -> MeterView:
        with self._lock:
            position = (
                self._position_sample
                if position_sample is None
                else self._clamp_position(position_sample)
            )
            return MeterView(
                snapshot=self._snapshot,
                playback_position_sample=position,
                current_block_index=self._block_index_for_position(position),
                playing=self._playing,
            )

    def play(self) -> None:
        with self._lock:
            self._ensure_open()
            self._playing = True

    def pause(self) -> None:
        with self._lock:
            self._ensure_open()
            self._playing = False
            self._release_temporary_handles()

    @property
    def playing(self) -> bool:
        with self._lock:
            return self._playing

    @property
    def position_sample(self) -> int:
        with self._lock:
            return self._position_sample

    def advance(self, sample_count: int) -> int:
        if sample_count < 0:
            raise InvalidParameterError("sample advance must be non-negative")
        with self._lock:
            self._ensure_open()
            target = self._clamp_position(self._position_sample + sample_count)
            self._position_sample = target
            if target >= self.audio.frame_count:
                self._playing = False
            self.commit_to(target)
            return target

    def seek(self, position_sample: int) -> int:
        with self._lock:
            self._ensure_open()
            self._position_sample = self._clamp_position(position_sample)
            self._release_temporary_handles()
            self.commit_to(self._position_sample)
            return self._position_sample

    def restart(self) -> int:
        """Restart playback while retaining the last committed snapshot."""
        return self.seek(0)

    def commit_to(self, position_sample: int) -> MeterSnapshot:
        with self._lock:
            self._ensure_open()
            target = self._clamp_position(position_sample)
            target_count = self._committed_count_for_position(target)
            if target_count <= len(self._measurements):
                return self._snapshot
            new_measurements = [
                self._cache.measurement(index, self.params)
                for index in range(len(self._measurements), target_count)
            ]
            new_blocks = tuple(
                block_result_from_measurement(
                    measurement,
                    self.params,
                    self.parameter_version,
                    self._integrated.push(measurement),
                )
                for measurement in new_measurements
            )
            previous_count = len(self._blocks)
            self._measurements = [*self._measurements, *new_measurements]
            self._blocks = (*self._blocks, *new_blocks)
            self._publish(
                reason="commit",
                affected_blocks=tuple(
                    block.index for block in self._blocks[previous_count:]
                ),
                cache_stats={
                    "measurement_hits": self._cache.measurement_hits,
                    "measurement_misses": self._cache.measurement_misses,
                    "recomputed_measurement_blocks": len(new_measurements),
                    "reused_measurement_blocks": len(self._measurements)
                    - len(new_measurements),
                    "affected_blocks": len(new_measurements),
                },
            )
            return self._snapshot

    def change_parameters(
        self,
        updates: Mapping[str, Any] | None = None,
        **kwargs: Any,
    ) -> MeterSnapshot:
        changes = dict(updates or {})
        changes.update(kwargs)
        with self._lock:
            self._ensure_open()
            new_params = self.params.with_updates(
                channel_count=self.audio.channels, **changes
            )
            if new_params.digest() == self.params.digest():
                return self._snapshot
            old_params = self.params
            new_version = self.parameter_version + 1
            before_hits = self._cache.measurement_hits
            before_misses = self._cache.measurement_misses
            new_measurements = [
                self._cache.measurement(index, new_params)
                for index in range(len(self._measurements))
            ]
            recomputed_measurements = self._cache.measurement_misses - before_misses
            measurement_changed = (
                old_params.measurement_signature() != new_params.measurement_signature()
            )
            points = integrated_series(
                new_measurements,
                new_params.absolute_gate_lufs,
                new_params.relative_gate_lu,
            )
            changed_fields = _parameter_diff(old_params, new_params)
            new_blocks: list[BlockResult] = []
            affected: list[int] = []
            for old_block, measurement, point in zip(
                self._blocks, new_measurements, points
            ):
                exceeds = (
                    measurement.true_peak.dbtp is not None
                    and measurement.true_peak.dbtp > new_params.true_peak_limit_dbtp
                )
                if measurement_changed:
                    block = BlockResult(
                        index=measurement.index,
                        start_sample=measurement.start_sample,
                        end_sample=measurement.end_sample,
                        sync_marker=measurement.sync_marker,
                        content_hash=measurement.content_hash,
                        parameter_version=new_version,
                        mean_square=measurement.mean_square,
                        momentary_lufs=measurement.momentary_lufs,
                        short_lufs=measurement.short_lufs,
                        integrated_lufs=point.integrated_lufs,
                        true_peak_dbtp=measurement.true_peak.dbtp,
                        true_peak_position_sample=measurement.true_peak.position_sample,
                        true_peak_channel=measurement.true_peak.channel,
                        peak_exceeded=exceeds,
                        measurement_hash=measurement.measurement_hash,
                    )
                else:
                    block = old_block
                    if block.peak_exceeded != exceeds:
                        block = replace(
                            block,
                            peak_exceeded=exceeds,
                            parameter_version=new_version,
                        )
                    if block.integrated_lufs != point.integrated_lufs:
                        block = replace(
                            block,
                            integrated_lufs=point.integrated_lufs,
                            parameter_version=new_version,
                        )
                if block is not old_block:
                    affected.append(block.index)
                new_blocks.append(block)

            self.params = new_params
            self.parameter_version = new_version
            self._measurements = new_measurements
            self._blocks = tuple(new_blocks)
            self._integrated = IntegratedTracker(
                new_params.absolute_gate_lufs,
                new_params.relative_gate_lu,
            )
            for measurement in new_measurements:
                self._integrated.push(measurement)
            entry = ParameterHistoryEntry(
                version=new_version,
                previous_version=new_version - 1,
                reason="parameter_change",
                changed_fields=changed_fields,
                affected_blocks=tuple(affected),
                committed_blocks_at_change=len(self._blocks),
                params=new_params.as_dict(),
            )
            self._history = (*self._history, entry)
            self._publish(
                reason="parameter_change",
                affected_blocks=tuple(affected),
                cache_stats={
                    "measurement_hits": self._cache.measurement_hits - before_hits,
                    "measurement_misses": recomputed_measurements,
                    "recomputed_measurement_blocks": recomputed_measurements,
                    "reused_measurement_blocks": len(self._measurements)
                    - recomputed_measurements,
                    "affected_blocks": len(affected),
                },
            )
            return self._snapshot

    def close(self) -> MeterSnapshot:
        with self._lock:
            if not self._closed:
                self._playing = False
                self._release_temporary_handles()
                self._closed = True
                self._manager._release(self)
            return self._snapshot

    @property
    def closed(self) -> bool:
        with self._lock:
            return self._closed

    def _publish(
        self,
        *,
        reason: str,
        affected_blocks: tuple[int, ...],
        cache_stats: dict[str, Any],
    ) -> None:
        self._snapshot_version += 1
        committed_until = self._blocks[-1].end_sample if self._blocks else 0
        self._snapshot = MeterSnapshot(
            version=self._snapshot_version,
            parameter_version=self.parameter_version,
            params=self.params,
            blocks=self._blocks,
            committed_until_sample=committed_until,
            parameter_history=self._history,
            cache_stats=dict(cache_stats),
            affected_blocks=affected_blocks,
            publish_reason=reason,
        )

    def _committed_count_for_position(self, position: int) -> int:
        count = self._cache.block_count(self.params)
        if count == 0:
            return 0
        context = self._cache.context(self.params)
        eligible = 0
        for start, end in context.block_bounds:
            if end <= position:
                eligible += 1
            else:
                break
        return eligible

    def _block_index_for_position(self, position: int) -> int:
        count = self._cache.block_count(self.params)
        if count == 0:
            return 0
        if position < self.params.block_samples:
            return 0
        return min(
            count - 1,
            (position - self.params.block_samples) // self.params.hop_samples + 1,
        )

    def _clamp_position(self, position: int) -> int:
        return max(0, min(int(position), self.audio.frame_count))

    def _ensure_open(self) -> None:
        if self._closed:
            raise InvalidParameterError("analysis session is closed")

    @staticmethod
    def _release_temporary_handles() -> None:
        # WAV contents are fully read into memory and no audio device is opened.
        return None


class SessionManager:
    """One active session per source content hash."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active: dict[str, MeterSession] = {}

    def open_session(
        self,
        audio: AudioBuffer,
        params: AnalysisParameters,
    ) -> MeterSession:
        with self._lock:
            if audio.source_sha256 in self._active:
                raise BusyError(
                    "an analysis session is already active for this material",
                    source_sha256=audio.source_sha256,
                )
            session = MeterSession(audio, params, self)
            self._active[audio.source_sha256] = session
            return session

    def _release(self, session: MeterSession) -> None:
        with self._lock:
            current = self._active.get(session.audio.source_sha256)
            if current is session:
                self._active.pop(session.audio.source_sha256, None)

    @property
    def active_count(self) -> int:
        with self._lock:
            return len(self._active)


def _parameter_diff(
    old: AnalysisParameters,
    new: AnalysisParameters,
) -> dict[str, Any]:
    old_dict = old.as_dict()
    new_dict = new.as_dict()
    return {
        key: {"old": old_dict[key], "new": new_dict[key]}
        for key in old_dict
        if old_dict[key] != new_dict[key]
    }
