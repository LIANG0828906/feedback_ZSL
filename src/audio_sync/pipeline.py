"""End-to-end synchronization pipeline with transactional publication."""

from __future__ import annotations

import bisect
import json
import math
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable

import numpy as np

from .cache import CacheBackend, CacheStats
from .delay import (
    delay_sequence_diff,
    estimate_segment,
    reference_segment,
    segment_bounds,
)
from .drift import fit_drift_model
from .errors import (
    E_CANCELLED,
    E_COMPENSATION_FAILED,
    E_DRIFT_UNKNOWN,
    E_INVALID_MARKERS,
    E_MARKER_MISSING,
    E_RESIDUAL_TOLERANCE,
    SyncError,
)
from .markers import detect_markers_reader
from .models import (
    ChannelResult,
    DriftModel,
    MarkerChannelResult,
    MarkerSpec,
    SegmentDelay,
    SyncConfig,
    content_hash,
    dataclass_to_dict,
    parse_marker_spec,
)
from .resample import compensate_segment
from .wavio import (
    AtomicWavSegmentWriter,
    WavReader,
    hash_file,
    write_json_atomic,
    write_npz_atomic,
)

ProgressCallback = Callable[[str, float, str], None]
CheckpointCallback = Callable[[str], None]


def _no_progress(phase: str, fraction: float, detail: str) -> None:
    return None


def _no_checkpoint(label: str) -> None:
    return None


def _next_power_of_two(value: int) -> int:
    return 1 << max(0, int(value - 1).bit_length())


def _strategy_decision(
    config: SyncConfig,
    duration_seconds: float,
    channel_count: int,
    frames: int,
    sample_rate: int,
    selected_bounds: list[tuple[int, int]],
) -> dict:
    auto_segmented = (
        duration_seconds >= config.duration_strategy_threshold_s
        or channel_count >= config.channel_strategy_threshold
    )
    if config.strategy == "auto":
        selected = "segmented" if auto_segmented else "full"
        reason = (
            f"duration {duration_seconds:.3f}s >= {config.duration_strategy_threshold_s:.1f}s"
            if duration_seconds >= config.duration_strategy_threshold_s
            else (
                f"channel count {channel_count} >= {config.channel_strategy_threshold}"
                if channel_count >= config.channel_strategy_threshold
                else "below auto thresholds"
            )
        )
    else:
        selected = config.strategy
        reason = "explicitly requested"

    if selected == "full":
        nfft = _next_power_of_two(2 * frames - 1)
        work = float(channel_count * nfft * max(1, math.log2(nfft)))
        segment_count = 1
        basis = "one FFT CCF per channel over the complete recording"
    else:
        work = 0.0
        for start, stop in selected_bounds:
            nfft = _next_power_of_two(2 * (stop - start) - 1)
            work += float(channel_count * nfft * max(1, math.log2(nfft)))
        segment_count = len(selected_bounds)
        basis = "one FFT CCF per channel per segment; model fit is O(channels * segments)"

    return {
        "selected": selected,
        "requested": config.strategy,
        "reason": reason,
        "auto_would_select_segmented": auto_segmented,
        "channel_count": channel_count,
        "duration_seconds": duration_seconds,
        "segment_count": segment_count,
        "thresholds": {
            "duration_seconds": config.duration_strategy_threshold_s,
            "channel_count": config.channel_strategy_threshold,
        },
        "estimated_cost": {
            "basis": basis,
            "fft_work_units": work,
            "sample_count": frames,
            "marker_detection_unchanged": True,
            "sample_count_preserved": True,
        },
    }


def _strategy_bounds(
    strategy: str,
    frames: int,
    sample_rate: int,
    segment_seconds: float,
) -> list[tuple[int, int]]:
    if strategy == "full":
        return [(0, frames)]
    return segment_bounds(frames, sample_rate, segment_seconds)


