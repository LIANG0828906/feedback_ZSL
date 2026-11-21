"""Known-period sync pulse detection with explicit missing-marker reporting."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .models import MarkerChannelResult, MarkerHit, MarkerSpec

_EPS = 1e-12


def _moving_rms(signal: np.ndarray, width: int) -> np.ndarray:
    if signal.size == 0:
        return np.empty(0, dtype=np.float64)
    width = max(1, min(width, signal.size))
    squared = np.square(signal, dtype=np.float64)
    cumulative = np.concatenate(([0.0], np.cumsum(squared, dtype=np.float64)))
    shape = signal.size - width + 1
    if shape <= 0:
        return np.sqrt(np.mean(squared, keepdims=True))
    energy = (cumulative[width:] - cumulative[:-width]) / float(width)
    return np.sqrt(np.maximum(energy, 0.0))


def _parabolic_peak(profile: np.ndarray, index: int) -> tuple[float, float]:
    if index <= 0 or index >= profile.size - 1:
        return float(profile[index]), float(index)
    left, center, right = profile[index - 1 : index + 2]
    denominator = left - 2.0 * center + right
    if abs(denominator) < _EPS:
        return float(center), float(index)
    offset = 0.5 * (left - right) / denominator
    offset = float(np.clip(offset, -0.5, 0.5))
    refined = center - 0.25 * (left - right) * offset
    return float(max(refined, center)), float(index) + offset


def _profile_for_window(window: np.ndarray, spec: MarkerSpec) -> tuple[np.ndarray, float]:
    """Return a peak profile and the offset from profile index to pulse center."""

    if spec.template is not None:
        template = np.asarray(spec.template, dtype=np.float64)
        template = template - np.mean(template)
        template_norm = float(np.linalg.norm(template))
        if template_norm > _EPS and window.size >= template.size:
            profile = np.abs(np.correlate(window - np.mean(window), template, mode="valid"))
            profile = profile / template_norm
            return profile, (template.size - 1) / 2.0
    return _moving_rms(window, spec.pulse_width_samples), (spec.pulse_width_samples - 1) / 2.0


@dataclass
class _WindowPeak:
    marker_index: int
    expected: int
    peak: float
    arrival: float
    noise: float
    snr: float


def _detect_array(
    signal: np.ndarray,
    spec: MarkerSpec,
    min_snr: float,
) -> MarkerChannelResult | tuple[list[_WindowPeak], float]:
    sample_count = int(signal.size)
    peaks: list[_WindowPeak] = []
    margin = max(1, int(round(spec.pulse_width_samples * 0.75)))

    for marker_index, expected in enumerate(spec.expected_samples):
        if expected >= sample_count:
            peaks.append(
                _WindowPeak(marker_index, expected, 0.0, float(expected), 0.0, 0.0)
            )
            continue

        start = max(0, expected - spec.search_radius_samples - margin)
        stop = min(sample_count, expected + spec.search_radius_samples + margin + 1)
        window = np.asarray(signal[start:stop], dtype=np.float64)
        profile, center_offset = _profile_for_window(window, spec)
        if profile.size == 0:
            peaks.append(
                _WindowPeak(marker_index, expected, 0.0, float(expected), 0.0, 0.0)
            )
            continue

        local_index = int(np.argmax(profile))
        peak, fractional_index = _parabolic_peak(profile, local_index)
        local_noise = float(np.median(profile))
        snr = peak / (local_noise + _EPS)
        arrival_center = start + fractional_index + center_offset
        peaks.append(
            _WindowPeak(
                marker_index=marker_index,
                expected=expected,
                peak=peak,
                arrival=float(arrival_center),
                noise=local_noise,
                snr=float(snr),
            )
        )

    maximum = max((peak.peak for peak in peaks), default=0.0)
    marker_hits: list[MarkerHit] = []
    missing: list[int] = []
    for peak in peaks:
        floor = peak.noise
        confidence = (peak.peak - floor) / max(maximum - floor, _EPS)
        confidence = float(np.clip(confidence, 0.0, 1.0))
        accepted = bool(
            confidence >= spec.min_confidence
            and peak.snr >= min_snr
            and maximum > _EPS
        )
        if not accepted:
            missing.append(peak.marker_index)
        marker_hits.append(
            MarkerHit(
                marker_index=peak.marker_index,
                arrival_sample=peak.arrival if accepted else None,
                confidence=confidence,
                expected_sample=peak.expected,
                accepted=accepted,
            )
        )

    return peaks, maximum


def detect_markers_array(
    signal: np.ndarray,
    channel: int,
    spec: MarkerSpec,
    min_snr: float = 5.0,
) -> MarkerChannelResult:
    """Detect each expected marker in a channel.

    Detection is deliberately bounded to each expected marker window. This avoids
    a low-SNR peak from silently taking the slot of a missing marker.
    """

    peaks, _ = _detect_array(np.asarray(signal, dtype=np.float64), spec, min_snr)
    maximum = max((peak.peak for peak in peaks), default=0.0)
    hits: list[MarkerHit] = []
    missing: list[int] = []
    snrs: list[float] = []
    for peak in peaks:
        confidence = (peak.peak - peak.noise) / max(maximum - peak.noise, _EPS)
        confidence = float(np.clip(confidence, 0.0, 1.0))
        accepted = bool(
            confidence >= spec.min_confidence
            and peak.snr >= min_snr
            and maximum > _EPS
        )
        if accepted:
            snrs.append(peak.snr)
        else:
            missing.append(peak.marker_index)
        hits.append(
            MarkerHit(
                marker_index=peak.marker_index,
                arrival_sample=peak.arrival if accepted else None,
                confidence=confidence,
                expected_sample=peak.expected,
                accepted=accepted,
            )
        )
    median_snr = float(np.median(snrs)) if snrs else 0.0
    return MarkerChannelResult(
        channel=channel,
        markers=tuple(hits),
        missing_indices=tuple(missing),
        peak_to_noise=median_snr,
    )


def detect_markers_reader(
    reader,
    channel: int,
    spec: MarkerSpec,
    min_snr: float = 5.0,
    start: int = 0,
    stop: int | None = None,
) -> MarkerChannelResult:
    """Detect markers while reading only the expected marker windows from disk."""

    stop = reader.frames if stop is None else min(stop, reader.frames)
    peaks: list[_WindowPeak] = []
    margin = max(1, int(round(spec.pulse_width_samples * 0.75)))
    maximum = 0.0
    for marker_index, expected in enumerate(spec.expected_samples):
        if expected < start or expected >= stop:
            peaks.append(
                _WindowPeak(marker_index, expected, 0.0, float(expected), 0.0, 0.0)
            )
            continue
        window_start = max(start, expected - spec.search_radius_samples - margin)
        window_stop = min(stop, expected + spec.search_radius_samples + margin + 1)
        window_i16 = reader.read_i16(channel, window_start, window_stop)
        window = window_i16.astype(np.float64) / 32768.0
        profile, center_offset = _profile_for_window(window, spec)
        if profile.size == 0:
            peaks.append(
                _WindowPeak(marker_index, expected, 0.0, float(expected), 0.0, 0.0)
            )
            continue
        local_index = int(np.argmax(profile))
        peak, fractional_index = _parabolic_peak(profile, local_index)
        local_noise = float(np.median(profile))
        arrival_center = window_start + fractional_index + center_offset
        peaks.append(
            _WindowPeak(
                marker_index=marker_index,
                expected=expected,
                peak=peak,
                arrival=float(arrival_center),
                noise=local_noise,
                snr=float(peak / (local_noise + _EPS)),
            )
        )
        maximum = max(maximum, peak)

    hits: list[MarkerHit] = []
    missing: list[int] = []
    snrs: list[float] = []
    for peak in peaks:
        confidence = (peak.peak - peak.noise) / max(maximum - peak.noise, _EPS)
        confidence = float(np.clip(confidence, 0.0, 1.0))
        accepted = bool(
            confidence >= spec.min_confidence
            and peak.snr >= min_snr
            and maximum > _EPS
        )
        if accepted:
            snrs.append(peak.snr)
        else:
            missing.append(peak.marker_index)
        hits.append(
            MarkerHit(
                marker_index=peak.marker_index,
                arrival_sample=peak.arrival if accepted else None,
                confidence=confidence,
                expected_sample=peak.expected,
                accepted=accepted,
            )
        )
    return MarkerChannelResult(
        channel=channel,
        markers=tuple(hits),
        missing_indices=tuple(missing),
        peak_to_noise=float(np.median(snrs)) if snrs else 0.0,
    )
