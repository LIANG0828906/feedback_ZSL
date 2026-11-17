from __future__ import annotations

from dataclasses import dataclass
from typing import Sequence

import numpy as np

from .manifest import ChannelSpec, ReflectionSpec, ReflectionWindow
from .synth import shift_signal


@dataclass(frozen=True)
class DetectionResult:
    channel: str
    status: str
    detected_samples: tuple[int, ...]
    reference_samples: tuple[int, ...]
    alignment_shift: int | None
    residual_errors: tuple[int, ...]
    message: str | None = None

    @property
    def ok(self) -> bool:
        return self.status == "OK"


@dataclass(frozen=True)
class ReflectionEstimate:
    channel: str
    ambient_floor_dbfs: float
    delays_samples: tuple[int, ...]
    delays_ms: tuple[float, ...]
    gains_db: tuple[float, ...]
    declared_delays_samples: tuple[int, ...]
    declared_gains_db: tuple[float, ...]
    confidence: tuple[float, ...]


@dataclass(frozen=True)
class CompensationResult:
    signal: np.ndarray
    window_energy_before: float
    window_energy_after: float
    reduction_db: float
    peak_before: float
    peak_after: float
    peak_loss_db: float
    outside_unchanged: bool


def _mono(samples: np.ndarray) -> np.ndarray:
    if samples.ndim == 1:
        return samples.astype(np.float64, copy=False)
    return np.mean(samples.astype(np.float64, copy=False), axis=1)


def _detect_one(
    signal: np.ndarray,
    expected_sample: int,
    sample_rate: int,
    search_ms: float,
) -> int | None:
    radius = max(8, int(round(sample_rate * search_ms / 1000.0)))
    start = max(0, expected_sample - radius)
    end = min(signal.size, expected_sample + radius + 1)
    if end - start < 8:
        return None
    derivative = np.abs(np.diff(signal, prepend=signal[0]))
    segment = derivative[start:end]
    peak_offset = int(np.argmax(segment))
    peak = float(segment[peak_offset])
    if peak <= 1e-7:
        return None

    pre_start = max(0, start - radius)
    pre_end = max(pre_start + 1, start)
    pre = derivative[pre_start:pre_end]
    baseline = float(np.median(pre)) if pre.size else 0.0
    threshold = max(4.0 * baseline, 0.035 * peak)
    if peak < 8.0 * max(baseline, 1e-9):
        return None

    onset = peak_offset
    while onset > 0 and segment[onset - 1] > threshold:
        onset -= 1
    return start + onset


def detect_channel(
    channel: ChannelSpec,
    signal: np.ndarray,
    sample_rate: int,
) -> DetectionResult:
    references = channel.reference_samples(sample_rate)
    detected: list[int] = []
    for expected in references:
        value = _detect_one(signal, expected, sample_rate, search_ms=75.0)
        if value is None:
            return DetectionResult(
                channel=channel.channel,
                status="DETECTION_FAILED",
                detected_samples=tuple(detected),
                reference_samples=references,
                alignment_shift=None,
                residual_errors=(),
                message=f"transient near sample {expected} was not detected",
            )
        detected.append(value)
    if not detected:
        return DetectionResult(
            channel=channel.channel,
            status="DETECTION_FAILED",
            detected_samples=(),
            reference_samples=references,
            alignment_shift=None,
            residual_errors=(),
            message="no reference times were supplied",
        )

    raw_errors = np.asarray(detected, dtype=np.int64) - np.asarray(references, dtype=np.int64)
    shift = int(-round(float(np.median(raw_errors))))
    residual = tuple(int(value) for value in raw_errors + shift)
    if any(abs(value) > 1 for value in residual):
        return DetectionResult(
            channel=channel.channel,
            status="DETECTION_FAILED",
            detected_samples=tuple(detected),
            reference_samples=references,
            alignment_shift=None,
            residual_errors=residual,
            message="detected points do not share a single-sample alignment",
        )
    return DetectionResult(
        channel=channel.channel,
        status="OK",
        detected_samples=tuple(detected),
        reference_samples=references,
        alignment_shift=shift,
        residual_errors=residual,
    )


def align_channel(signal: np.ndarray, shift: int | None) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    if shift is None or shift == 0:
        return values.copy()
    if values.ndim == 1:
        return shift_signal(values, shift)
    shifted = np.zeros_like(values)
    count = values.shape[0]
    if shift > 0:
        shifted[shift:] = values[: count - shift]
    elif shift < 0:
        shifted[: count + shift] = values[-shift:]
    return shifted