def _marker_errors(
    marker_results: dict[int, MarkerChannelResult],
    stage: str,
) -> list[dict]:
    errors: list[dict] = []
    for channel, result in sorted(marker_results.items()):
        confidence = {hit.marker_index: hit.confidence for hit in result.markers}
        for marker_index in result.missing_indices:
            errors.append(
                {
                    "code": E_MARKER_MISSING,
                    "message": (
                        f"channel {channel} marker {marker_index} was not detected "
                        f"at the required confidence"
                    ),
                    "details": {
                        "channel": channel,
                        "marker_index": marker_index,
                        "stage": stage,
                        "confidence": confidence.get(marker_index, 0.0),
                    },
                }
            )
    return errors


def _drift_errors(models: dict[int, DriftModel]) -> list[dict]:
    errors: list[dict] = []
    for channel, model in sorted(models.items()):
        if model.error_code == E_DRIFT_UNKNOWN:
            errors.append(
                {
                    "code": E_DRIFT_UNKNOWN,
                    "message": f"channel {channel} drift source cannot be classified",
                    "details": {
                        "channel": channel,
                        "candidates": list(model.candidates),
                        "evidence_segments": list(model.evidence_segments),
                        "fit_residual_rms": model.fit_residual_rms,
                    },
                }
            )
    return errors


def _residual_arrays(
    marker_results: dict[int, MarkerChannelResult],
    reference_channel: int,
    channels: tuple[int, ...],
    bounds: list[tuple[int, int]],
) -> tuple[dict[int, list[float | None]], dict[int, list[float | None]], dict[int, list[int]]]:
    starts = [start for start, _ in bounds]

    def segment_for(sample: float) -> int:
        index = bisect.bisect_right(starts, sample) - 1
        return max(0, min(len(bounds) - 1, index))

    reference_hits = {
        hit.marker_index: hit.arrival_sample
        for hit in marker_results[reference_channel].markers
        if hit.accepted and hit.arrival_sample is not None
    }
    rms_map: dict[int, list[float | None]] = {}
    max_map: dict[int, list[float | None]] = {}
    count_map: dict[int, list[int]] = {}
    for channel in channels:
        grouped: list[list[float]] = [[] for _ in bounds]
        for hit in marker_results[channel].markers:
            if not hit.accepted or hit.arrival_sample is None:
                continue
            reference_arrival = reference_hits.get(hit.marker_index)
            if reference_arrival is None:
                continue
            residual = float(abs(hit.arrival_sample - reference_arrival))
            grouped[segment_for(reference_arrival)].append(residual)
        rms_map[channel] = []
        max_map[channel] = []
        count_map[channel] = []
        for values in grouped:
            if values:
                array = np.asarray(values, dtype=np.float64)
                rms_map[channel].append(float(np.sqrt(np.mean(array * array))))
                max_map[channel].append(float(np.max(array)))
            else:
                rms_map[channel].append(None)
                max_map[channel].append(None)
            count_map[channel].append(len(values))
    return rms_map, max_map, count_map


def _expected_model_residual_bounds(
    marker_results: dict[int, MarkerChannelResult],
    reference_channel: int,
    channels: tuple[int, ...],
    models: dict[int, DriftModel],
) -> tuple[dict[int, float], float]:
    reference_hits = {
        hit.marker_index: hit.arrival_sample
        for hit in marker_results[reference_channel].markers
        if hit.accepted and hit.arrival_sample is not None
    }
    bounds: dict[int, float] = {}
    for channel in channels:
        if channel == reference_channel:
            bounds[channel] = 0.0
            continue
        residuals: list[float] = []
        for hit in marker_results[channel].markers:
            if not hit.accepted or hit.arrival_sample is None:
                continue
            reference_arrival = reference_hits.get(hit.marker_index)
            if reference_arrival is None:
                continue
            observed_delay = float(hit.arrival_sample - reference_arrival)
            predicted_delay = float(models[channel].delay_at(reference_arrival))
            residuals.append(abs(observed_delay - predicted_delay))
        if residuals:
            bounds[channel] = float(max(residuals) + 0.5)
        else:
            bounds[channel] = float((models[channel].fit_residual_rms or 0.0) + 0.5)
    return bounds, max(bounds.values(), default=0.5)


