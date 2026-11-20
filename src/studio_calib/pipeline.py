from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

import numpy as np

from .align import align_channel
from .analysis import analyse_channel
from .bands import Band, make_bands
from .cache import CacheEntry, ContentAddressedCache
from .config import CalibrationConfig
from .consistency import diagnose_consistency
from .errors import CalibrationError
from .filters import (
    choose_nfft,
    correction_segments,
    design_linear_phase_fir,
)
from .hashing import file_sha256, short_hash, stable_hash
from .propagation import build_error_band
from .smoothing import smooth_band, smoothing_weights, weights_signature
from .wavio import Measurement, layout_channel_names, read_pcm16_wav


@dataclass
class TraceEntry:
    node: str
    channel: str
    band: str
    estimate: Any
    error_upper: Any
    cache_hit: bool
    content_hash: str
    detail: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "node": self.node,
            "channel": self.channel,
            "band": self.band,
            "estimate": self.estimate,
            "error_upper": self.error_upper,
            "cache_hit": bool(self.cache_hit),
            "content_hash": self.content_hash,
            "detail": self.detail,
        }


class TraceRecorder:
    def __init__(self) -> None:
        self.entries: list[TraceEntry] = []

    def add(
        self,
        node: str,
        channel: str,
        band: str,
        estimate: Any,
        error_upper: Any,
        hit: bool,
        content_hash: str,
        **detail: Any,
    ) -> None:
        self.entries.append(
            TraceEntry(
                node=node,
                channel=channel,
                band=band,
                estimate=estimate,
                error_upper=error_upper,
                cache_hit=hit,
                content_hash=content_hash,
                detail=detail,
            )
        )

    def jsonl(self) -> str:
        lines = [
            json.dumps(entry.to_dict(), sort_keys=True, separators=(",", ":"), ensure_ascii=True)
            for entry in self.entries
        ]
        return "\n".join(lines) + ("\n" if lines else "")


@dataclass
class ProcessResult:
    version: int
    content_hash: str
    measurement: dict[str, Any]
    config: CalibrationConfig
    bands: list[dict[str, Any]]
    alignments: dict[str, dict[str, Any]]
    analysis: dict[str, dict[str, Any]]
    smoothing: dict[str, dict[str, Any]]
    filters: dict[str, dict[str, Any]]
    errors: dict[str, list[dict[str, Any]]]
    consistency: dict[str, Any]
    trace: list[dict[str, Any]]
    affected_nodes: list[str]
    cache_summary: dict[str, int]
    diff: dict[str, Any]

    @property
    def channel_names(self) -> list[str]:
        return list(self.measurement["channel_names"])

    def to_dict(self, include_trace: bool = True) -> dict[str, Any]:
        data = {
            "version": self.version,
            "content_hash": self.content_hash,
            "measurement": self.measurement,
            "config": self.config.to_dict(),
            "bands": self.bands,
            "alignments": self.alignments,
            "analysis": self.analysis,
            "smoothing": self.smoothing,
            "filters": self.filters,
            "errors": self.errors,
            "consistency": self.consistency,
            "affected_nodes": self.affected_nodes,
            "cache_summary": self.cache_summary,
            "diff": self.diff,
        }
        if include_trace:
            data["trace"] = self.trace
        return data


def _as_array(value: Any, dtype: Any = np.float64) -> np.ndarray:
    return np.asarray(value, dtype=dtype)


def _combine_smoothing(
    bands: tuple[Band, ...], entries: list[dict[str, Any]]
) -> dict[str, Any]:
    count = len(bands)
    keys = [
        "smoothed_power",
        "smoothed_db",
        "propagated_quantization_bound",
        "propagated_segmentation_bound",
        "smoothing_coupling_bound",
        "max_neighbor_weight",
    ]
    result: dict[str, Any] = {}
    for key in keys:
        result[key] = np.asarray([entry[key] for entry in entries], dtype=np.float64)
    result["weights"] = [
        weights_signature(
            _as_array(entry["weight_indices"], np.int64),
            _as_array(entry["weights"]),
        )
        for entry in entries
    ]
    result["coupling_sources"] = [entry["coupling_sources"] for entry in entries]
    return result


