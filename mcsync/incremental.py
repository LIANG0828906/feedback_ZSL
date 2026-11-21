"""Incremental stage caches with atomic snapshot publication."""

from __future__ import annotations

import copy
import threading
from dataclasses import asdict, is_dataclass
from dataclasses import dataclass, field
from typing import Any, Iterable

import numpy as np

from .delays import (
    SegmentWindow,
    estimate_segment_delay,
    segment_windows,
)
from .diagnostics import diagnose_all_channels
from .errors import (
    E_ALIGNMENT_FAILED,
    E_RESIDUAL_THRESHOLD,
    Issue,
    deduplicate_issues,
)
from .fitting import compare_drift_fits, fit_channel_drift
from .markers import MarkerDetection, MarkerDetectionSet, detect_all_markers
from .models import (
    CompensationParams,
    DriftDiagnosis,
    DriftFit,
    Marker,
    PipelineConfig,
    ResidualSummary,
    SegmentDelay,
    SyncReport,
)
from .pipeline import (
    _layout,
    _parameter_version,
    _residual_threshold_issues,
)
from .resample import (
    build_residual_summary,
    compensated_length,
    concatenate_channel_chunks,
    resample_channel_range,
)
from .utils import sha256_array, sha256_json
from .wavio import AudioData


@dataclass
class CacheEntry:
    key: str
    content_sha256: str
    value: Any

    @classmethod
    def create(cls, key: str, value: Any) -> "CacheEntry":
        content_hash = _value_hash(value)
        return cls(key=key, content_sha256=content_hash, value=value)

    def valid(self) -> bool:
        return _value_hash(self.value) == self.content_sha256


def _value_hash(value: Any) -> str:
    if isinstance(value, np.ndarray):
        return sha256_array(value)
    if hasattr(value, "to_dict"):
        return sha256_json(value.to_dict())
    if is_dataclass(value):
        return sha256_json(asdict(value))
    return sha256_json(value)


@dataclass(frozen=True)
class PublishedSnapshot:
    revision: int
    config: PipelineConfig
    report: SyncReport
    compensated_audio: AudioData
    residual_summary: ResidualSummary
    marker_detection: MarkerDetectionSet
    issues: tuple[Issue, ...]
    parameter_version: str
    reuse_report: dict[str, Any]
    trace: tuple[dict[str, Any], ...]

    def query(self) -> "PublishedSnapshot":
        return self


@dataclass
class UpdateResult:
    snapshot: PublishedSnapshot
    reuse_report: dict[str, Any]


def _marker_key(
    audio_hash: str,
    channel: int,
    config: PipelineConfig,
) -> str:
    return sha256_json(
        {
            "audio": audio_hash,
            "channel": channel,
            "reference_channel": config.reference_channel,
            "marker": config.marker.to_dict(),
        }
    )


def _segment_key(
    *,
    audio_hash: str,
    channel: int,
    reference_channel: int,
    window: SegmentWindow,
    reference_markers: tuple[Marker, ...],
    channel_markers: tuple[Marker, ...],
    config: PipelineConfig,
) -> str:
    return sha256_json(
        {
            "audio": audio_hash,
            "channel": channel,
            "reference_channel": reference_channel,
            "window": {
                "index": window.index,
                "start": window.start_sample,
                "end": window.end_sample,
            },
            "reference_markers": [
                marker.to_dict() for marker in reference_markers
            ],
            "channel_markers": [marker.to_dict() for marker in channel_markers],
            "segmentation": config.segmentation.to_dict(),
        }
    )


def _fit_key(
    *,
    channel: int,
    segments: Iterable[SegmentDelay],
    config: PipelineConfig,
    sample_rate: int,
) -> str:
    return sha256_json(
        {
            "channel": channel,
            "segments": [segment.to_dict() for segment in segments],
            "fit": config.fit.to_dict(),
            "sample_rate": sample_rate,
            "reference_channel": config.reference_channel,
        }
    )