def _previous_interpolation_comparison(
    previous_payload: dict | None,
    source_sha: str,
    current_mode: str,
    current_after_max: dict[int, list[float | None]],
) -> dict:
    if not previous_payload:
        return {"available": False, "reason": "no committed predecessor"}
    previous_source = previous_payload.get("input", {}).get("sha256")
    previous_comp = previous_payload.get("compensation", {})
    previous_mode = previous_comp.get("mode")
    if previous_source != source_sha:
        return {"available": False, "reason": "predecessor used different source material"}
    if previous_mode == current_mode:
        return {
            "available": False,
            "reason": "predecessor used the same interpolation mode",
            "mode": current_mode,
        }

    previous_channels = {
        int(item["channel"]): item for item in previous_comp.get("channels", [])
    }
    channels: list[dict] = []
    for channel, current_values in sorted(current_after_max.items()):
        old_item = previous_channels.get(channel, {})
        old_values = old_item.get("after_max", [])
        old_max = max((value for value in old_values if value is not None), default=None)
        new_max = max((value for value in current_values if value is not None), default=None)
        channels.append(
            {
                "channel": channel,
                "previous_after_max_samples": old_max,
                "current_after_max_samples": new_max,
                "max_improvement_samples": (
                    float(old_max - new_max)
                    if old_max is not None and new_max is not None
                    else None
                ),
            }
        )
    return {
        "available": True,
        "previous_mode": previous_mode,
        "current_mode": current_mode,
        "channels": channels,
        "summary": {
            "previous_worst_max_samples": max(
                (
                    item["previous_after_max_samples"]
                    for item in channels
                    if item["previous_after_max_samples"] is not None
                ),
                default=None,
            ),
            "current_worst_max_samples": max(
                (
                    item["current_after_max_samples"]
                    for item in channels
                    if item["current_after_max_samples"] is not None
                ),
                default=None,
            ),
        },
    }