def _window_union(
    reference_samples: Sequence[int],
    window: ReflectionWindow,
    sample_rate: int,
    length: int,
) -> np.ndarray:
    mask = np.zeros(length, dtype=bool)
    start_offset = int(round(window.start_ms * sample_rate / 1000.0))
    end_offset = int(round(window.end_ms * sample_rate / 1000.0))
    for reference in reference_samples:
        start = max(0, reference + start_offset)
        end = min(length, reference + end_offset)
        if end > start:
            mask[start:end] = True
    return mask


def _energy(values: np.ndarray) -> float:
    if values.size == 0:
        return 0.0
    return float(np.sum(values.astype(np.float64) ** 2))


def _db(value: float, floor: float = 1e-30) -> float:
    return float(10.0 * np.log10(max(value, floor)))


def _highpass_transient(signal: np.ndarray, sample_rate: int) -> np.ndarray:
    frequency = min(12000.0, 0.42 * sample_rate)
    quality = 1.0 / np.sqrt(2.0)
    omega = 2.0 * np.pi * frequency / sample_rate
    cosine = np.cos(omega)
    alpha = np.sin(omega) / (2.0 * quality)
    b0 = (1.0 + cosine) / 2.0
    b1 = -(1.0 + cosine)
    b2 = b0
    a0 = 1.0 + alpha
    a1 = -2.0 * cosine
    a2 = 1.0 - alpha
    b0, b1, b2 = b0 / a0, b1 / a0, b2 / a0
    a1, a2 = a1 / a0, a2 / a0
    output = np.empty_like(signal)
    x1 = x2 = y1 = y2 = 0.0
    for index in range(signal.size):
        x0 = float(signal[index])
        y0 = b0 * x0 + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        output[index] = y0
        x2, x1 = x1, x0
        y2, y1 = y1, y0
    return output


def estimate_reflections(
    channel: ChannelSpec,
    signal: np.ndarray,
    ambient_signal: np.ndarray,
    reflection: ReflectionSpec,
    sample_rate: int,
) -> ReflectionEstimate:
    declared_delays = reflection.delay_samples(sample_rate)
    declared_gains = reflection.gains_db
    ambient_segment = ambient_signal[: signal.size]
    ambient_rms = float(np.sqrt(np.mean(ambient_segment.astype(np.float64) ** 2))) if (
        ambient_segment.size
    ) else 0.0
    ambient_floor_db = 20.0 * np.log10(max(ambient_rms, 1e-12))
    if (
        channel.reflection_window is None
        or not declared_delays
        or not channel.early_reflections
    ):
        return ReflectionEstimate(
            channel=channel.channel,
            ambient_floor_dbfs=float(ambient_floor_db),
            delays_samples=(),
            delays_ms=(),
            gains_db=(),
            declared_delays_samples=declared_delays,
            declared_gains_db=declared_gains,
            confidence=(),
        )

    window = channel.reflection_window
    template_length = max(8, int(round(0.0011 * sample_rate)))
    window_start = int(round(window.start_ms * sample_rate / 1000.0))
    window_end = int(round(window.end_ms * sample_rate / 1000.0))
    search_radius = max(8, int(round(0.001 * sample_rate)))
    references = channel.reference_samples(sample_rate)
    highpassed = _highpass_transient(signal, sample_rate)
    ambient_highpassed = _highpass_transient(ambient_segment, sample_rate)
    estimated_delays: list[int] = []
    estimated_gains: list[float] = []
    confidences: list[float] = []

    for declared_delay, declared_gain in zip(declared_delays, declared_gains):
        candidates: list[tuple[float, int, float]] = []
        for lag in range(
            max(1, max(window_start, declared_delay - search_radius)),
            min(window_end, declared_delay + search_radius) + 1,
        ):
            correlations: list[float] = []
            gains: list[float] = []
            for reference in references:
                start = reference
                candidate_start = reference + lag
                if (
                    start + template_length > signal.size
                    or candidate_start + template_length > signal.size
                ):
                    continue
                template = highpassed[start : start + template_length]
                candidate = highpassed[
                    candidate_start : candidate_start + template_length
                ]
                template_energy = float(np.dot(template, template))
                candidate_energy = float(np.dot(candidate, candidate))
                noise_energy = (
                    float(np.mean(ambient_highpassed.astype(np.float64) ** 2))
                    * template_length
                )
                template_energy = max(template_energy - noise_energy, 1e-12)
                candidate_energy = max(candidate_energy - noise_energy, 1e-12)
                if template_energy <= 1e-12 or candidate_energy <= 2.0 * noise_energy:
                    continue
                confidence = float(
                    np.dot(template, candidate)
                    / np.sqrt(template_energy * candidate_energy)
                )
                gain = float(np.dot(template, candidate) / template_energy)
                correlations.append(confidence)
                gains.append(max(gain, 1e-12))
            if correlations:
                candidates.append(
                    (
                        float(np.median(correlations)),
                        lag,
                        float(np.median(gains)),
                    )
                )
        if candidates:
            confidence, delay, gain = max(candidates, key=lambda item: item[0])
            if confidence >= 0.12:
                estimated_delays.append(delay)
                estimated_gains.append(20.0 * np.log10(max(gain, 1e-12)))
                confidences.append(confidence)
                continue
        estimated_delays.append(declared_delay)
        estimated_gains.append(declared_gain)
        confidences.append(0.0)

    return ReflectionEstimate(
        channel=channel.channel,
        ambient_floor_dbfs=float(ambient_floor_db),
        delays_samples=tuple(estimated_delays),
        delays_ms=tuple(value * 1000.0 / sample_rate for value in estimated_delays),
        gains_db=tuple(float(value) for value in estimated_gains),
        declared_delays_samples=declared_delays,
        declared_gains_db=declared_gains,
        confidence=tuple(confidences),
    )