def _chunk_key(
    *,
    audio_hash: str,
    channel: int,
    fit: DriftFit,
    mode: str,
    sinc_half_taps: int,
    output_start: int,
    output_end: int,
) -> str:
    return sha256_json(
        {
            "audio": audio_hash,
            "channel": channel,
            "fit": fit.to_dict(),
            "mode": mode,
            "sinc_half_taps": sinc_half_taps,
            "output_start": output_start,
            "output_end": output_end,
        }
    )


def _copy_config(config: PipelineConfig) -> PipelineConfig:
    return config.clone()


class IncrementalSyncEngine:
    def __init__(
        self,
        audio: AudioData,
        config: PipelineConfig | None = None,
    ) -> None:
        self._audio = audio
        self._audio_hash = audio.content_hash()
        self._config = _copy_config(config or PipelineConfig())
        self._config.validate()
        self._lock = threading.RLock()
        self._update_lock = threading.RLock()
        self._revision = 0
        self._snapshot: PublishedSnapshot | None = None
        self._marker_cache: dict[str, CacheEntry] = {}
        self._segment_cache: dict[str, CacheEntry] = {}
        self._fit_cache: dict[str, CacheEntry] = {}
        self._chunk_cache: dict[str, CacheEntry] = {}
        self._build(first=True)

    @property
    def audio(self) -> AudioData:
        return self._audio

    @property
    def config(self) -> PipelineConfig:
        with self._lock:
            return self._config

    def snapshot(self) -> PublishedSnapshot:
        with self._lock:
            if self._snapshot is None:
                raise RuntimeError("no snapshot has been published")
            return self._snapshot

    query = snapshot

    def update(
        self,
        config: PipelineConfig | None = None,
        *,
        fit_overrides: dict[int, DriftFit] | None = None,
    ) -> UpdateResult:
        with self._update_lock:
            new_config = _copy_config(config or self._config)
            new_config.validate()
            builder = copy.copy(self)
            outcome = builder._build(
                config=new_config,
                fit_overrides=fit_overrides,
                first=False,
            )
            with self._lock:
                self._marker_cache = builder._marker_cache
                self._segment_cache = builder._segment_cache
                self._fit_cache = builder._fit_cache
                self._chunk_cache = builder._chunk_cache
                self._config = builder._config
                self._revision = builder._revision
                self._snapshot = builder._snapshot
            return outcome

    def update_parameters(
        self,
        *,
        marker_config: Any | None = None,
        segment_length_samples: int | None = None,
        reference_channel: int | None = None,
        active_channels: tuple[int, ...] | None = None,
        mode: str | None = None,
        fit_config: Any | None = None,
        fit_overrides: dict[int, DriftFit] | None = None,
    ) -> UpdateResult:
        with self._update_lock:
            base = self._config.clone()
            values: dict[str, Any] = {
                "reference_channel": base.reference_channel,
                "active_channels": base.active_channels,
                "marker": base.marker,
                "segmentation": base.segmentation,
                "fit": base.fit,
                "compensation": base.compensation,
                "diagnosis": base.diagnosis,
            }
            if marker_config is not None:
                values["marker"] = marker_config
            if segment_length_samples is not None:
                values["segmentation"] = type(base.segmentation)(
                    segment_length_samples=int(segment_length_samples),
                    delay_search_samples=base.segmentation.delay_search_samples,
                    min_markers_per_segment=base.segmentation.min_markers_per_segment,
                    correlation_window_samples=base.segmentation.correlation_window_samples,
                    refine_search_samples=base.segmentation.refine_search_samples,
                )
            if reference_channel is not None:
                values["reference_channel"] = int(reference_channel)
            if active_channels is not None:
                values["active_channels"] = tuple(active_channels)
            if mode is not None:
                values["compensation"] = type(base.compensation)(
                    mode=mode,
                    tolerance_samples=base.compensation.tolerance_samples,
                    sinc_half_taps=base.compensation.sinc_half_taps,
                    parameter_version=base.compensation.parameter_version,
                )
            if fit_config is not None:
                values["fit"] = fit_config
            new_config = PipelineConfig(**values)
            new_config.validate()
            return self.update(new_config, fit_overrides=fit_overrides)

    def _ensure_markers(
        self,
        config: PipelineConfig,
        channels: tuple[int, ...],
        *,
        marker_cache: dict[str, CacheEntry],
    ) -> tuple[MarkerDetectionSet, list[dict[str, Any]]]:
        keys = {
            channel: _marker_key(self._audio_hash, channel, config)
            for channel in channels
        }
        reused: list[int] = []
        recomputed: list[int] = []
        detections: dict[int, MarkerDetection] = {}
        stale: list[int] = []
        for channel in channels:
            entry = marker_cache.get(keys[channel])
            if entry is not None and entry.valid():
                detections[channel] = entry.value
                reused.append(channel)
            else:
                stale.append(channel)
        if stale:
            fresh = detect_all_markers(
                self._audio,
                config.marker,
                reference_channel=config.reference_channel,
                channels=channels,
            )
            for channel in channels:
                if channel in reused:
                    continue
                detection = fresh.channels[channel]
                detections[channel] = detection
                marker_cache[keys[channel]] = CacheEntry.create(
                    keys[channel], detection
                )
                recomputed.append(channel)
        issues = [
            issue
            for channel in sorted(detections)
            for issue in detections[channel].issues
        ]
        detection_set = MarkerDetectionSet(
            channels=detections,
            issues=tuple(issues),
            reference_channel=config.reference_channel,
        )
        trace = [
            {
                "stage": "markers",
                "reused_channels": sorted(reused),
                "recomputed_channels": sorted(recomputed),
            }
        ]
        return detection_set, trace

    def _segment_channel(
        self,
        channel: int,
        config: PipelineConfig,
        marker_detection: MarkerDetectionSet,
        windows: tuple[SegmentWindow, ...],
        *,
        segment_cache: dict[str, CacheEntry],
    ) -> tuple[list[SegmentDelay], list[dict[str, Any]], list[dict[str, Any]]]:
        output: list[SegmentDelay] = []
        reused: list[dict[str, Any]] = []
        recomputed: list[dict[str, Any]] = []
        previous_delay = 0.0
        previous_slope = 0.0
        for window in windows:
            reference_markers = tuple(
                marker
                for marker in marker_detection.markers_for(
                    config.reference_channel
                )
                if window.start_sample <= marker.arrival_sample < window.end_sample
            )
            channel_markers = tuple(
                marker
                for marker in marker_detection.markers_for(channel)
                if window.start_sample <= marker.arrival_sample < window.end_sample
            )
            key = _segment_key(
                audio_hash=self._audio_hash,
                channel=channel,
                reference_channel=config.reference_channel,
                window=window,
                reference_markers=reference_markers,
                channel_markers=channel_markers,
                config=config,
            )
            entry = segment_cache.get(key)
            identity = {
                "channel": channel,
                "segment_index": window.index,
                "content_sha256": entry.content_sha256 if entry else None,
            }
            if entry is not None and entry.valid():
                segment = entry.value
                reused.append(identity)
            else:
                if channel == config.reference_channel:
                    segment = SegmentDelay(
                        channel=channel,
                        segment_index=window.index,
                        start_sample=window.start_sample,
                        end_sample=window.end_sample,
                        delay_samples=0.0,
                        peak_correlation=1.0,
                        ci_low_samples=0.0,
                        ci_high_samples=0.0,
                        marker_count=len(channel_markers),
                        method="reference",
                    )
                else:
                    segment = estimate_segment_delay(
                        self._audio.channel(config.reference_channel),
                        self._audio.channel(channel),
                        channel=channel,
                        window=window,
                        reference_markers=reference_markers,
                        channel_markers=channel_markers,
                        config=config.segmentation,
                        sample_rate=self._audio.sample_rate,
                        fallback_delay=previous_delay
                        + previous_slope
                        * config.segmentation.segment_length_samples,
                    )
                entry = CacheEntry.create(key, segment)
                segment_cache[key] = entry
                identity["content_sha256"] = entry.content_sha256
                recomputed.append(identity)
            output.append(segment)
            if window.index >= 1:
                previous_slope = segment.delay_samples - previous_delay
            previous_delay = segment.delay_samples
        return output, reused, recomputed

    def _build(
        self,
        config: PipelineConfig | None = None,
        *,
        fit_overrides: dict[int, DriftFit] | None = None,
        first: bool,
    ) -> UpdateResult:
        new_config = _copy_config(config or self._config)
        if new_config.active_channels is None:
            channels = tuple(range(self._audio.channel_count))
        else:
            channels = tuple(sorted(set(new_config.active_channels)))
        if new_config.reference_channel not in channels:
            channels = (new_config.reference_channel,) + channels
        invalid = [
            channel
            for channel in channels
            if channel < 0 or channel >= self._audio.channel_count
        ]
        if invalid:
            from .errors import E_CHANNEL_INVALID, SyncError

            raise SyncError(
                E_CHANNEL_INVALID,
                f"active channel indices out of range: {invalid}",
                details={"channel_count": self._audio.channel_count},
            )
        marker_cache = dict(self._marker_cache)
        segment_cache = dict(self._segment_cache)
        fit_cache = dict(self._fit_cache)
        chunk_cache = dict(self._chunk_cache)
        old_snapshot = self._snapshot
        old_channels = (
            set(old_snapshot.marker_detection.channels)
            if old_snapshot is not None
            else set()
        )
        marker_detection, marker_trace = self._ensure_markers(
            new_config, channels, marker_cache=marker_cache
        )
        windows = segment_windows(
            self._audio.frame_count, new_config.segmentation
        )
        all_segments: list[SegmentDelay] = []
        reused_segments: list[dict[str, Any]] = []
        recomputed_segments: list[dict[str, Any]] = []
        for channel in channels:
            segments, reused, recomputed = self._segment_channel(
                channel,
                new_config,
                marker_detection,
                windows,
                segment_cache=segment_cache,
            )
            all_segments.extend(segments)
            reused_segments.extend(reused)
            recomputed_segments.extend(recomputed)
        fits: list[DriftFit] = []
        reused_fits: list[int] = []
        recomputed_fits: list[int] = []
        for channel in channels:
            channel_segments = [
                segment for segment in all_segments if segment.channel == channel
            ]
            key = _fit_key(
                channel=channel,
                segments=channel_segments,
                config=new_config,
                sample_rate=self._audio.sample_rate,
            )
            entry = fit_cache.get(key)
            override = (fit_overrides or {}).get(channel)
            if override is not None:
                fit = override
                fit_cache[key] = CacheEntry.create(key, fit)
                recomputed_fits.append(channel)
            elif entry is not None and entry.valid():
                fit = entry.value
                reused_fits.append(channel)
            else:
                fit = fit_channel_drift(
                    channel_segments,
                    channel=channel,
                    sample_rate=self._audio.sample_rate,
                    config=new_config.fit,
                    reference_channel=new_config.reference_channel,
                )
                fit_cache[key] = CacheEntry.create(key, fit)
                recomputed_fits.append(channel)
            fits.append(fit)
        diagnoses, diagnosis_issues = diagnose_all_channels(
            all_segments, fits, new_config.diagnosis
        )
        fit_comparison = (
            compare_drift_fits(
                old_snapshot.report.drift_fits,
                fits,
                all_segments,
            )
            if old_snapshot is not None
            and (
                old_snapshot.config.fit.to_dict()
                != new_config.fit.to_dict()
                or bool(fit_overrides)
            )
            else None
        )
        fit_by_channel = {fit.channel: fit for fit in fits}
        output_length = compensated_length(self._audio.frame_count, fits)
        chunk_rows: dict[int, list[tuple[int, int, np.ndarray]]] = {
            channel: [] for channel in channels
        }
        reused_chunks: list[dict[str, Any]] = []
        recomputed_chunks: list[dict[str, Any]] = []
        for channel in channels:
            fit = fit_by_channel[channel]
            for window in windows:
                output_start = min(window.start_sample, output_length)
                output_end = min(window.end_sample, output_length)
                if output_end <= output_start:
                    continue
                key = _chunk_key(
                    audio_hash=self._audio_hash,
                    channel=channel,
                    fit=fit,
                    mode=new_config.compensation.mode,
                    sinc_half_taps=new_config.compensation.sinc_half_taps,
                    output_start=output_start,
                    output_end=output_end,
                )
                entry = chunk_cache.get(key)
                identity = {
                    "channel": channel,
                    "segment_index": window.index,
                    "output_start": output_start,
                    "output_end": output_end,
                }
                if entry is not None and entry.valid():
                    values = entry.value
                    identity["content_sha256"] = entry.content_sha256
                    reused_chunks.append(identity)
                else:
                    values = resample_channel_range(
                        self._audio.channel(channel),
                        fit,
                        mode=new_config.compensation.mode,
                        output_start=output_start,
                        output_end=output_end,
                        sinc_half_taps=new_config.compensation.sinc_half_taps,
                    )
                    entry = CacheEntry.create(key, values)
                    chunk_cache[key] = entry
                    identity["content_sha256"] = entry.content_sha256
                    recomputed_chunks.append(identity)
                chunk_rows[channel].append(
                    (output_start, output_end, values)
                )
        matrix = np.zeros((len(channels), output_length), dtype=np.float64)
        for row, channel in enumerate(channels):
            matrix[row] = concatenate_channel_chunks(
                chunk_rows[channel], output_length
            )
        compensated = AudioData(
            samples=matrix,
            sample_rate=self._audio.sample_rate,
            channel_names=tuple(
                self._audio.channel_names[channel] for channel in channels
            ),
        )
        residual_summary, residual_detection, residual_issues = (
            build_residual_summary(
                compensated,
                reference_channel=new_config.reference_channel,
                marker_config=new_config.marker,
                tolerance_samples=new_config.compensation.tolerance_samples,
                channels=channels,
            )
        )
        issues: list[Issue] = list(marker_detection.issues)
        issues.extend(diagnosis_issues)
        issues.extend(_residual_threshold_issues(fits))
        issues.extend(residual_issues)
        if (
            residual_summary.max_abs_delay_samples
            > new_config.compensation.tolerance_samples
        ):
            issues.append(
                Issue(
                    code=E_ALIGNMENT_FAILED,
                    message=(
                        "compensated marker residuals exceed the declared "
                        "tolerance"
                    ),
                    channel=residual_summary.worst_channel,
                    details={
                        "max_abs_delay_samples": residual_summary.max_abs_delay_samples,
                        "tolerance_samples": new_config.compensation.tolerance_samples,
                    },
                )
            )
        parameter_version = _parameter_version(self._audio, new_config, fits)
        compensation = CompensationParams(
            mode=new_config.compensation.mode,
            parameter_version=parameter_version,
            tolerance_samples=new_config.compensation.tolerance_samples,
            sinc_half_taps=new_config.compensation.sinc_half_taps,
            output_length=output_length,
        )
        issues = list(deduplicate_issues(issues))
        report = SyncReport(
            sample_rate=self._audio.sample_rate,
            reference_channel=new_config.reference_channel,
            channel_layout=_layout(
                self._audio, channels, new_config.reference_channel
            ),
            markers=marker_detection.markers,
            segment_delays=tuple(all_segments),
            drift_fits=tuple(fits),
            diagnoses=diagnoses,
            compensation=compensation,
            residual_summary=residual_summary,
            segmentation=new_config.segmentation,
            fitting=new_config.fit,
            parameter_version=parameter_version,
            schema_version="v3",
            source_version="v3",
            issues=tuple(issues),
            provenance={
                "markers": "incremental:detected_or_reused",
                "segment_delays": "incremental:segmented_normalized_xcorr",
                "drift_fits": "incremental:first_order_robust_fit",
                "diagnoses": "incremental:continuity_and_jump_analysis",
                "compensation": "incremental:fractional_delay_chunks",
                "residual_summary": "incremental:post_compensation_marker_detection",
            },
            integrity={},
        )
        report.with_integrity()
        report.integrity["input_audio_sha256"] = self._audio_hash
        removed_channels = sorted(old_channels - set(channels))
        reused_channel_list = sorted(
            {
                int(entry["channel"])
                for entry in reused_chunks
            }
            | set(reused_fits)
        )
        recomputed_channel_list = sorted(
            {
                int(entry["channel"])
                for entry in recomputed_chunks
            }
            | set(recomputed_fits)
        )
        reuse_report = {
            "schema_version": "incremental-reuse-v1",
            "previous_revision": self._revision if old_snapshot else None,
            "revision": self._revision + 1,
            "reused_channels": reused_channel_list,
            "recomputed_channels": recomputed_channel_list,
            "removed_channels": removed_channels,
            "reused_marker_channels": marker_trace[0]["reused_channels"],
            "recomputed_marker_channels": marker_trace[0]["recomputed_channels"],
            "reused_segments": reused_segments,
            "recomputed_segments": recomputed_segments,
            "reused_fits": sorted(reused_fits),
            "recomputed_fits": sorted(recomputed_fits),
            "fit_parameter_comparison": fit_comparison,
            "reused_concat_chunks": reused_chunks,
            "recomputed_concat_chunks": recomputed_chunks,
            "unchanged_from_previous": {
                "marker_config": (
                    old_snapshot is not None
                    and old_snapshot.config.marker.to_dict()
                    == new_config.marker.to_dict()
                ),
                "segmentation": (
                    old_snapshot is not None
                    and old_snapshot.config.segmentation.to_dict()
                    == new_config.segmentation.to_dict()
                ),
                "reference_channel": (
                    old_snapshot is not None
                    and old_snapshot.config.reference_channel
                    == new_config.reference_channel
                ),
                "interpolation_mode": (
                    old_snapshot is not None
                    and old_snapshot.config.compensation.mode
                    == new_config.compensation.mode
                ),
                "fit_config": (
                    old_snapshot is not None
                    and old_snapshot.config.fit.to_dict()
                    == new_config.fit.to_dict()
                ),
            },
        }
        trace = (
            marker_trace[0],
            {
                "stage": "segments",
                "reused": len(reused_segments),
                "recomputed": len(recomputed_segments),
            },
            {
                "stage": "fit",
                "reused_channels": sorted(reused_fits),
                "recomputed_channels": sorted(recomputed_fits),
                "comparison_available": fit_comparison is not None,
            },
            {
                "stage": "compensation",
                "mode": new_config.compensation.mode,
                "reused_chunks": len(reused_chunks),
                "recomputed_chunks": len(recomputed_chunks),
            },
            {
                "stage": "publish",
                "parameter_version": parameter_version,
                "report_sha256": report.integrity["content_sha256"],
            },
        )
        snapshot = PublishedSnapshot(
            revision=self._revision + 1,
            config=new_config,
            report=report,
            compensated_audio=compensated,
            residual_summary=residual_summary,
            marker_detection=marker_detection,
            issues=tuple(issues),
            parameter_version=parameter_version,
            reuse_report=reuse_report,
            trace=trace,
        )
        self._marker_cache = marker_cache
        self._segment_cache = segment_cache
        self._fit_cache = fit_cache
        self._chunk_cache = chunk_cache
        self._config = new_config
        self._revision = snapshot.revision
        self._snapshot = snapshot
        return UpdateResult(snapshot=snapshot, reuse_report=reuse_report)