def run_pipeline(
    wav_path: str | Path,
    markers_path: str | Path,
    output_root: str | Path,
    config: SyncConfig,
    previous_payload: dict | None = None,
    progress: ProgressCallback | None = None,
    checkpoint: CheckpointCallback | None = None,
    run_id: str | None = None,
) -> dict:
    """Run one complete synchronization and return an unversioned report payload."""

    config.validate()
    progress = progress or _no_progress
    checkpoint = checkpoint or _no_checkpoint
    started = time.perf_counter()
    root = Path(output_root).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    run_id = run_id or f"{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-{uuid.uuid4().hex[:8]}"
    run_dir = root / "runs" / run_id
    run_dir.mkdir(parents=True, exist_ok=True)
    cache = CacheBackend(root / "cache")
    stats = CacheStats()
    errors: list[dict] = []
    new_segment_keys: list[str] = []
    channel_cache_stats: dict[int, dict[str, int]] = {}

    checkpoint("start")
    progress("read", 0.01, "inspecting WAV and marker description")
    markers_file = Path(markers_path).expanduser().resolve()
    try:
        marker_payload = json.loads(markers_file.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise SyncError(E_INVALID_MARKERS, f"cannot read marker description: {exc}") from exc
    spec = parse_marker_spec(marker_payload)

    with WavReader(wav_path) as reader:
        if spec.sample_rate is not None and spec.sample_rate != reader.sample_rate:
            raise SyncError(
                E_INVALID_MARKERS,
                f"marker sample rate {spec.sample_rate} does not match WAV {reader.sample_rate}",
            )
        if spec.channel_count is not None and spec.channel_count != reader.channels:
            raise SyncError(
                E_INVALID_MARKERS,
                f"marker channel count {spec.channel_count} does not match WAV {reader.channels}",
            )
        if reader.frames < reader.sample_rate // 10:
            raise SyncError(
                E_INVALID_MARKERS,
                "recording is too short for reliable segmented synchronization",
            )

        channels = tuple(config.enabled_channels or range(reader.channels))
        if not channels:
            raise SyncError(E_INVALID_MARKERS, "no channels are enabled")
        if any(channel >= reader.channels for channel in channels):
            raise SyncError(
                E_INVALID_MARKERS,
                "enabled channel is outside the WAV channel range",
            )
        if config.reference_channel not in channels:
            raise SyncError(
                E_INVALID_MARKERS,
                "reference channel must be one of the enabled channels",
            )

        duration = reader.frames / reader.sample_rate
        preliminary_bounds = segment_bounds(
            reader.frames, reader.sample_rate, config.segment_seconds
        )
        strategy_decision = _strategy_decision(
            config,
            duration,
            len(channels),
            reader.frames,
            reader.sample_rate,
            preliminary_bounds,
        )
        selected_strategy = strategy_decision["selected"]
        bounds = _strategy_bounds(
            selected_strategy,
            reader.frames,
            reader.sample_rate,
            config.segment_seconds,
        )
        strategy_decision["segment_count"] = len(bounds)
        method = "whole_recording" if selected_strategy == "full" else "segmented"

        marker_results: dict[int, MarkerChannelResult] = {}
        marker_signature = spec.signature()
        for channel_index, channel in enumerate(channels):
            checkpoint(f"markers:channel-{channel}")
            progress(
                "markers",
                0.03 + 0.12 * channel_index / len(channels),
                f"detecting markers in channel {channel}",
            )
            key = cache.marker_key(
                reader.info.sha256,
                channel,
                marker_signature,
                config.marker_min_snr,
            )
            result = cache.get_markers(key, stats)
            if result is None:
                result = detect_markers_reader(
                    reader,
                    channel,
                    spec,
                    min_snr=config.marker_min_snr,
                )
                cache.put_markers(key, result)
            marker_results[channel] = result
        errors.extend(_marker_errors(marker_results, "source"))

        delay_results: dict[int, tuple[SegmentDelay, ...]] = {}
        for channel_index, channel in enumerate(channels):
            checkpoint(f"delay:channel-{channel}")
            progress(
                "delay",
                0.16 + 0.25 * channel_index / len(channels),
                f"estimating channel {channel} delays in {len(bounds)} segment(s)",
            )
            before_hits = stats.delay_hits
            before_misses = stats.delay_misses
            estimates: list[SegmentDelay] = []
            for segment_index, (start, stop) in enumerate(bounds):
                checkpoint(f"delay:channel-{channel}:segment-{segment_index}")
                key = cache.delay_key(
                    reader.info.sha256,
                    channel,
                    config.reference_channel,
                    selected_strategy,
                    start,
                    stop,
                    config.max_lag_seconds,
                    config.min_segment_correlation,
                    method,
                )
                cached = cache.get_delays(key, stats)
                if cached is not None and len(cached) == 1:
                    estimate = cached[0]
                else:
                    if channel == config.reference_channel:
                        estimate = reference_segment(segment_index, start, stop)
                    else:
                        reference = reader.read_float(config.reference_channel, start, stop)
                        signal = reader.read_float(channel, start, stop)
                        estimate = estimate_segment(
                            reference=reference,
                            channel=signal,
                            sample_rate=reader.sample_rate,
                            segment_index=segment_index,
                            start_sample=start,
                            end_sample=stop,
                            max_lag_seconds=config.max_lag_seconds,
                            min_correlation=config.min_segment_correlation,
                            method=method,
                        )
                    cache.put_delays(key, (estimate,))
                estimates.append(estimate)
            delay_results[channel] = tuple(estimates)
            channel_cache_stats[channel] = {
                "delay_hits": stats.delay_hits - before_hits,
                "delay_misses": stats.delay_misses - before_misses,
            }

        drift_models: dict[int, DriftModel] = {}
        for channel_index, channel in enumerate(channels):
            checkpoint(f"drift:channel-{channel}")
            progress(
                "drift",
                0.42 + 0.12 * channel_index / len(channels),
                f"fitting channel {channel} drift model",
            )
            key = cache.fit_key(delay_results[channel], reader.sample_rate, config)
            model = cache.get_fit(key, stats)
            if model is None:
                model = fit_drift_model(
                    channel,
                    delay_results[channel],
                    reader.sample_rate,
                    config,
                )
                cache.put_fit(key, model)
            drift_models[channel] = model
        errors.extend(_drift_errors(drift_models))
        expected_bounds, expected_worst_bound = _expected_model_residual_bounds(
            marker_results,
            config.reference_channel,
            channels,
            drift_models,
        )

        aligned_path = run_dir / "aligned.wav"
        segment_hashes: dict[int, list[str]] = {channel: [] for channel in channels}
        compensation_failed = False
        try:
            with AtomicWavSegmentWriter(
                aligned_path,
                channels=len(channels),
                sample_rate=reader.sample_rate,
                total_frames=reader.frames,
            ) as writer:
                for segment_index, (start, stop) in enumerate(bounds):
                    checkpoint(f"compensate:segment-{segment_index}")
                    progress(
                        "compensate",
                        0.54 + 0.28 * segment_index / len(bounds),
                        f"compensating segment {segment_index + 1}/{len(bounds)}",
                    )
                    segment_arrays: list[np.ndarray] = []
                    for channel in channels:
                        key = cache.segment_key(
                            reader.info.sha256,
                            channel,
                            start,
                            stop,
                            drift_models[channel],
                            config.interpolation,
                            config.sinc_half_taps,
                        )
                        cached_segment = cache.get_segment(key, stats)
                        if cached_segment is None:
                            try:
                                if channel == config.reference_channel:
                                    corrected = reader.read_i16(channel, start, stop)
                                else:
                                    raw = reader.read_i16(channel, start, stop)
                                    corrected = compensate_segment(
                                        raw,
                                        drift_models[channel],
                                        global_start_sample=start,
                                        interpolation=config.interpolation,
                                        sinc_half_taps=config.sinc_half_taps,
                                    )
                                digest = cache.put_segment(key, corrected)
                                new_segment_keys.append(key)
                            except Exception as exc:
                                compensation_failed = True
                                raise SyncError(
                                    E_COMPENSATION_FAILED,
                                    (
                                        f"compensation failed for channel {channel}, "
                                        f"segment {segment_index}: {exc}"
                                    ),
                                    {
                                        "channel": channel,
                                        "segment_index": segment_index,
                                        "start_sample": start,
                                        "end_sample": stop,
                                    },
                                ) from exc
                        else:
                            corrected, digest = cached_segment
                        expected_length = stop - start
                        if corrected.size != expected_length:
                            compensation_failed = True
                            raise SyncError(
                                E_COMPENSATION_FAILED,
                                (
                                    f"compensated length mismatch for channel {channel}, "
                                    f"segment {segment_index}"
                                ),
                            )
                        segment_hashes[channel].append(digest)
                        segment_arrays.append(np.asarray(corrected, dtype=np.int16))
                    writer.write_segment(np.stack(segment_arrays, axis=0))
                writer.commit()
        except BaseException:
            for key in new_segment_keys:
                cache.discard_segment(key)
            raise

        progress("residuals", 0.84, "detecting markers in compensated audio")
        checkpoint("residuals:start")
        output_positions = {channel: index for index, channel in enumerate(channels)}
        with WavReader(aligned_path) as aligned_reader:
            after_marker_results: dict[int, MarkerChannelResult] = {}
            for channel in channels:
                checkpoint(f"residuals:channel-{channel}")
                after_marker_results[channel] = detect_markers_reader(
                    aligned_reader,
                    output_positions[channel],
                    spec,
                    min_snr=config.marker_min_snr,
                )

        original_after_errors = {
            (item["details"]["channel"], item["details"]["marker_index"])
            for item in _marker_errors(after_marker_results, "compensated")
        }
        source_missing = {
            (channel, marker_index)
            for channel, result in marker_results.items()
            for marker_index in result.missing_indices
        }
        new_missing = original_after_errors - source_missing
        if new_missing:
            for channel, marker_index in sorted(new_missing):
                errors.append(
                    {
                        "code": E_MARKER_MISSING,
                        "message": (
                            f"channel {channel} marker {marker_index} disappeared "
                            "after compensation"
                        ),
                        "details": {
                            "channel": channel,
                            "marker_index": marker_index,
                            "stage": "compensated",
                        },
                    }
                )

        before_rms, before_max, before_counts = _residual_arrays(
            marker_results,
            config.reference_channel,
            channels,
            bounds,
        )
        after_rms, after_max, after_counts = _residual_arrays(
            after_marker_results,
            config.reference_channel,
            channels,
            bounds,
        )

        channel_results: list[ChannelResult] = []
        heatmap_before_rms = np.full((len(channels), len(bounds)), np.nan, dtype=np.float64)
        heatmap_after_rms = np.full_like(heatmap_before_rms, np.nan)
        heatmap_before_max = np.full_like(heatmap_before_rms, np.nan)
        heatmap_after_max = np.full_like(heatmap_before_rms, np.nan)
        total_tolerance_ok = True
        for channel_index, channel in enumerate(channels):
            valid_after = [value for value in after_max[channel] if value is not None]
            tolerance_ok = bool(
                all(value <= config.tolerance_samples for value in valid_after)
                and not any(item[0] == channel for item in new_missing)
            )
            total_tolerance_ok = total_tolerance_ok and tolerance_ok
            if not tolerance_ok:
                errors.append(
                    {
                        "code": E_RESIDUAL_TOLERANCE,
                        "message": (
                            f"channel {channel} exceeds the {config.tolerance_samples:g}-sample "
                            "residual tolerance"
                        ),
                        "details": {
                            "channel": channel,
                            "tolerance_samples": config.tolerance_samples,
                            "measured_max_samples": max(valid_after, default=None),
                        },
                    }
                )
            for segment_index in range(len(bounds)):
                before_value = before_rms[channel][segment_index]
                after_value = after_rms[channel][segment_index]
                before_peak = before_max[channel][segment_index]
                after_peak = after_max[channel][segment_index]
                if before_value is not None:
                    heatmap_before_rms[channel_index, segment_index] = before_value
                if after_value is not None:
                    heatmap_after_rms[channel_index, segment_index] = after_value
                if before_peak is not None:
                    heatmap_before_max[channel_index, segment_index] = before_peak
                if after_peak is not None:
                    heatmap_after_max[channel_index, segment_index] = after_peak
            channel_results.append(
                ChannelResult(
                    channel=channel,
                    marker_result=marker_results[channel],
                    segment_delays=delay_results[channel],
                    drift=drift_models[channel],
                    segment_hashes=tuple(segment_hashes[channel]),
                    before_rms=tuple(before_rms[channel]),
                    after_rms=tuple(after_rms[channel]),
                    before_max=tuple(before_max[channel]),
                    after_max=tuple(after_max[channel]),
                    marker_counts_before=tuple(before_counts[channel]),
                    marker_counts_after=tuple(after_counts[channel]),
                    tolerance_ok=tolerance_ok,
                    cache_hits=0,
                    cache_misses=0,
                )
            )

        previous_sequences = (
            {
                channel: [
                    SegmentDelay(**item)
                    for item in channel_payload["segments"]
                ]
                for channel_payload in previous_payload.get("delay_estimation", {}).get(
                    "channels", []
                )
                for channel in [int(channel_payload["channel"])]
            }
            if previous_payload
            else {}
        )
        previous_source = (
            previous_payload.get("input", {}).get("sha256")
            if previous_payload
            else None
        )
        if previous_payload and previous_source != reader.info.sha256:
            change_summary = {
                "skipped": "predecessor used different source material",
                "previous_version": previous_payload.get("version"),
                "previous_content_hash": previous_payload.get("content_hash"),
            }
        else:
            change_summary = delay_sequence_diff(
                previous_sequences,
                delay_results,
                reader.sample_rate,
            )
            if previous_payload:
                change_summary["previous_version"] = previous_payload.get("version")
                change_summary["previous_content_hash"] = previous_payload.get(
                    "content_hash"
                )

        interpolation_comparison = _previous_interpolation_comparison(
            previous_payload,
            reader.info.sha256,
            config.interpolation,
            after_max,
        )

        measured_worst = max(
            (
                value
                for channel_values in after_max.values()
                for value in channel_values
                if value is not None
            ),
            default=None,
        )
        strategy_decision["estimated_residual_bound_samples"] = expected_worst_bound
        strategy_decision["expected_residual_bounds_per_channel"] = {
            str(channel): value for channel, value in expected_bounds.items()
        }
        strategy_decision["measured_residual_max_samples"] = measured_worst
        strategy_decision["elapsed_seconds"] = time.perf_counter() - started
        strategy_decision["two_strategies_comparable"] = True

        heatmap_path = run_dir / "residual_heatmap.npz"
        write_npz_atomic(
            heatmap_path,
            channel_ids=np.asarray(channels, dtype=np.int32),
            segment_starts=np.asarray([start for start, _ in bounds], dtype=np.int64),
            segment_ends=np.asarray([stop for start, stop in bounds], dtype=np.int64),
            before_rms=heatmap_before_rms,
            after_rms=heatmap_after_rms,
            before_max=heatmap_before_max,
            after_max=heatmap_after_max,
            marker_counts_before=np.asarray(
                [before_counts[channel] for channel in channels], dtype=np.int32
            ),
            marker_counts_after=np.asarray(
                [after_counts[channel] for channel in channels], dtype=np.int32
            ),
        )

        report: dict = {
            "schema_version": "1.0",
            "run_id": run_id,
            "generated_at_utc": datetime.now(timezone.utc).isoformat(),
            "input": {
                **reader.info.to_dict(),
                "markers_path": str(markers_file),
                "markers_sha256": hash_file(markers_file),
                "enabled_channels": list(channels),
                "reference_channel": config.reference_channel,
            },
            "config": config.to_dict(),
            "config_hash": config.signature(),
            "strategy": strategy_decision,
            "marker_detection": {
                "marker_description": {
                    "sample_rate": spec.sample_rate,
                    "pulse_width_samples": spec.pulse_width_samples,
                    "search_radius_samples": spec.search_radius_samples,
                    "min_confidence": spec.min_confidence,
                    "marker_count": spec.count,
                    "signature": marker_signature,
                },
                "channels": [
                    marker_results[channel].to_dict() for channel in channels
                ],
            },
            "delay_estimation": {
                "reference_channel": config.reference_channel,
                "method": method,
                "segment_count": len(bounds),
                "channels": [
                    {
                        "channel": channel,
                        "segments": [item.to_dict() for item in delay_results[channel]],
                    }
                    for channel in channels
                ],
                "change_summary": change_summary,
            },
            "drift": {
                "channels": [drift_models[channel].to_dict() for channel in channels],
            },
            "compensation": {
                "mode": config.interpolation,
                "sinc_half_taps": config.sinc_half_taps if config.interpolation == "windowed_sinc" else None,
                "tolerance_samples": config.tolerance_samples,
                "total_samples": reader.frames,
                "channel_count": len(channels),
                "sample_aligned": True,
                "slice_order_stable": True,
                "tolerance_ok": total_tolerance_ok,
                "channels": [result.to_dict() for result in channel_results],
                "interpolation_comparison": interpolation_comparison,
            },
            "cache": {
                "root": str(cache.root),
                "stats": stats.to_dict(),
                "per_channel": {
                    str(channel): values
                    for channel, values in channel_cache_stats.items()
                },
            },
            "outputs": {
                "aligned_wav": str(aligned_path),
                "residual_heatmap_npz": str(heatmap_path),
                "run_summary_json": str(run_dir / "summary.json"),
            },
            "errors": errors,
            "diagnostics": {
                "overall_status": "ok"
                if not errors and total_tolerance_ok
                else "attention",
                "residual_tolerance_ok": total_tolerance_ok,
                "measured_worst_residual_samples": measured_worst,
            },
        }
        total_elapsed = time.perf_counter() - started
        report["strategy"]["elapsed_seconds"] = total_elapsed
        write_json_atomic(run_dir / "summary.json", report)
        progress("complete", 1.0, "synchronization complete")
        return report
