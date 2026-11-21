"""Versioned report parsing, upgrade, downgrade and stable serialization."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .atomic import atomic_write_json
from .diagnostics import diagnose_all_channels
from .errors import (
    E_VERSION_INVALID,
    E_VERSION_LOSS,
    Issue,
    SyncError,
)
from .fitting import fit_all_channels
from .models import (
    ChannelLayout,
    ChannelResidual,
    ChannelSpec,
    CompensationParams,
    DiagnosisConfig,
    DriftDiagnosis,
    DriftFit,
    FitConfig,
    Marker,
    ResidualSummary,
    SegmentDelay,
    SegmentationConfig,
    SyncReport,
)
from .utils import optional_float, sha256_json, short_hash


SUPPORTED_VERSIONS = ("v1", "v2", "v3")


@dataclass
class DowngradeResult:
    document: dict[str, Any]
    losses: tuple[Issue, ...]

    @property
    def lost_fields(self) -> tuple[str, ...]:
        output: list[str] = []
        for issue in self.losses:
            output.extend(issue.fields)
        return tuple(output)


def normalize_version(value: Any) -> str:
    if value is None:
        return "v1"
    text = str(value).strip().lower()
    if text in {"1", "v1", "1.0", "v1.0"}:
        return "v1"
    if text in {"2", "v2", "2.0", "v2.0"}:
        return "v2"
    if text in {"3", "v3", "3.0", "v3.0"}:
        return "v3"
    raise SyncError(
        E_VERSION_INVALID,
        f"unsupported report version: {value}",
        fields=["schema_version"],
        details={"supported": list(SUPPORTED_VERSIONS)},
    )


def _first_present(data: dict[str, Any], keys: Iterable[str]) -> Any:
    for key in keys:
        if key in data:
            return data[key]
    return None


def _number(value: Any, default: float = 0.0) -> float:
    result = optional_float(value)
    return default if result is None else result


def _channel_from_key(value: Any, fallback: int | None = None) -> int:
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, str):
        match = re.search(r"-?\d+", value)
        if match:
            return int(match.group(0))
    if fallback is not None:
        return int(fallback)
    raise SyncError(
        E_VERSION_INVALID, f"cannot determine marker channel from {value!r}"
    )


def _looks_like_marker(value: Any) -> bool:
    return isinstance(value, dict) and any(
        key in value
        for key in (
            "arrival_sample",
            "arrival",
            "sample",
            "sample_index",
            "time_s",
            "time",
            "arrival_seconds",
        )
    )


def _flatten_grouped(
    value: Any,
    *,
    channel_key: str = "channel",
) -> list[dict[str, Any]]:
    if value is None:
        return []
    if isinstance(value, list):
        return [dict(item) for item in value if isinstance(item, dict)]
    if isinstance(value, dict):
        if _looks_like_marker(value):
            return [dict(value)]
        output: list[dict[str, Any]] = []
        for key, items in value.items():
            if isinstance(items, list):
                for item in items:
                    if isinstance(item, dict):
                        normalized = dict(item)
                        normalized.setdefault(
                            channel_key, _channel_from_key(key)
                        )
                        output.append(normalized)
            elif isinstance(items, dict) and _looks_like_marker(items):
                normalized = dict(items)
                normalized.setdefault(channel_key, _channel_from_key(key))
                output.append(normalized)
        return output
    return []


def _parse_markers(
    data: dict[str, Any],
    sample_rate: int,
) -> tuple[Marker, ...]:
    raw = _first_present(
        data,
        (
            "markers",
            "sync_markers",
            "marker_arrivals",
            "sync_marker_description",
            "channel_markers",
            "arrivals",
        ),
    )
    flattened = _flatten_grouped(raw)
    output: list[Marker] = []
    counters: dict[int, int] = {}
    for item in flattened:
        channel = _channel_from_key(
            _first_present(item, ("channel", "channel_id", "ch", "input_channel")),
            0,
        )
        if "marker_index" in item or "index" in item or "sequence" in item:
            marker_index = int(
                _first_present(item, ("marker_index", "index", "sequence"))
            )
        else:
            marker_index = counters.get(channel, 0)
        counters[channel] = max(counters.get(channel, 0), marker_index + 1)
        arrival = _first_present(
            item,
            (
                "arrival_sample",
                "arrival_samples",
                "sample",
                "sample_index",
                "arrival",
                "timestamp_samples",
            ),
        )
        if arrival is None:
            seconds = _first_present(
                item,
                ("time_s", "time", "arrival_seconds", "timestamp_seconds"),
            )
            if seconds is None:
                raise SyncError(
                    E_VERSION_INVALID,
                    "marker entry has no arrival sample or time",
                    channel=channel,
                    marker_index=marker_index,
                )
            arrival = float(seconds) * sample_rate
        confidence = _number(
            _first_present(item, ("confidence", "score")),
            1.0,
        )
        snr_db = _number(_first_present(item, ("snr_db", "snr")), 0.0)
        output.append(
            Marker(
                channel=channel,
                marker_index=marker_index,
                arrival_sample=float(arrival),
                confidence=float(confidence),
                snr_db=float(snr_db),
            )
        )
    return tuple(
        sorted(output, key=lambda marker: (marker.channel, marker.marker_index))
    )


def _parse_segments(
    data: dict[str, Any],
    segmentation: SegmentationConfig,
) -> tuple[SegmentDelay, ...]:
    raw = _first_present(
        data,
        ("segment_delays", "segments", "segment_delay_series", "delay_segments"),
    )
    flattened = _flatten_grouped(raw)
    output: list[SegmentDelay] = []
    counters: dict[int, int] = {}
    length = segmentation.segment_length_samples
    for item in flattened:
        channel = _channel_from_key(
            _first_present(item, ("channel", "channel_id", "ch")), 0
        )
        segment_index = int(
            _first_present(item, ("segment_index", "index", "segment"))
            or counters.get(channel, 0)
        )
        counters[channel] = max(counters.get(channel, 0), segment_index + 1)
        start = _first_present(
            item, ("start_sample", "start", "start_frame", "begin_sample")
        )
        end = _first_present(
            item, ("end_sample", "end", "end_frame", "stop_sample")
        )
        if start is None:
            start = segment_index * length
        if end is None:
            end = int(start) + length
        delay = _first_present(
            item,
            (
                "delay_samples",
                "lag_samples",
                "delay",
                "lag",
                "estimated_delay_samples",
            ),
        )
        if delay is None:
            raise SyncError(
                E_VERSION_INVALID,
                "segment entry has no delay value",
                channel=channel,
                segment_index=segment_index,
            )
        correlation = optional_float(
            _first_present(
                item,
                ("peak_correlation", "correlation", "peak_score", "confidence"),
            )
        )
        ci_low = optional_float(
            _first_present(
                item,
                ("ci_low_samples", "ci_low", "uncertainty_low_samples"),
            )
        )
        ci_high = optional_float(
            _first_present(
                item,
                ("ci_high_samples", "ci_high", "uncertainty_high_samples"),
            )
        )
        if ci_low is None and ci_high is None:
            uncertainty = optional_float(
                _first_present(item, ("uncertainty_samples", "uncertainty"))
            )
            if uncertainty is not None:
                ci_low = float(delay) - uncertainty
                ci_high = float(delay) + uncertainty
        output.append(
            SegmentDelay(
                channel=channel,
                segment_index=segment_index,
                start_sample=int(start),
                end_sample=int(end),
                delay_samples=float(delay),
                peak_correlation=correlation,
                ci_low_samples=ci_low,
                ci_high_samples=ci_high,
                marker_count=int(item.get("marker_count", 0)),
                method=str(item.get("method", "loaded")),
            )
        )
    return tuple(
        sorted(output, key=lambda segment: (segment.channel, segment.segment_index))
    )


def _parse_fits(
    data: dict[str, Any],
    segments: tuple[SegmentDelay, ...],
    fitting: FitConfig,
) -> tuple[DriftFit, ...]:
    raw = _first_present(
        data, ("drift_fits", "drift_models", "fits", "clock_models")
    )
    flattened = _flatten_grouped(raw)
    output: list[DriftFit] = []
    for item in flattened:
        channel = _channel_from_key(
            _first_present(item, ("channel", "channel_id", "ch")), 0
        )
        slope_value = optional_float(
            _first_present(item, ("slope", "clock_slope", "frequency_scale"))
        )
        drift_ppm = optional_float(
            _first_present(
                item,
                ("drift_ppm", "frequency_ppm", "clock_ppm", "ppm"),
            )
        )
        if drift_ppm is None:
            drift_ppm = (slope_value or 0.0) * 1.0e6
        if slope_value is None:
            slope_value = drift_ppm * 1.0e-6
        initial = _number(
            _first_present(
                item,
                (
                    "initial_offset_samples",
                    "offset_samples",
                    "initial_offset",
                    "intercept_samples",
                ),
            ),
            0.0,
        )
        residual_by_segment = tuple(
            float(value)
            for value in item.get("residual_by_segment", item.get("residuals", []))
        )
        threshold = _number(
            _first_present(
                item,
                ("residual_threshold_samples", "threshold_samples"),
            ),
            fitting.residual_threshold_samples,
        )
        residual_rms = _number(
            _first_present(item, ("residual_rms", "fit_residual_rms")), 0.0
        )
        residual_max = _number(
            _first_present(item, ("residual_max", "fit_residual_max")), 0.0
        )
        channel_segments = [
            segment for segment in segments if segment.channel == channel
        ]
        predicted_raw = item.get("predicted_by_segment", [])
        predicted_by_segment: list[dict[str, Any]] = []
        if isinstance(predicted_raw, list) and predicted_raw:
            for entry in predicted_raw:
                if isinstance(entry, dict):
                    predicted_by_segment.append(dict(entry))
        if not predicted_by_segment:
            for segment in sorted(
                channel_segments, key=lambda value: value.segment_index
            ):
                midpoint = 0.5 * (
                    segment.start_sample + segment.end_sample - 1
                )
                prediction = initial + slope_value * midpoint
                predicted_by_segment.append(
                    {
                        "segment_index": segment.segment_index,
                        "midpoint_sample": midpoint,
                        "predicted_delay_samples": prediction,
                        "observed_delay_samples": segment.delay_samples,
                        "residual_samples": segment.delay_samples - prediction,
                    }
                )
        max_segment = item.get("max_residual_segment")
        if max_segment is None and residual_by_segment:
            max_segment = channel_segments[
                int(np.argmax(np.abs(residual_by_segment)))
            ].segment_index
        output.append(
            DriftFit(
                channel=channel,
                initial_offset_samples=initial,
                drift_ppm=float(drift_ppm),
                slope=float(slope_value),
                residual_rms=residual_rms,
                residual_max=residual_max,
                residual_threshold_samples=threshold,
                max_residual_segment=(
                    int(max_segment) if max_segment is not None else None
                ),
                residual_by_segment=residual_by_segment,
                predicted_by_segment=tuple(predicted_by_segment),
                exceeded_threshold=bool(
                    item.get(
                        "exceeded_threshold",
                        residual_max > threshold,
                    )
                ),
                slope_std=_number(item.get("slope_std"), 0.0),
                drift_ppm_ci_low=optional_float(item.get("drift_ppm_ci_low")),
                drift_ppm_ci_high=optional_float(item.get("drift_ppm_ci_high")),
            )
        )
    return tuple(sorted(output, key=lambda fit: fit.channel))


def _parse_diagnoses(data: dict[str, Any]) -> tuple[DriftDiagnosis, ...]:
    raw = _first_present(
        data, ("diagnoses", "diagnostics", "conclusions", "drift_diagnoses")
    )
    flattened = _flatten_grouped(raw)
    output: list[DriftDiagnosis] = []
    for item in flattened:
        channel = _channel_from_key(
            _first_present(item, ("channel", "channel_id", "ch")), 0
        )
        output.append(
            DriftDiagnosis(
                channel=channel,
                conclusion=str(
                    _first_present(
                        item, ("conclusion", "class", "diagnosis", "result")
                    )
                    or "unknown"
                ),
                confidence=_number(
                    _first_present(item, ("confidence", "score")), 0.0
                ),
                evidence_segments=tuple(
                    int(value)
                    for value in item.get(
                        "evidence_segments",
                        item.get("evidence", []),
                    )
                ),
                ci_low_ppm=optional_float(
                    _first_present(item, ("ci_low_ppm", "ci_low"))
                ),
                ci_high_ppm=optional_float(
                    _first_present(item, ("ci_high_ppm", "ci_high"))
                ),
                jump_segments=tuple(
                    int(value) for value in item.get("jump_segments", [])
                ),
                candidates=tuple(
                    str(value) for value in item.get("candidates", [])
                ),
                message=str(item.get("message", "")),
            )
        )
    return tuple(sorted(output, key=lambda diagnosis: diagnosis.channel))


def _parse_layout(
    data: dict[str, Any],
    *,
    markers: tuple[Marker, ...],
    segments: tuple[SegmentDelay, ...],
    fits: tuple[DriftFit, ...],
    sample_rate: int,
    reference_channel: int | None,
) -> ChannelLayout:
    layout_data = _first_present(data, ("channel_layout", "layout"))
    if isinstance(layout_data, dict):
        reference_channel = (
            int(layout_data.get("reference_channel", reference_channel or 0))
            if reference_channel is None
            else reference_channel
        )
        sample_rate = int(layout_data.get("sample_rate", sample_rate))
        raw_channels = layout_data.get("channels", [])
        channel_specs: list[ChannelSpec] = []
        if isinstance(raw_channels, dict):
            raw_channels = [
                {"index": _channel_from_key(key), "name": str(key)}
                for key in raw_channels
            ]
        for fallback, item in enumerate(raw_channels):
            if isinstance(item, dict):
                index = int(
                    _first_present(item, ("index", "channel", "id")) or fallback
                )
                name = str(
                    _first_present(item, ("name", "label"))
                    or f"channel_{index}"
                )
                rate = int(item.get("sample_rate", sample_rate))
            else:
                index = int(item)
                name = f"channel_{index}"
                rate = sample_rate
            channel_specs.append(
                ChannelSpec(index=index, name=name, sample_rate=rate)
            )
    else:
        reference_channel = 0 if reference_channel is None else reference_channel
        channel_specs = []
    observed_channels = {marker.channel for marker in markers}
    observed_channels.update(segment.channel for segment in segments)
    observed_channels.update(fit.channel for fit in fits)
    if channel_specs:
        observed_channels.update(spec.index for spec in channel_specs)
    maximum = max(observed_channels | {reference_channel}, default=0)
    existing = {spec.index for spec in channel_specs}
    for index in range(maximum + 1):
        if index not in existing:
            channel_specs.append(
                ChannelSpec(
                    index=index,
                    name=f"channel_{index}",
                    sample_rate=sample_rate,
                )
            )
    channel_specs.sort(key=lambda spec: spec.index)
    return ChannelLayout(
        channels=tuple(channel_specs),
        reference_channel=int(reference_channel),
        sample_rate=int(sample_rate),
    )


def _parse_compensation(data: dict[str, Any]) -> CompensationParams | None:
    raw = _first_present(
        data, ("compensation", "compensation_params", "resampling")
    )
    if not isinstance(raw, dict):
        return None
    return CompensationParams(
        mode=str(raw.get("mode", "windowed_sinc")),
        parameter_version=str(
            _first_present(raw, ("parameter_version", "version", "revision"))
            or data.get("parameter_version", "")
        ),
        tolerance_samples=_number(raw.get("tolerance_samples"), 2.0),
        sinc_half_taps=int(raw.get("sinc_half_taps", 16)),
        output_length=int(raw.get("output_length", 0)),
    )


def _parse_residual_summary(data: dict[str, Any]) -> ResidualSummary | None:
    raw = _first_present(
        data, ("residual_summary", "residuals", "synchronization_residuals")
    )
    if not isinstance(raw, dict):
        return None
    channels_raw = raw.get("channels", raw.get("per_channel", []))
    if isinstance(channels_raw, dict):
        channels_raw = _flatten_grouped(channels_raw)
    channels: list[ChannelResidual] = []
    for item in channels_raw:
        if not isinstance(item, dict):
            continue
        maximum = _number(
            _first_present(
                item,
                ("max_abs_delay_samples", "residual_max_samples", "max_abs"),
            ),
            0.0,
        )
        channels.append(
            ChannelResidual(
                channel=_channel_from_key(
                    _first_present(item, ("channel", "channel_id", "ch")), 0
                ),
                marker_count=int(item.get("marker_count", 0)),
                residual_rms_samples=_number(
                    _first_present(
                        item, ("residual_rms_samples", "rms_samples", "rms")
                    ),
                    0.0,
                ),
                residual_max_samples=_number(
                    _first_present(
                        item, ("residual_max_samples", "max_samples")
                    ),
                    maximum,
                ),
                max_abs_delay_samples=maximum,
                within_tolerance=bool(item.get("within_tolerance", True)),
            )
        )
    tolerance = _number(raw.get("tolerance_samples"), 2.0)
    return ResidualSummary(
        tolerance_samples=tolerance,
        worst_channel=(
            int(raw["worst_channel"])
            if raw.get("worst_channel") is not None
            else None
        ),
        max_abs_delay_samples=_number(
            raw.get("max_abs_delay_samples"), 0.0
        ),
        rms_samples=_number(raw.get("rms_samples"), 0.0),
        channels=tuple(sorted(channels, key=lambda item: item.channel)),
    )


def _parse_issues(data: dict[str, Any]) -> tuple[Issue, ...]:
    raw = data.get("issues", data.get("errors", []))
    if not isinstance(raw, list):
        return ()
    return tuple(
        Issue.from_dict(item) for item in raw if isinstance(item, dict)
    )


def _parse_segmentation(data: dict[str, Any]) -> SegmentationConfig:
    raw = data.get("segmentation")
    if isinstance(raw, dict):
        return SegmentationConfig.from_dict(raw)
    return SegmentationConfig(
        segment_length_samples=int(
            _first_present(
                data,
                ("segment_length_samples", "segment_samples", "block_size"),
            )
            or 48000
        ),
        delay_search_samples=int(data.get("delay_search_samples", 1024)),
        min_markers_per_segment=int(data.get("min_markers_per_segment", 1)),
        correlation_window_samples=int(data.get("correlation_window_samples", 8192)),
        refine_search_samples=int(data.get("refine_search_samples", 96)),
    )


def parse_report(data: dict[str, Any], *, upgrade: bool = True) -> SyncReport:
    if not isinstance(data, dict):
        raise SyncError(E_VERSION_INVALID, "report root must be a JSON object")
    version = normalize_version(
        _first_present(data, ("schema_version", "version", "report_version"))
    )
    sample_rate = int(
        _first_present(data, ("sample_rate", "sample_rate_hz", "fs")) or 48000
    )
    markers = _parse_markers(data, sample_rate)
    segmentation = _parse_segmentation(data)
    segments = _parse_segments(data, segmentation)
    fitting = FitConfig.from_dict(
        data.get("fitting") if isinstance(data.get("fitting"), dict) else None
    )
    fits = _parse_fits(data, segments, fitting)
    reference_raw = _first_present(
        data, ("reference_channel", "reference", "ref_channel")
    )
    reference_channel = (
        int(reference_raw) if reference_raw is not None else None
    )
    if reference_channel is None and not isinstance(
        _first_present(data, ("channel_layout", "layout")), dict
    ):
        observed = {marker.channel for marker in markers}
        observed.update(segment.channel for segment in segments)
        observed.update(fit.channel for fit in fits)
        reference_channel = min(observed) if observed else 0
    layout = _parse_layout(
        data,
        markers=markers,
        segments=segments,
        fits=fits,
        sample_rate=sample_rate,
        reference_channel=reference_channel,
    )
    diagnoses = _parse_diagnoses(data)
    compensation = _parse_compensation(data)
    residual_summary = _parse_residual_summary(data)
    parameter_version = str(
        _first_present(data, ("parameter_version", "revision", "params_version"))
        or ""
    )
    provenance = {
        str(key): str(value)
        for key, value in data.get("provenance", {}).items()
    } if isinstance(data.get("provenance"), dict) else {}
    integrity = (
        dict(data.get("integrity", {}))
        if isinstance(data.get("integrity"), dict)
        else {}
    )
    report = SyncReport(
        sample_rate=sample_rate,
        reference_channel=layout.reference_channel,
        channel_layout=layout,
        markers=markers,
        segment_delays=segments,
        drift_fits=fits,
        diagnoses=diagnoses,
        compensation=compensation,
        residual_summary=residual_summary,
        segmentation=segmentation,
        fitting=fitting,
        parameter_version=parameter_version,
        schema_version=version,
        source_version=str(data.get("source_version", version)),
        kind=str(data.get("kind", "sync_diagnostic_report")),
        issues=_parse_issues(data),
        provenance=provenance,
        integrity=integrity,
    )
    if upgrade and version != "v3":
        report = upgrade_report(report)
    return report


def _derive_segments_from_markers(
    report: SyncReport,
) -> tuple[SegmentDelay, ...]:
    markers_by_channel: dict[int, dict[int, Marker]] = {}
    for marker in report.markers:
        markers_by_channel.setdefault(marker.channel, {})[
            marker.marker_index
        ] = marker
    reference_markers = markers_by_channel.get(
        report.reference_channel, {}
    )
    if not reference_markers:
        return ()
    frames = int(
        max(
            (marker.arrival_sample for marker in report.markers),
            default=0.0,
        )
    )
    if frames <= 0:
        return ()
    length = report.segmentation.segment_length_samples
    segment_count = max(1, int(np.ceil((frames + 1) / length)))
    output: list[SegmentDelay] = []
    for channel in sorted(markers_by_channel):
        channel_markers = markers_by_channel[channel]
        for segment_index in range(segment_count):
            start = segment_index * length
            end = start + length
            common = sorted(
                index
                for index in set(reference_markers).intersection(channel_markers)
                if start
                <= reference_markers[index].arrival_sample
                < end
            )
            if not common:
                continue
            delays = np.array(
                [
                    channel_markers[index].arrival_sample
                    - reference_markers[index].arrival_sample
                    for index in common
                ],
                dtype=np.float64,
            )
            if channel == report.reference_channel:
                delay = 0.0
                ci_half = 0.0
                correlation = 1.0
            else:
                if delays.size >= 2:
                    line_x = np.array(
                        [
                            reference_markers[index].arrival_sample
                            for index in common
                        ],
                        dtype=np.float64,
                    )
                    design = np.column_stack(
                        (np.ones_like(line_x), line_x)
                    )
                    coefficients, _, _, _ = np.linalg.lstsq(
                        design, delays, rcond=None
                    )
                    delay = float(
                        coefficients[0]
                        + coefficients[1] * 0.5 * (start + end - 1)
                    )
                    residual = delays - design @ coefficients
                    ci_half = 1.96 * float(np.std(residual, ddof=1)) / np.sqrt(
                        delays.size
                    )
                else:
                    delay = float(delays[0])
                    ci_half = 1.0
                correlation = None
            output.append(
                SegmentDelay(
                    channel=channel,
                    segment_index=segment_index,
                    start_sample=start,
                    end_sample=end,
                    delay_samples=delay,
                    peak_correlation=correlation,
                    ci_low_samples=delay - ci_half,
                    ci_high_samples=delay + ci_half,
                    marker_count=len(common),
                    method="derived_from_legacy_markers",
                )
            )
    return tuple(output)


def upgrade_report(report: SyncReport) -> SyncReport:
    source = normalize_version(report.schema_version)
    provenance = dict(report.provenance)
    if report.markers:
        provenance.setdefault("markers", f"{source}:direct_marker_times")
    if report.segment_delays:
        provenance.setdefault("segment_delays", f"{source}:direct_segment_delays")
    if report.drift_fits:
        provenance.setdefault("drift_fits", f"{source}:direct_drift_fits")
    if report.diagnoses:
        provenance.setdefault("diagnoses", f"{source}:direct_diagnoses")
    if report.compensation is not None:
        provenance.setdefault("compensation", f"{source}:direct_compensation")
    if report.residual_summary is not None:
        provenance.setdefault(
            "residual_summary", f"{source}:direct_residual_summary"
        )
    if report.markers and not provenance.get("channel_layout"):
        provenance["channel_layout"] = (
            f"{source}:inferred_from_marker_channels"
        )
    if not report.segment_delays and report.markers:
        report.segment_delays = _derive_segments_from_markers(report)
        if report.segment_delays:
            provenance["segment_delays"] = (
                f"{source}:derived_from_marker_times"
            )
    if not report.drift_fits and report.segment_delays:
        channels = sorted(
            {segment.channel for segment in report.segment_delays}
        )
        report.drift_fits = fit_all_channels(
            report.segment_delays,
            channels=channels,
            reference_channel=report.reference_channel,
            sample_rate=report.sample_rate,
            config=report.fitting,
        )
        provenance["drift_fits"] = (
            f"{source}:derived_from_upgraded_segment_delays"
        )
    if not report.diagnoses and report.drift_fits:
        diagnoses, issues = diagnose_all_channels(
            report.segment_delays,
            report.drift_fits,
            DiagnosisConfig(
                residual_threshold_samples=report.fitting.residual_threshold_samples,
                jump_threshold_samples=report.fitting.jump_threshold_samples,
            ),
        )
        report.diagnoses = diagnoses
        report.issues = tuple(report.issues) + issues
        provenance["diagnoses"] = (
            f"{source}:derived_from_upgraded_drift_fits"
        )
    if report.compensation is None:
        provenance["compensation"] = f"unavailable_in_{source}"
    if report.residual_summary is None:
        provenance["residual_summary"] = f"unavailable_in_{source}"
    if not report.parameter_version:
        report.parameter_version = (
            f"legacy-{short_hash([marker.to_dict() for marker in report.markers])}"
        )
        provenance["parameter_version"] = f"{source}:derived_content_hash"
    report.schema_version = "v3"
    report.provenance = provenance
    report.integrity = {}
    return report


def downgrade_report(
    report: SyncReport,
    target_version: str,
) -> DowngradeResult:
    target = normalize_version(target_version)
    if target == "v3":
        output = report.to_dict()
        return DowngradeResult(document=output, losses=())
    losses: list[Issue] = []

    def loss(fields: list[str], reason: str) -> None:
        losses.append(
            Issue(
                code=E_VERSION_LOSS,
                message=reason,
                severity="warning",
                fields=fields,
            )
        )

    if target == "v2":
        lost = [
            "channel_layout",
            "compensation",
            "residual_summary",
            "provenance",
            "integrity",
        ]
        if report.issues:
            lost.append("issues")
        document: dict[str, Any] = {
            "schema_version": "v2",
            "kind": report.kind,
            "source_version": report.source_version,
            "sample_rate": report.sample_rate,
            "reference_channel": report.reference_channel,
            "parameter_version": report.parameter_version,
            "segment_length_samples": report.segmentation.segment_length_samples,
            "segmentation": {
                "segment_length_samples": report.segmentation.segment_length_samples,
                "delay_search_samples": report.segmentation.delay_search_samples,
            },
            "markers": [marker.to_dict() for marker in report.markers],
            "segments": [
                segment.to_dict() for segment in report.segment_delays
            ],
            "drift_models": [fit.to_dict() for fit in report.drift_fits],
            "diagnoses": [
                diagnosis.to_dict() for diagnosis in report.diagnoses
            ],
            "fitting": report.fitting.to_dict(),
        }
        loss(
            lost,
            "v2 cannot express v3 channel layout, compensation parameters, "
            "residual summary, provenance, or integrity metadata",
        )
    else:
        lost = [
            "segment_delays",
            "drift_fits",
            "diagnoses",
            "segmentation",
            "fitting",
            "channel_layout",
            "compensation",
            "residual_summary",
            "provenance",
            "integrity",
            "markers[*].confidence",
            "markers[*].snr_db",
        ]
        if report.issues:
            lost.append("issues")
        document = {
            "schema_version": "v1",
            "kind": report.kind,
            "source_version": report.source_version,
            "sample_rate": report.sample_rate,
            "reference_channel": report.reference_channel,
            "markers": [
                {
                    "channel": marker.channel,
                    "marker_index": marker.marker_index,
                    "arrival_sample": marker.arrival_sample,
                }
                for marker in report.markers
            ],
        }
        loss(
            lost,
            "v1 can express marker times only",
        )
    document["compatibility"] = {
        "code": E_VERSION_LOSS,
        "source_version": "v3",
        "target_version": target,
        "lost_fields": [field for issue in losses for field in issue.fields],
    }
    return DowngradeResult(document=document, losses=tuple(losses))


def report_to_version(
    report: SyncReport,
    target_version: str,
) -> DowngradeResult:
    return downgrade_report(report, target_version)


def serialize_report(
    report: SyncReport,
    target_version: str = "v3",
) -> dict[str, Any]:
    return downgrade_report(report, target_version).document


def load_report(
    path: str | Path,
    *,
    verify_integrity: bool = True,
) -> SyncReport:
    source = Path(path)
    try:
        data = json.loads(source.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SyncError(
            E_VERSION_INVALID,
            f"cannot read report JSON: {source}",
            details={"error": str(exc)},
        ) from exc
    report = parse_report(data)
    integrity = report.integrity
    if verify_integrity and integrity:
        expected = integrity.get("content_sha256")
        actual = report.content_hash()
        if expected and expected != actual:
            raise SyncError(
                E_VERSION_INVALID,
                "report content hash does not match integrity metadata",
                fields=["integrity.content_sha256"],
                details={"expected": expected, "actual": actual},
            )
    return report


def save_report(
    report: SyncReport,
    path: str | Path,
    target_version: str = "v3",
) -> DowngradeResult:
    result = downgrade_report(report, target_version)
    atomic_write_json(path, result.document)
    return result

