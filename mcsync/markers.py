"""Periodic pulse-marker detection with explicit missing-marker diagnostics."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .errors import E_MARKER_MISSING, Issue
from .models import Marker, MarkerConfig
from .wavio import AudioData


@dataclass
class MarkerDetection:
    channel: int
    markers: tuple[Marker, ...]
    issues: tuple[Issue, ...]
    expected_count: int
    noise_floor: float
    threshold: float

    @property
    def missing_indices(self) -> tuple[int, ...]:
        return tuple(issue.marker_index for issue in self.issues if issue.marker_index is not None)


@dataclass
class MarkerDetectionSet:
    channels: dict[int, MarkerDetection]
    issues: tuple[Issue, ...]
    reference_channel: int

    @property
    def markers(self) -> tuple[Marker, ...]:
        output: list[Marker] = []
        for channel in sorted(self.channels):
            output.extend(self.channels[channel].markers)
        return tuple(output)

    def markers_for(self, channel: int) -> tuple[Marker, ...]:
        detection = self.channels.get(channel)
        return detection.markers if detection is not None else ()


def _sliding_mean(values: np.ndarray, width: int) -> np.ndarray:
    width = max(1, int(width))
    if width == 1:
        return values
    left = width // 2
    right = width - left
    padded = np.pad(values, (left, right), mode="edge")
    cumulative = np.concatenate(([0.0], np.cumsum(padded, dtype=np.float64)))
    return (cumulative[width:] - cumulative[:-width]) / float(width)


def marker_envelope(
    samples: np.ndarray,
    *,
    window_samples: int,
) -> np.ndarray:
    energy = np.asarray(samples, dtype=np.float64) ** 2
    return np.sqrt(np.maximum(_sliding_mean(energy, window_samples), 0.0))


def _local_peaks(values: np.ndarray) -> np.ndarray:
    if values.size < 3:
        return np.array([], dtype=np.int64)
    middle = values[1:-1]
    mask = (middle >= values[:-2]) & (middle > values[2:])
    return np.flatnonzero(mask).astype(np.int64) + 1


def _fractional_peak(values: np.ndarray, index: int) -> float:
    if index <= 0 or index >= values.size - 1:
        return float(index)
    left = float(values[index - 1])
    center = float(values[index])
    right = float(values[index + 1])
    denominator = left - 2.0 * center + right
    if abs(denominator) < 1.0e-15:
        return float(index)
    delta = 0.5 * (left - right) / denominator
    return float(index) + float(np.clip(delta, -0.5, 0.5))


def _energy_centroid(
    samples: np.ndarray,
    center: float,
    radius: int,
) -> float:
    start = max(0, int(np.floor(center - radius)))
    end = min(samples.size, int(np.ceil(center + radius)) + 1)
    if end <= start:
        return float(center)
    energy = np.asarray(samples[start:end], dtype=np.float64) ** 2
    total = float(np.sum(energy))
    if total <= 1.0e-20:
        return float(center)
    indices = np.arange(start, end, dtype=np.float64)
    return float(np.sum(indices * energy) / total)


def _robust_noise_floor(envelope: np.ndarray) -> tuple[float, float]:
    sample = envelope
    if envelope.size > 200_000:
        stride = max(1, envelope.size // 200_000)
        sample = envelope[::stride]
    median = float(np.median(sample))
    mad = float(np.median(np.abs(sample - median)))
    sigma = 1.4826 * mad
    return median, sigma


def _peak_confidence(peak: float, noise: float, snr_db: float) -> float:
    contrast = (peak - noise) / max(peak + noise, 1.0e-12)
    snr_component = 1.0 / (1.0 + np.exp(-(snr_db - 8.0) / 5.0))
    return float(np.clip(0.65 * contrast + 0.35 * snr_component, 0.0, 1.0))


def detect_markers(
    samples: np.ndarray,
    sample_rate: int,
    config: MarkerConfig,
    *,
    channel: int = 0,
    first_marker_hint: float | None = None,
    expected_count: int | None = None,
) -> MarkerDetection:
    config.validate()
    signal = np.asarray(samples, dtype=np.float64)
    if signal.ndim != 1:
        raise ValueError("marker detector expects a mono channel")
    frame_count = int(signal.size)
    period_samples = float(config.period_seconds) * float(sample_rate)
    smoothing = config.envelope_smoothing_samples or config.pulse_duration_samples
    smoothing = max(3, min(int(smoothing), max(3, frame_count)))
    envelope = marker_envelope(signal, window_samples=smoothing)
    peaks = _local_peaks(envelope)
    noise, noise_sigma = _robust_noise_floor(envelope)
    threshold = noise + max(6.0 * noise_sigma, 0.02)
    strong = peaks[envelope[peaks] >= threshold]
    if strong.size == 0:
        strong = peaks

    first_hint = (
        float(first_marker_hint)
        if first_marker_hint is not None
        else config.first_marker_sample
    )
    if first_hint is None:
        first_window_end = min(frame_count, max(1, int(round(period_samples))))
        window_peaks = strong[strong < first_window_end]
        if window_peaks.size == 0:
            window_peaks = strong
        if window_peaks.size == 0:
            first_hint = period_samples * 0.5
        else:
            best = window_peaks[np.argmax(envelope[window_peaks])]
            first_hint = _energy_centroid(
                signal,
                _fractional_peak(envelope, int(best)),
                config.pulse_duration_samples,
            )

    if expected_count is None:
        if config.expected_marker_count is not None:
            expected_count = int(config.expected_marker_count)
        else:
            expected_count = max(
                0,
                int(np.floor((frame_count - first_hint) / period_samples)) + 1,
            )
    expected_count = max(0, int(expected_count))

    accepted: list[Marker] = []
    issues: list[Issue] = []
    last_position = float(first_hint)
    last_index = 0
    rate_estimate = 1.0
    search_radius = max(8.0, period_samples * config.search_fraction)

    for marker_index in range(expected_count):
        if marker_index == 0:
            predicted = float(first_hint)
        else:
            gap = marker_index - last_index
            predicted = last_position + gap * period_samples * rate_estimate
        low = max(0, int(np.floor(predicted - search_radius)))
        high = min(frame_count, int(np.ceil(predicted + search_radius)) + 1)
        candidates = peaks[(peaks >= low) & (peaks < high)]
        if candidates.size:
            values = envelope[candidates]
            best_position = int(candidates[int(np.argmax(values))])
            peak_value = float(envelope[best_position])
        else:
            best_position = -1
            peak_value = float(noise)
        snr_db = 20.0 * np.log10(max(peak_value, 1.0e-12) / max(noise, 1.0e-12))
        confidence = _peak_confidence(peak_value, noise, float(snr_db))
        accepted_marker = (
            best_position >= 0
            and peak_value >= threshold
            and snr_db >= config.min_snr_db
            and confidence >= config.min_confidence
        )
        if not accepted_marker:
            issues.append(
                Issue(
                    code=E_MARKER_MISSING,
                    message=(
                        f"marker {marker_index} is missing or below the configured "
                        f"SNR/confidence threshold on channel {channel}"
                    ),
                    channel=channel,
                    marker_index=marker_index,
                    details={
                        "expected_sample": predicted,
                        "search_start": low,
                        "search_end": high,
                        "snr_db": float(snr_db),
                        "confidence": confidence,
                        "minimum_snr_db": config.min_snr_db,
                        "minimum_confidence": config.min_confidence,
                    },
                )
            )
            continue
        arrival = _energy_centroid(
            signal,
            _fractional_peak(envelope, best_position),
            config.pulse_duration_samples,
        )
        marker = Marker(
            channel=channel,
            marker_index=marker_index,
            arrival_sample=arrival,
            confidence=confidence,
            snr_db=float(snr_db),
        )
        accepted.append(marker)
        if marker_index > last_index:
            observed_rate = (arrival - last_position) / (
                (marker_index - last_index) * period_samples
            )
            if 0.98 <= observed_rate <= 1.02:
                rate_estimate = 0.7 * rate_estimate + 0.3 * observed_rate
        last_position = arrival
        last_index = marker_index

    return MarkerDetection(
        channel=channel,
        markers=tuple(accepted),
        issues=tuple(issues),
        expected_count=expected_count,
        noise_floor=noise,
        threshold=threshold,
    )


def detect_all_markers(
    audio: AudioData,
    config: MarkerConfig,
    *,
    reference_channel: int = 0,
    channels: Iterable[int] | None = None,
) -> MarkerDetectionSet:
    channel_list = tuple(
        range(audio.channel_count) if channels is None else sorted(set(channels))
    )
    if reference_channel not in channel_list:
        channel_list = (reference_channel,) + channel_list
    reference_detection = detect_markers(
        audio.channel(reference_channel),
        audio.sample_rate,
        config,
        channel=reference_channel,
    )
    hint: float | None = (
        reference_detection.markers[0].arrival_sample
        if reference_detection.markers
        else None
    )
    expected_count = reference_detection.expected_count
    detections: dict[int, MarkerDetection] = {
        reference_channel: reference_detection
    }
    all_issues = list(reference_detection.issues)
    for channel in channel_list:
        if channel == reference_channel:
            continue
        detection = detect_markers(
            audio.channel(channel),
            audio.sample_rate,
            config,
            channel=channel,
            first_marker_hint=hint,
            expected_count=expected_count,
        )
        detections[channel] = detection
        all_issues.extend(detection.issues)
    return MarkerDetectionSet(
        channels=detections,
        issues=tuple(all_issues),
        reference_channel=reference_channel,
    )


def marker_detection_summary(
    detection_set: MarkerDetectionSet,
) -> list[dict[str, Any]]:
    return [
        {
            "channel": channel,
            "expected_count": result.expected_count,
            "detected_count": len(result.markers),
            "missing_indices": list(result.missing_indices),
            "noise_floor": result.noise_floor,
            "threshold": result.threshold,
        }
        for channel, result in sorted(detection_set.channels.items())
    ]