def _publication_payload(
    measurement: dict[str, Any],
    config: CalibrationConfig,
    bands: list[dict[str, Any]],
    alignments: dict[str, dict[str, Any]],
    analysis: dict[str, dict[str, Any]],
    smoothing: dict[str, dict[str, Any]],
    filters: dict[str, dict[str, Any]],
    errors: dict[str, list[dict[str, Any]]],
    consistency: dict[str, Any],
) -> dict[str, Any]:
    return {
        "measurement": measurement,
        "config": config.to_dict(),
        "bands": bands,
        "alignments": alignments,
        "analysis": analysis,
        "smoothing": smoothing,
        "filters": filters,
        "errors": errors,
        "consistency": consistency,
    }


def _filter_content_for_publication(filters: dict[str, dict[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for channel, design in filters.items():
        result[channel] = {
            "coefficients": design["coefficients"],
            "group_delay_samples": design["group_delay_samples"],
            "group_delay_seconds": design["group_delay_seconds"],
            "band_realized_db": design["band_realized_db"],
            "truncation_db_bound": design["truncation_db_bound"],
            "response_frequencies_hz": design["response_frequencies_hz"],
            "response_db": design["response_db"],
            "nfft": design["nfft"],
            "segment_hashes": design["segment_hashes"],
        }
    return result


def _analysis_content_for_publication(analysis: dict[str, dict[str, Any]]) -> dict[str, Any]:
    excluded = {"aligned"}
    return {
        channel: {key: value for key, value in payload.items() if key not in excluded}
        for channel, payload in analysis.items()
    }


def _build_diff(previous: ProcessResult | None, current: ProcessResult) -> dict[str, Any]:
    if previous is None:
        return {
            "base_version": None,
            "content_hash_changed": True,
            "config_changes": {},
            "alignment_offsets": [],
            "band_energy_db": [],
            "filter_coefficients": [],
            "error_status": [],
            "warnings_added": [],
            "warnings_removed": [],
        }
    old_config = previous.config.to_dict()
    new_config = current.config.to_dict()
    config_changes: dict[str, Any] = {}
    for key in sorted(set(old_config) | set(new_config)):
        if old_config.get(key) != new_config.get(key):
            config_changes[key] = {"old": old_config.get(key), "new": new_config.get(key)}

    alignment_offsets: list[dict[str, Any]] = []
    same_measurement = (
        previous.measurement["file_sha256"] == current.measurement["file_sha256"]
    )
    if same_measurement:
        for channel in current.channel_names:
            old_offset = previous.alignments[channel]["offset_samples"]
            new_offset = current.alignments[channel]["offset_samples"]
            if old_offset != new_offset:
                alignment_offsets.append(
                    {
                        "channel": channel,
                        "old": int(old_offset),
                        "new": int(new_offset),
                        "delta": int(new_offset - old_offset),
                    }
                )

    band_energy_db: list[dict[str, Any]] = []
    for channel in current.channel_names:
        old_values = _as_array(previous.analysis[channel]["power_db"])
        new_values = _as_array(current.analysis[channel]["power_db"])
        for index, band in enumerate(current.bands):
            if index < old_values.size:
                old_value = float(old_values[index])
                delta = float(new_values[index] - old_value)
                if abs(delta) > 1e-9:
                    band_energy_db.append(
                        {
                            "channel": channel,
                            "band": band["label"],
                            "old": old_value,
                            "new": float(new_values[index]),
                            "delta_db": delta,
                        }
                    )
            else:
                band_energy_db.append(
                    {
                        "channel": channel,
                        "band": band["label"],
                        "old": None,
                        "new": float(new_values[index]),
                        "delta_db": None,
                    }
                )

    filter_coefficients: list[dict[str, Any]] = []
    for channel in current.channel_names:
        old_coeff = _as_array(previous.filters[channel]["coefficients"])
        new_coeff = _as_array(current.filters[channel]["coefficients"])
        if old_coeff.shape != new_coeff.shape:
            maximum = float("inf")
            changed = True
        else:
            maximum = float(np.max(np.abs(new_coeff - old_coeff)))
            changed = maximum > 0.0
        if changed:
            filter_coefficients.append(
                {
                    "channel": channel,
                    "max_abs_delta": maximum,
                    "changed": changed,
                }
            )

    error_status: list[dict[str, Any]] = []
    for channel in current.channel_names:
        old_errors = previous.errors[channel]
        for index, band in enumerate(current.bands):
            new_error = current.errors[channel][index]
            if index >= len(old_errors):
                error_status.append(
                    {
                        "channel": channel,
                        "band": band["label"],
                        "old": None,
                        "new": new_error["status"],
                        "old_total_db": None,
                        "new_total_db": new_error["total_error_db"],
                    }
                )
                continue
            old_error = old_errors[index]
            if old_error["status"] != new_error["status"]:
                error_status.append(
                    {
                        "channel": channel,
                        "band": band["label"],
                        "old": old_error["status"],
                        "new": new_error["status"],
                        "old_total_db": old_error["total_error_db"],
                        "new_total_db": new_error["total_error_db"],
                    }
                )

    def warning_key(item: dict[str, Any]) -> str:
        return stable_hash(item)

    old_warnings = {warning_key(item): item for item in previous.consistency["warnings"]}
    new_warnings = {warning_key(item): item for item in current.consistency["warnings"]}
    warnings_added = [new_warnings[key] for key in sorted(new_warnings.keys() - old_warnings.keys())]
    warnings_removed = [
        old_warnings[key] for key in sorted(old_warnings.keys() - new_warnings.keys())
    ]

    return {
        "base_version": previous.version,
        "content_hash_changed": previous.content_hash != current.content_hash,
        "config_changes": config_changes,
        "alignment_offsets": alignment_offsets,
        "band_energy_db": band_energy_db,
        "filter_coefficients": filter_coefficients,
        "error_status": error_status,
        "warnings_added": warnings_added,
        "warnings_removed": warnings_removed,
    }


class CalibrationPipeline:
    def __init__(
        self,
        cache_dir: str | Path = "cache",
        cache_enabled: bool = True,
    ) -> None:
        self.cache = ContentAddressedCache(cache_dir, enabled=cache_enabled)

    def run(
        self,
        wav_path: str | Path,
        config: CalibrationConfig,
        previous_result: ProcessResult | None = None,
        cancel_event: Any | None = None,
        progress_callback: Callable[[str, float], None] | None = None,
    ) -> ProcessResult:
        path = Path(wav_path)
        trace = TraceRecorder()
        affected: set[str] = set()
        hit_count = 0
        miss_count = 0

        def check_cancel() -> None:
            if cancel_event is not None and cancel_event.is_set():
                raise CalibrationError("E_CANCELLED", "processing cancelled")

        def progress(stage: str, fraction: float) -> None:
            if progress_callback is not None:
                progress_callback(stage, fraction)

        def track(entry: CacheEntry, node: str) -> None:
            nonlocal hit_count, miss_count
            if entry.hit:
                hit_count += 1
            else:
                miss_count += 1
                affected.add(node)

        progress("load", 0.01)
        check_cancel()
        file_hash = file_sha256(str(path))
        load_key = self.cache.key("load", {"file_sha256": file_hash})
        load_entry = self.cache.get(load_key)

        def load_measurement_payload() -> dict[str, Any]:
            measurement = read_pcm16_wav(str(path))
            return {
                "sample_rate": measurement.sample_rate,
                "layout": measurement.layout,
                "quantization_step": measurement.quantization_step,
                "channels": measurement.channels,
            }

        if load_entry is None:
            load_payload = load_measurement_payload()
            load_entry = self.cache.put(load_key, load_payload)
        track(load_entry, "load")
        payload = load_entry.payload
        canonical_names = layout_channel_names(str(payload["layout"]))
        if set(canonical_names) != set(payload["channels"]):
            raise CalibrationError(
                "E_WAV_INVALID", "cached channel layout does not match channel data"
            )
        measurement = Measurement(
            sample_rate=int(payload["sample_rate"]),
            layout=str(payload["layout"]),
            quantization_step=float(payload["quantization_step"]),
            channels={
                name: _as_array(payload["channels"][name])
                for name in canonical_names
            },
        )
        channel_names = list(measurement.channels)
        trace.add(
            "load",
            "*",
            "*",
            measurement.layout,
            0.0,
            load_entry.hit,
            load_entry.content_hash,
            sample_rate=measurement.sample_rate,
            channel_names=channel_names,
            quantization_step=measurement.quantization_step,
        )

        config.validate(measurement.sample_rate, channel_names)
        bands = make_bands(config.band_fraction, config.f_min_hz, config.f_max_hz)
        if not bands:
            raise CalibrationError("E_CONFIG_INVALID", "no analysis bands", "band_fraction")
        band_dicts = [band.to_dict() for band in bands]
        centers = np.asarray([band.center_hz for band in bands], dtype=np.float64)

        raw_hashes = {
            channel: stable_hash(measurement.channels[channel]) for channel in channel_names
        }
        ref_channel = config.reference_channel
        alignments: dict[str, dict[str, Any]] = {}
        align_hashes: dict[str, str] = {}
        for channel_index, channel in enumerate(channel_names):
            check_cancel()
            progress("align", 0.05 + 0.10 * channel_index / len(channel_names))
            identity = {
                "file_sha256": file_hash,
                "channel_hash": raw_hashes[channel],
                "reference_channel": ref_channel,
                "reference_channel_hash": raw_hashes[ref_channel],
                "threshold_sigma": config.threshold_sigma,
                "threshold_peak_ratio": config.threshold_peak_ratio,
                "sample_rate": measurement.sample_rate,
            }
            node = f"align:{channel}"
            entry = self.cache.get_or_compute(
                "align",
                identity,
                lambda channel=channel: align_channel(
                    measurement.channels[channel],
                    measurement.channels[ref_channel],
                    config.threshold_sigma,
                    config.threshold_peak_ratio,
                ),
            )
            track(entry, node)
            align_payload = dict(entry.payload)
            old_offset: int | None = None
            if (
                previous_result is not None
                and previous_result.measurement["file_sha256"] == file_hash
                and channel in previous_result.alignments
            ):
                old_offset = int(previous_result.alignments[channel]["offset_samples"])
            new_offset = int(align_payload["offset_samples"])
            offset_delta = None if old_offset is None else new_offset - old_offset
            align_payload["offset_delta_samples"] = offset_delta
            alignments[channel] = align_payload
            align_hashes[channel] = entry.content_hash
            trace.add(
                "align",
                channel,
                "*",
                new_offset,
                float(align_payload["alignment_uncertainty_samples"]),
                entry.hit,
                entry.content_hash,
                arrival_sample=int(align_payload["arrival_sample"]),
                reference_channel=ref_channel,
                reference_arrival_sample=int(
                    align_payload["reference_arrival_sample"]
                ),
                threshold=float(align_payload["threshold"]),
                noise_sigma=float(align_payload["noise_sigma"]),
                normalization_gain=float(align_payload["normalization_gain"]),
                old_offset_samples=old_offset,
                offset_delta_samples=offset_delta,
            )

        analysis: dict[str, dict[str, Any]] = {}
        analysis_hashes: dict[str, str] = {}
        for channel_index, channel in enumerate(channel_names):
            check_cancel()
            progress("bands", 0.15 + 0.18 * channel_index / len(channel_names))
            aligned = _as_array(alignments[channel]["aligned"])
            identity = {
                "aligned_hash": stable_hash(aligned),
                "sample_rate": measurement.sample_rate,
                "quantization_step": measurement.quantization_step,
                "frame_size": config.frame_size,
                "overlap": config.overlap,
                "window": config.window,
                "band_fraction": config.band_fraction,
                "f_min_hz": config.f_min_hz,
                "f_max_hz": config.f_max_hz,
                "segmentation_safety": config.segmentation_safety,
            }
            node = f"bands:{channel}"
            entry = self.cache.get_or_compute(
                "bands",
                identity,
                lambda channel=channel, aligned=aligned: analyse_channel(
                    aligned,
                    measurement.sample_rate,
                    measurement.quantization_step,
                    bands,
                    config.frame_size,
                    config.overlap,
                    config.window,
                    config.segmentation_safety,
                ),
            )
            track(entry, node)
            analysis[channel] = dict(entry.payload)
            analysis_hashes[channel] = entry.content_hash
            for band_index, band in enumerate(bands):
                q = float(
                    analysis[channel]["quantization_power_bound"][band_index]
                )
                seg = float(analysis[channel]["segmentation_power_bound"][band_index])
                trace.add(
                    "bands",
                    channel,
                    band.label,
                    float(analysis[channel]["power_db"][band_index]),
                    10.0 * math.log10(
                        1.0
                        + (q + seg)
                        / max(
                            float(analysis[channel]["power"][band_index]),
                            np.finfo(np.float64).tiny,
                        )
                    ),
                    entry.hit,
                    entry.content_hash,
                    phase_deg=float(analysis[channel]["phase_deg"][band_index]),
                    quantization_power_bound=q,
                    segmentation_power_bound=seg,
                    segment_variance=float(
                        analysis[channel]["segment_variance"][band_index]
                    ),
                    segment_count=int(analysis[channel]["segment_count"]),
                    coherent_gain=float(analysis[channel]["coherent_gain"]),
                )

        smoothing: dict[str, dict[str, Any]] = {}
        smoothing_band_hashes: dict[str, list[str]] = {}
        for channel_index, channel in enumerate(channel_names):
            check_cancel()
            progress("smoothing", 0.33 + 0.14 * channel_index / len(channel_names))
            entries: list[dict[str, Any]] = []
            hashes: list[str] = []
            for band_index, band in enumerate(bands):
                indices, weights = smoothing_weights(
                    bands, band_index, config.smoothing_octaves
                )
                upstream = [
                    {
                        "index": int(index),
                        "power": float(analysis[channel]["power"][index]),
                        "phase_rad": float(analysis[channel]["phase_rad"][index]),
                        "quantization_power_bound": float(
                            analysis[channel]["quantization_power_bound"][index]
                        ),
                        "segmentation_power_bound": float(
                            analysis[channel]["segmentation_power_bound"][index]
                        ),
                    }
                    for index in indices
                ]
                identity = {
                    "analysis_hash": analysis_hashes[channel],
                    "smoothing_octaves": config.smoothing_octaves,
                    "weights": weights_signature(indices, weights),
                    "upstream": upstream,
                }
                entry = self.cache.get_or_compute(
                    "smooth",
                    identity,
                    lambda band_index=band_index: smooth_band(
                        band_index,
                        bands,
                        analysis[channel],
                        config.smoothing_octaves,
                        config.segmentation_safety,
                    ),
                )
                node = f"smooth:{channel}:{band.label}"
                track(entry, node)
                entries.append(dict(entry.payload))
                hashes.append(entry.content_hash)
                trace.add(
                    "smooth",
                    channel,
                    band.label,
                    float(entry.payload["smoothed_db"]),
                    10.0
                    * math.log10(
                        1.0
                        + float(entry.payload["smoothing_coupling_bound"])
                        / max(
                            float(entry.payload["smoothed_power"]),
                            np.finfo(np.float64).tiny,
                        )
                    ),
                    entry.hit,
                    entry.content_hash,
                    coupling_bound=float(entry.payload["smoothing_coupling_bound"]),
                    max_neighbor_weight=float(entry.payload["max_neighbor_weight"]),
                    contributing_bands=[
                        bands[int(index)].label
                        for index in entry.payload["weight_indices"]
                    ],
                    weights=[float(value) for value in entry.payload["weights"]],
                )
            smoothing[channel] = _combine_smoothing(bands, entries)
            smoothing_band_hashes[channel] = hashes

        target_levels: dict[str, float] = {}
        for channel in channel_names:
            db_values = _as_array(smoothing[channel]["smoothed_db"])
            level_mask = (centers >= 100.0) & (centers <= 10000.0)
            if not np.any(level_mask):
                level_mask = np.ones_like(centers, dtype=bool)
            target_levels[channel] = float(np.median(db_values[level_mask]))
            smoothing[channel]["reference_level_db"] = target_levels[channel]

        filters: dict[str, dict[str, Any]] = {}
        filters_for_consistency: dict[str, dict[str, Any]] = {}
        correction_hashes: dict[str, list[str]] = {}
        for channel_index, channel in enumerate(channel_names):
            check_cancel()
            progress("filter", 0.47 + 0.20 * channel_index / len(channel_names))
            smoothed_db = _as_array(smoothing[channel]["smoothed_db"])
            target_db = config.target_gain_db(centers)
            measured_relative = smoothed_db - target_levels[channel]
            raw_correction = target_db - measured_relative
            corrections = np.clip(
                raw_correction, config.max_cut_db, config.max_boost_db
            )
            correction_entries: list[dict[str, Any]] = []
            correction_hashes_channel: list[str] = []
            for band_index, band in enumerate(bands):
                identity = {
                    "smoothed_hash": smoothing_band_hashes[channel][band_index],
                    "target_db": float(target_db[band_index]),
                    "measured_relative_db": float(measured_relative[band_index]),
                    "max_boost_db": config.max_boost_db,
                    "max_cut_db": config.max_cut_db,
                }
                correction_payload = {
                    "target_db": float(target_db[band_index]),
                    "measured_relative_db": float(measured_relative[band_index]),
                    "raw_correction_db": float(raw_correction[band_index]),
                    "correction_db": float(corrections[band_index]),
                    "clamped": bool(
                        abs(raw_correction[band_index] - corrections[band_index]) > 0
                    ),
                }
                entry = self.cache.get_or_compute(
                    "correction", identity, lambda payload=correction_payload: payload
                )
                node = f"correction:{channel}:{band.label}"
                track(entry, node)
                correction_entries.append(dict(entry.payload))
                correction_hashes_channel.append(entry.content_hash)
                trace.add(
                    "correction",
                    channel,
                    band.label,
                    float(entry.payload["correction_db"]),
                    0.0,
                    entry.hit,
                    entry.content_hash,
                    target_db=float(entry.payload["target_db"]),
                    measured_relative_db=float(
                        entry.payload["measured_relative_db"]
                    ),
                    clamped=bool(entry.payload["clamped"]),
                )
            smoothing[channel]["target_db"] = np.asarray(
                [entry["target_db"] for entry in correction_entries], dtype=np.float64
            )
            smoothing[channel]["correction_db"] = np.asarray(
                [entry["correction_db"] for entry in correction_entries], dtype=np.float64
            )
            smoothing[channel]["correction_hashes"] = correction_hashes_channel
            correction_hashes[channel] = correction_hashes_channel

            segments = correction_segments(centers, corrections)
            segment_hashes: list[str] = []
            for segment_index, segment in enumerate(segments):
                entry = self.cache.get_or_compute(
                    "filter_segment",
                    {
                        "centers_hz": centers.tolist(),
                        "segment_index": segment_index,
                        "segment": segment,
                    },
                    lambda segment=segment: segment,
                )
                node = f"filter_segment:{channel}:{segment_index}"
                track(entry, node)
                segment_hashes.append(entry.content_hash)
                estimate = 0.5 * (
                    float(segment["lower_correction_db"])
                    + float(segment["upper_correction_db"])
                )
                trace.add(
                    "filter_segment",
                    channel,
                    f"segment-{segment_index}",
                    estimate,
                    0.0,
                    entry.hit,
                    entry.content_hash,
                    kind=segment["kind"],
                    lower_center_hz=float(segment["lower_center_hz"]),
                    upper_center_hz=float(segment["upper_center_hz"]),
                )

            nfft = choose_nfft(config.filter_taps)
            filter_identity = {
                "segment_hashes": segment_hashes,
                "taps": config.filter_taps,
                "sample_rate": measurement.sample_rate,
                "nfft": nfft,
            }
            filter_node = f"filter:{channel}"
            filter_entry = self.cache.get_or_compute(
                "filter",
                filter_identity,
                lambda: design_linear_phase_fir(
                    centers,
                    corrections,
                    config.filter_taps,
                    measurement.sample_rate,
                    nfft,
                ),
            )
            track(filter_entry, filter_node)
            filter_payload = dict(filter_entry.payload)
            filter_payload["segment_hashes"] = segment_hashes
            filters[channel] = filter_payload
            filters_for_consistency[channel] = filter_payload
            trace.add(
                "filter",
                channel,
                "*",
                float(filter_payload["group_delay_samples"]),
                float(np.max(_as_array(filter_payload["truncation_db_bound"]))),
                filter_entry.hit,
                filter_entry.content_hash,
                taps=config.filter_taps,
                group_delay_seconds=float(filter_payload["group_delay_seconds"]),
                nfft=int(filter_payload["nfft"]),
                segment_hashes=segment_hashes,
            )

        errors: dict[str, list[dict[str, Any]]] = {}
        error_hashes: dict[str, list[str]] = {}
        for channel_index, channel in enumerate(channel_names):
            check_cancel()
            progress("error_budget", 0.67 + 0.16 * channel_index / len(channel_names))
            channel_errors: list[dict[str, Any]] = []
            hashes: list[str] = []
            error_values = _as_array(
                filters[channel]["truncation_db_bound"], dtype=np.float64
            )
            for band_index, band in enumerate(bands):
                intermediates = {
                    "offset_samples": int(alignments[channel]["offset_samples"]),
                    "alignment_uncertainty_samples": float(
                        alignments[channel]["alignment_uncertainty_samples"]
                    ),
                    "quantization_power_bound": float(
                        smoothing[channel]["propagated_quantization_bound"][band_index]
                    ),
                    "segmentation_power_bound": float(
                        smoothing[channel]["propagated_segmentation_bound"][band_index]
                    ),
                    "smoothing_coupling_bound": float(
                        smoothing[channel]["smoothing_coupling_bound"][band_index]
                    ),
                    "max_neighbor_weight": float(
                        smoothing[channel]["max_neighbor_weight"][band_index]
                    ),
                    "smoothing_weights": smoothing[channel]["weights"][band_index],
                    "truncation_db": float(error_values[band_index]),
                    "smoothed_power": float(
                        smoothing[channel]["smoothed_power"][band_index]
                    ),
                    "segment_count": int(analysis[channel]["segment_count"]),
                    "coherent_gain": float(analysis[channel]["coherent_gain"]),
                }
                identity = {
                    "channel": channel,
                    "band": band.label,
                    "sample_rate": measurement.sample_rate,
                    "error_threshold_db": config.error_threshold_db,
                    "truncation_db": float(error_values[band_index]),
                    "alignment_uncertainty_samples": float(
                        alignments[channel]["alignment_uncertainty_samples"]
                    ),
                    "smoothed_power": float(
                        smoothing[channel]["smoothed_power"][band_index]
                    ),
                    "quantization_power_bound": float(
                        smoothing[channel]["propagated_quantization_bound"][band_index]
                    ),
                    "segmentation_power_bound": float(
                        smoothing[channel]["propagated_segmentation_bound"][band_index]
                    ),
                    "smoothing_coupling_bound": float(
                        smoothing[channel]["smoothing_coupling_bound"][band_index]
                    ),
                }
                node = f"error:{channel}:{band.label}"
                entry = self.cache.get_or_compute(
                    "error",
                    identity,
                    lambda band_index=band_index, intermediates=intermediates: build_error_band(
                        channel,
                        bands[band_index].label,
                        bands[band_index].center_hz,
                        measurement.sample_rate,
                        float(alignments[channel]["alignment_uncertainty_samples"]),
                        float(smoothing[channel]["smoothed_power"][band_index]),
                        float(
                            smoothing[channel]["propagated_quantization_bound"][band_index]
                        ),
                        float(
                            smoothing[channel]["propagated_segmentation_bound"][band_index]
                        ),
                        float(
                            smoothing[channel]["smoothing_coupling_bound"][band_index]
                        ),
                        float(error_values[band_index]),
                        config.error_threshold_db,
                        intermediates,
                    ),
                )
                track(entry, node)
                channel_errors.append(dict(entry.payload))
                hashes.append(entry.content_hash)
                trace.add(
                    "error",
                    channel,
                    band.label,
                    float(entry.payload["total_error_db"]),
                    float(entry.payload["total_error_db"]),
                    entry.hit,
                    entry.content_hash,
                    status=entry.payload["status"],
                    dominant_node=entry.payload["dominant_node"],
                    terms_db=entry.payload["terms_db"],
                    terms_power_fraction=entry.payload["terms_power_fraction"],
                    threshold_db=config.error_threshold_db,
                )
            errors[channel] = channel_errors
            error_hashes[channel] = hashes

        check_cancel()
        progress("consistency", 0.84)
        consistency_identity = {
            "analysis_hashes": analysis_hashes,
            "align_offsets": {
                channel: int(alignments[channel]["offset_samples"])
                for channel in channel_names
            },
            "error_hashes": error_hashes,
            "filter_hashes": {
                channel: stable_hash(filters_for_consistency[channel])
                for channel in channel_names
            },
            "config": {
                "reference_channel": ref_channel,
                "consistency_gain_threshold_db": config.consistency_gain_threshold_db,
                "consistency_phase_threshold_deg": config.consistency_phase_threshold_deg,
                "gain_jump_threshold_db": config.gain_jump_threshold_db,
                "low_frequency_limit_hz": config.low_frequency_limit_hz,
            },
        }
        consistency_entry = self.cache.get_or_compute(
            "consistency",
            consistency_identity,
            lambda: diagnose_consistency(
                channel_names,
                ref_channel,
                bands,
                analysis,
                alignments,
                errors,
                filters_for_consistency,
                config.consistency_gain_threshold_db,
                config.consistency_phase_threshold_deg,
                config.gain_jump_threshold_db,
                config.low_frequency_limit_hz,
            ),
        )
        track(consistency_entry, "consistency")
        consistency = dict(consistency_entry.payload)
        trace.add(
            "consistency",
            "*",
            "*",
            int(consistency["warning_count"]),
            0.0,
            consistency_entry.hit,
            consistency_entry.content_hash,
            warning_count=int(consistency["warning_count"]),
            reference_channel=ref_channel,
        )

        progress("publish", 0.96)
        check_cancel()
        for alignment in alignments.values():
            alignment.pop("aligned", None)
        publish_analysis = _analysis_content_for_publication(analysis)
        publish_filters = _filter_content_for_publication(filters)
        publish_alignments = {
            channel: {
                key: value
                for key, value in alignment.items()
                if key not in {"aligned", "offset_delta_samples"}
            }
            for channel, alignment in alignments.items()
        }
        measurement_metadata = {
            "path": str(path),
            "file_sha256": file_hash,
            "sample_rate": measurement.sample_rate,
            "layout": measurement.layout,
            "channel_names": channel_names,
            "quantization_step": measurement.quantization_step,
        }
        hashed_measurement = {
            key: value
            for key, value in measurement_metadata.items()
            if key != "path"
        }
        publication = _publication_payload(
            hashed_measurement,
            config,
            band_dicts,
            publish_alignments,
            publish_analysis,
            smoothing,
            publish_filters,
            errors,
            consistency,
        )
        content_hash = stable_hash(publication)
        version = 1 if previous_result is None else previous_result.version + 1
        trace_dicts = [entry.to_dict() for entry in trace.entries]
        result = ProcessResult(
            version=version,
            content_hash=content_hash,
            measurement=measurement_metadata,
            config=config,
            bands=band_dicts,
            alignments=alignments,
            analysis=analysis,
            smoothing=smoothing,
            filters=filters,
            errors=errors,
            consistency=consistency,
            trace=trace_dicts,
            affected_nodes=sorted(affected),
            cache_summary={
                "hits": hit_count,
                "misses": miss_count,
                "node_count": hit_count + miss_count,
            },
            diff={},
        )
        result.diff = _build_diff(previous_result, result)
        progress("complete", 1.0)
        return result