def compensation_curve(
    reference_samples: Sequence[int],
    window: ReflectionWindow,
    sample_rate: int,
    length: int,
    attenuation_db: float = -18.0,
) -> np.ndarray:
    curve = np.ones(length, dtype=np.float64)
    target = 10.0 ** (attenuation_db / 20.0)
    ramp = max(1, int(round(0.001 * sample_rate)))
    start_offset = int(round(window.start_ms * sample_rate / 1000.0))
    end_offset = int(round(window.end_ms * sample_rate / 1000.0))
    for reference in reference_samples:
        start = max(0, reference + start_offset)
        end = min(length, reference + end_offset)
        if end <= start:
            continue
        local = np.full(end - start, target, dtype=np.float64)
        fade = min(ramp, local.size)
        phase = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, fade, endpoint=True))
        local[:fade] = 1.0 + (target - 1.0) * phase
        local[-fade:] = np.minimum(local[-fade:], local[-fade:][::-1])
        curve[start:end] = np.minimum(curve[start:end], local)
    return curve


def compensate_reflections(
    channel: ChannelSpec,
    signal: np.ndarray,
    sample_rate: int,
    attenuation_db: float = -18.0,
) -> CompensationResult:
    if (
        channel.reflection_window is None
        or not channel.reference_samples(sample_rate)
        or not channel.early_reflections
    ):
        return CompensationResult(
            signal=signal.copy(),
            window_energy_before=0.0,
            window_energy_after=0.0,
            reduction_db=0.0,
            peak_before=float(np.max(np.abs(signal), initial=0.0)),
            peak_after=float(np.max(np.abs(signal), initial=0.0)),
            peak_loss_db=0.0,
            outside_unchanged=True,
        )
    references = channel.reference_samples(sample_rate)
    mask = _window_union(references, channel.reflection_window, sample_rate, signal.size)
    curve = compensation_curve(
        references,
        channel.reflection_window,
        sample_rate,
        signal.size,
        attenuation_db,
    )
    output = signal * curve
    before = _energy(signal[mask])
    after = _energy(output[mask])
    reduction = _db(before) - _db(after)
    peak_before = float(np.max(np.abs(signal), initial=0.0))
    peak_after = float(np.max(np.abs(output), initial=0.0))
    peak_loss = 20.0 * np.log10(max(peak_before, 1e-12) / max(peak_after, 1e-12))
    outside = ~mask
    outside_unchanged = bool(np.array_equal(output[outside], signal[outside]))
    return CompensationResult(
        signal=output,
        window_energy_before=before,
        window_energy_after=after,
        reduction_db=float(reduction),
        peak_before=peak_before,
        peak_after=peak_after,
        peak_loss_db=float(peak_loss),
        outside_unchanged=outside_unchanged,
    )


def mono_channel(samples: np.ndarray) -> np.ndarray:
    return _mono(samples)
