"""Band analysis, transition detection/compensation, and mastering DSP."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .audio import (
    db_from_energy,
    integrated_loudness_lufs,
    oversampled_peak,
    true_peak_dbtp,
)


@dataclass(frozen=True)
class TransitionInterval:
    instrument: str
    start_seconds: float
    end_seconds: float
    band_jump_db: float

    def as_dict(self) -> dict[str, Any]:
        return {
            "instrument": self.instrument,
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "band_jump_db": self.band_jump_db,
        }


@dataclass(frozen=True)
class CompensationResult:
    compensated: np.ndarray
    entries: tuple[dict[str, Any], ...]


def band_mask(frequencies: np.ndarray, low_hz: float, high_hz: float) -> np.ndarray:
    width = high_hz - low_hz
    edge = min(120.0, max(1.0, width * 0.12))
    frequencies = np.asarray(frequencies, dtype=np.float64)
    mask = np.zeros_like(frequencies)
    left_end = low_hz + edge
    right_start = high_hz - edge
    if right_start <= left_end:
        mask[(frequencies >= low_hz) & (frequencies <= high_hz)] = 1.0
        return mask
    rising = (frequencies > low_hz) & (frequencies < left_end)
    mask[rising] = 0.5 - 0.5 * np.cos(
        np.pi * (frequencies[rising] - low_hz) / edge
    )
    mask[(frequencies >= left_end) & (frequencies <= right_start)] = 1.0
    falling = (frequencies > right_start) & (frequencies < high_hz)
    mask[falling] = 0.5 + 0.5 * np.cos(
        np.pi * (frequencies[falling] - right_start) / edge
    )
    return mask


def fft_band_component(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    frames = values.shape[0]
    if frames == 0:
        return values.copy()
    nfft = 1
    while nfft < max(2, frames * 2):
        nfft <<= 1
    frequencies = np.fft.rfftfreq(nfft, d=1.0 / sample_rate)
    mask = band_mask(frequencies, low_hz, high_hz)
    output = np.empty_like(values)
    for channel in range(values.shape[1]):
        spectrum = np.fft.rfft(values[:, channel], n=nfft)
        output[:, channel] = np.fft.irfft(spectrum * mask, n=nfft)[:frames]
    return output


def _stft_powers(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
    *,
    frame_size: int,
    hop_size: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    mono = np.mean(np.asarray(signal, dtype=np.float64), axis=1)
    frames = mono.shape[0]
    if frames == 0:
        empty = np.zeros(0, dtype=np.float64)
        return empty, empty, empty, empty
    count = 1 + max(0, (frames - 1) // hop_size)
    padded_length = max(frames, (count - 1) * hop_size + frame_size)
    padded = np.zeros(padded_length, dtype=np.float64)
    padded[:frames] = mono
    window = np.hanning(frame_size)
    frequency_bins = np.fft.rfftfreq(frame_size, d=1.0 / sample_rate)
    band = band_mask(frequency_bins, low_hz, high_hz)
    total_band = (frequency_bins >= 80.0) & (frequency_bins <= min(20000.0, sample_rate * 0.48))
    powers = np.empty((count, frequency_bins.shape[0]), dtype=np.float64)
    for index in range(count):
        start = index * hop_size
        frame = padded[start : start + frame_size]
        spectrum = np.fft.rfft(frame * window)
        powers[index] = np.abs(spectrum) ** 2
    band_power = powers[:, band > 0.0] @ (band[band > 0.0] ** 2)
    total_power = np.sum(powers[:, total_band], axis=1)
    times = (np.arange(count, dtype=np.float64) * hop_size + frame_size / 2.0) / sample_rate
    return band_power, total_power, times, frequency_bins


def _fill_short_false_runs(active: np.ndarray, max_gap_frames: int) -> np.ndarray:
    result = active.copy()
    index = 0
    length = result.shape[0]
    while index < length:
        if result[index]:
            index += 1
            continue
        end = index
        while end < length and not result[end]:
            end += 1
        if index > 0 and end < length and end - index <= max_gap_frames:
            result[index:end] = True
        index = end
    return result


def _remove_short_true_runs(active: np.ndarray, min_frames: int) -> np.ndarray:
    result = active.copy()
    index = 0
    length = result.shape[0]
    while index < length:
        if not result[index]:
            index += 1
            continue
        end = index
        while end < length and result[end]:
            end += 1
        if end - index < min_frames:
            result[index:end] = False
        index = end
    return result


def detect_transition_intervals(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
    *,
    instrument: str,
    threshold_db: float = 7.0,
) -> tuple[TransitionInterval, ...]:
    frame_size = 2048 if sample_rate >= 32000 else 1024
    hop_size = max(1, frame_size // 8)
    band_power, total_power, times, _ = _stft_powers(
        signal,
        sample_rate,
        low_hz,
        high_hz,
        frame_size=frame_size,
        hop_size=hop_size,
    )
    if times.size == 0:
        return ()
    valid = total_power > 1e-12
    if not np.any(valid):
        return ()
    ratios = np.full(times.shape, -240.0, dtype=np.float64)
    ratios[valid] = 10.0 * np.log10(
        np.maximum(band_power[valid], 1e-30) / total_power[valid]
    )
    baseline = float(np.median(ratios[valid]))
    active = valid & (ratios > baseline + threshold_db)
    hop_seconds = hop_size / sample_rate
    active = _fill_short_false_runs(active, max(1, int(round(0.045 / hop_seconds))))
    active = _remove_short_true_runs(active, max(1, int(round(0.180 / hop_seconds))))
    if not np.any(active):
        return ()

    coarse: list[tuple[float, float]] = []
    index = 0
    while index < active.shape[0]:
        if not active[index]:
            index += 1
            continue
        end = index
        while end < active.shape[0] and active[end]:
            end += 1
        start_seconds = float(times[index])
        end_seconds = float(times[end - 1] + hop_seconds)
        coarse.append((start_seconds, end_seconds))
        index = end

    mono = np.mean(np.asarray(signal, dtype=np.float64), axis=1)
    output: list[TransitionInterval] = []
    for coarse_start, coarse_end in coarse:
        start, end = coarse_start, coarse_end
        start = max(0.0, start)
        end = min(mono.shape[0] / sample_rate, end)
        if end - start < 0.180:
            continue
        jump, _, _ = interval_band_metrics(
            mono[:, None],
            sample_rate,
            low_hz,
            high_hz,
            start,
            end,
        )
        output.append(
            TransitionInterval(
                instrument=instrument,
                start_seconds=round(start, 9),
                end_seconds=round(end, 9),
                band_jump_db=round(jump, 9),
            )
        )
    return tuple(output)


def interval_band_metrics(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
    start_seconds: float,
    end_seconds: float,
    *,
    guard_seconds: float = 0.22,
) -> tuple[float, float, float]:
    band = fft_band_component(signal, sample_rate, low_hz, high_hz)
    mono = np.mean(band, axis=1)
    start = max(0, int(round(start_seconds * sample_rate)))
    end = min(mono.shape[0], int(round(end_seconds * sample_rate)))
    if end <= start:
        return -240.0, -240.0, -240.0
    guard = int(round(guard_seconds * sample_rate))
    before = mono[max(0, start - guard) : max(0, start - int(0.03 * sample_rate))]
    after = mono[
        min(mono.shape[0], end + int(0.03 * sample_rate)) : min(
            mono.shape[0], end + guard
        )
    ]
    baseline = np.concatenate([before, after])
    baseline_energy = float(np.mean(baseline * baseline)) if baseline.size else 0.0
    interval_energy = float(np.mean(mono[start:end] * mono[start:end]))
    if baseline_energy <= 1e-24:
        return db_from_energy(interval_energy), -240.0, db_from_energy(interval_energy)
    jump = 10.0 * math.log10(max(interval_energy, 1e-24) / baseline_energy)
    return (
        jump,
        db_from_energy(interval_energy),
        db_from_energy(baseline_energy),
    )


def _ola_stft(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
    intervals: tuple[TransitionInterval, ...],
) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    frames = values.shape[0]
    if frames == 0 or not intervals:
        return values.copy()
    frame_size = 2048 if sample_rate >= 32000 else 1024
    hop_size = frame_size // 4
    count = 1 + max(0, (frames - 1) // hop_size)
    padded_length = max(frames, (count - 1) * hop_size + frame_size)
    window = np.hanning(frame_size)
    frequency_bins = np.fft.rfftfreq(frame_size, d=1.0 / sample_rate)
    mask = band_mask(frequency_bins, low_hz, high_hz)
    bin_indices = np.flatnonzero(mask > 0.0)
    times = (np.arange(count, dtype=np.float64) * hop_size + frame_size / 2.0) / sample_rate
    band_powers = np.zeros(count, dtype=np.float64)
    spectra = np.empty((count, frequency_bins.shape[0]), dtype=np.complex128)
    for index in range(count):
        start = index * hop_size
        source = np.zeros(frame_size, dtype=np.float64)
        available = min(frame_size, frames - start)
        if available > 0:
            source[:available] = values[start : start + available, 0]
        spectrum = np.fft.rfft(source * window)
        spectra[index] = spectrum
        band_powers[index] = float(
            np.sum(np.abs(spectrum[bin_indices]) ** 2 * mask[bin_indices] ** 2)
        )

    outside = np.ones(count, dtype=bool)
    for interval in intervals:
        outside &= ~((times >= interval.start_seconds) & (times <= interval.end_seconds))
    valid = band_powers > 1e-24
    if np.any(valid & outside):
        baseline_power = float(np.median(band_powers[valid & outside]))
    elif np.any(valid):
        baseline_power = float(np.median(band_powers[valid]))
    else:
        return values.copy()

    gains = np.ones(count, dtype=np.float64)
    for interval in intervals:
        active = (times >= interval.start_seconds) & (times <= interval.end_seconds)
        for index in np.flatnonzero(active):
            if band_powers[index] <= baseline_power:
                continue
            gains[index] = min(1.0, math.sqrt(baseline_power / band_powers[index]))
        indices = np.flatnonzero(active)
        if indices.size >= 3:
            local = gains[indices]
            smoothed = local.copy()
            smoothed[1:-1] = 0.25 * local[:-2] + 0.5 * local[1:-1] + 0.25 * local[2:]
            gains[indices] = np.minimum(local, smoothed)

    output = np.zeros((padded_length, values.shape[1]), dtype=np.float64)
    normalizer = np.zeros(padded_length, dtype=np.float64)
    for index in range(count):
        spectrum = spectra[index].copy()
        if gains[index] < 1.0:
            spectrum[bin_indices] *= gains[index]
        frame = np.fft.irfft(spectrum, n=frame_size) * window
        start = index * hop_size
        output[start : start + frame_size, 0] += frame
        normalizer[start : start + frame_size] += window * window
    valid_normalizer = normalizer > 1e-12
    output[valid_normalizer, 0] /= normalizer[valid_normalizer]
    output = output[:frames]

    original = values.copy()
    fade = max(1, int(round(0.005 * sample_rate)))
    for interval in intervals:
        start = max(0, int(round(interval.start_seconds * sample_rate)))
        end = min(frames, int(round(interval.end_seconds * sample_rate)))
        if end <= start:
            continue
        fade_here = min(fade, max(1, (end - start) // 4))
        delta = output[start:end, 0] - original[start:end, 0]
        ramp_in = np.linspace(0.0, 1.0, fade_here, endpoint=False)
        ramp_out = np.linspace(1.0, 0.0, fade_here, endpoint=False)
        delta[:fade_here] *= ramp_in
        delta[-fade_here:] *= ramp_out
        original[start:end, 0] += delta
    return original


def compensate_first_violin(
    signal: np.ndarray,
    sample_rate: int,
    low_hz: float,
    high_hz: float,
    intervals: tuple[TransitionInterval, ...],
) -> CompensationResult:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    compensated = values.copy()
    for channel in range(values.shape[1]):
        compensated[:, channel] = _ola_stft(
            values[:, channel : channel + 1],
            sample_rate,
            low_hz,
            high_hz,
            intervals,
        )[:, 0]
    entries: list[dict[str, Any]] = []
    for interval in intervals:
        before_jump, before_energy, baseline = interval_band_metrics(
            values,
            sample_rate,
            low_hz,
            high_hz,
            interval.start_seconds,
            interval.end_seconds,
        )
        after_jump, after_energy, _ = interval_band_metrics(
            compensated,
            sample_rate,
            low_hz,
            high_hz,
            interval.start_seconds,
            interval.end_seconds,
        )
        start = max(0, int(round(interval.start_seconds * sample_rate)))
        end = min(values.shape[0], int(round(interval.end_seconds * sample_rate)))
        modification = (
            float(np.max(np.abs(compensated[start:end] - values[start:end])))
            if end > start
            else 0.0
        )
        entries.append(
            {
                "instrument": "第一小提琴",
                "start_seconds": interval.start_seconds,
                "end_seconds": interval.end_seconds,
                "band_low_hz": low_hz,
                "band_high_hz": high_hz,
                "before_band_energy_dbfs": round(before_energy, 9),
                "after_band_energy_dbfs": round(after_energy, 9),
                "baseline_band_energy_dbfs": round(baseline, 9),
                "before_jump_db": round(before_jump, 9),
                "after_jump_db": round(after_jump, 9),
                "jump_reduction_db": round(before_jump - after_jump, 9),
                "max_abs_modification": round(modification, 12),
            }
        )
    return CompensationResult(compensated=compensated, entries=tuple(entries))


def _forward_minimum(values: np.ndarray, window: int) -> np.ndarray:
    if window <= 1:
        return values.copy()
    result = np.empty_like(values)
    queue: list[int] = []
    next_index = 0
    length = values.shape[0]
    for index in range(length):
        limit = min(length, index + window)
        while next_index < limit:
            while queue and values[queue[-1]] >= values[next_index]:
                queue.pop()
            queue.append(next_index)
            next_index += 1
        while queue and queue[0] < index:
            queue.pop(0)
        result[index] = values[queue[0]]
    return result


def peak_limiter(
    signal: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
) -> tuple[np.ndarray, np.ndarray, int]:
    values = np.asarray(signal, dtype=np.float64)
    ceiling = 10.0 ** (ceiling_dbtp / 20.0)
    peak = np.max(np.abs(values), axis=1)
    required = np.minimum(1.0, ceiling / np.maximum(peak, 1e-12))
    lookahead = max(1, int(round(0.004 * sample_rate)))
    anticipated = _forward_minimum(required, lookahead)
    release_coefficient = math.exp(-1.0 / (0.080 * sample_rate))
    envelope = np.empty_like(anticipated)
    current = 1.0
    for index in range(anticipated.shape[0]):
        desired = anticipated[index]
        if desired < current:
            current = desired
        else:
            current = release_coefficient * current + (1.0 - release_coefficient) * desired
        envelope[index] = current
    limited = np.clip(values * envelope[:, None], -ceiling, ceiling)
    triggered = int(np.count_nonzero(envelope < 1.0 - 1e-10))
    return limited, envelope, triggered


@dataclass(frozen=True)
class Calibration:
    common_gain_db: float
    limiter_envelope: np.ndarray
    attenuation: np.ndarray
    limiter_triggered_samples: int
    loudness_lufs: float
    true_peak_dbtp: float
    ceiling_dbtp: float
    max_limiter_reduction_db: float


def calibrate_master(
    base_master: np.ndarray,
    sample_rate: int,
    target_loudness: float,
    true_peak_limit: float,
) -> Calibration:
    values = np.asarray(base_master, dtype=np.float64)
    measured = integrated_loudness_lufs(values, sample_rate)
    if not math.isfinite(measured):
        measured = -120.0
    gain_db = target_loudness - measured
    safety_ceiling = true_peak_limit - 0.20
    last: tuple[np.ndarray, np.ndarray, int, float, float, float, float] | None = None
    for _iteration in range(8):
        scalar = 10.0 ** (gain_db / 20.0)
        scaled = values * scalar
        if true_peak_dbtp(scaled, sample_rate) > safety_ceiling:
            limited, limiter_envelope, triggered = peak_limiter(
                scaled,
                sample_rate,
                safety_ceiling,
            )
        else:
            limited = scaled
            limiter_envelope = np.ones(scaled.shape[0], dtype=np.float64)
            triggered = 0
        output_loudness = integrated_loudness_lufs(limited, sample_rate)
        output_peak = true_peak_dbtp(limited, sample_rate)
        if output_peak > true_peak_limit - 0.04:
            correction = 10.0 ** ((true_peak_limit - 0.06 - output_peak) / 20.0)
            limited *= correction
            limiter_envelope *= correction
            output_peak = true_peak_dbtp(limited, sample_rate)
            output_loudness = integrated_loudness_lufs(limited, sample_rate)
        attenuation = scalar * limiter_envelope
        last = (
            limited,
            attenuation,
            triggered,
            output_loudness,
            output_peak,
            scalar,
            limiter_envelope,
        )
        if abs(output_loudness - target_loudness) <= 0.04:
            break
        gain_db += target_loudness - output_loudness
    assert last is not None
    (
        limited,
        attenuation,
        triggered,
        output_loudness,
        output_peak,
        scalar,
        limiter_envelope,
    ) = last
    del limited
    minimum_envelope = float(np.min(limiter_envelope))
    max_reduction = (
        -20.0 * math.log10(minimum_envelope) if minimum_envelope > 0.0 else 240.0
    )
    if abs(max_reduction) < 1e-12:
        max_reduction = 0.0
    return Calibration(
        common_gain_db=float(20.0 * math.log10(max(scalar, 1e-30))),
        limiter_envelope=limiter_envelope,
        limiter_triggered_samples=triggered,
        loudness_lufs=float(output_loudness),
        true_peak_dbtp=float(output_peak),
        ceiling_dbtp=float(safety_ceiling),
        attenuation=attenuation,
        max_limiter_reduction_db=float(max_reduction),
    )


def contribution_energy_shares(
    contributions: dict[str, np.ndarray],
    attenuation: np.ndarray,
) -> dict[str, float]:
    energies: dict[str, float] = {}
    for instrument, signal in contributions.items():
        values = np.asarray(signal, dtype=np.float64)
        weighted = values * attenuation[:, None]
        energies[instrument] = float(np.sum(weighted * weighted))
    total = sum(energies.values())
    if total <= 0.0:
        return {instrument: 0.0 for instrument in energies}
    return {instrument: energy / total for instrument, energy in energies.items()}
