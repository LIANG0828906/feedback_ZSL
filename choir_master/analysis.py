"""Breakpoint detection and band-limited timbre compensation."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np
from scipy.ndimage import median_filter
from scipy.signal import butter, sosfiltfilt, stft


FRAME_SIZE = 2048
HOP_SIZE = 240  # 5 ms at 48 kHz


@dataclass(frozen=True)
class BreakpointInterval:
    voice: str
    start_s: float
    end_s: float
    band_low_hz: float
    band_high_hz: float
    baseline_band_energy_db: float
    interval_band_energy_db: float
    band_energy_drop_db: float
    band_ratio_drop_db: float

    def as_dict(self) -> dict:
        return {
            "voice": self.voice,
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "band_hz": [round(self.band_low_hz, 6), round(self.band_high_hz, 6)],
            "baseline_band_energy_db": round(self.baseline_band_energy_db, 6),
            "interval_band_energy_db": round(self.interval_band_energy_db, 6),
            "band_energy_drop_db": round(self.band_energy_drop_db, 6),
            "band_ratio_drop_db": round(self.band_ratio_drop_db, 6),
        }


@dataclass(frozen=True)
class CompensationStats:
    voice: str
    start_s: float
    end_s: float
    band_low_hz: float
    band_high_hz: float
    before_band_energy_db: float
    after_band_energy_db: float
    baseline_band_energy_db: float
    before_drop_db: float
    after_drop_db: float
    improvement_db: float
    applied_gain_db: float
    max_modification_amplitude: float

    def as_dict(self) -> dict:
        return {
            "voice": self.voice,
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "band_hz": [round(self.band_low_hz, 6), round(self.band_high_hz, 6)],
            "before_band_energy_db": round(self.before_band_energy_db, 6),
            "after_band_energy_db": round(self.after_band_energy_db, 6),
            "baseline_band_energy_db": round(self.baseline_band_energy_db, 6),
            "before_drop_db": round(self.before_drop_db, 6),
            "after_drop_db": round(self.after_drop_db, 6),
            "improvement_db": round(self.improvement_db, 6),
            "applied_gain_db": round(self.applied_gain_db, 6),
            "max_modification_amplitude": round(self.max_modification_amplitude, 12),
        }


@dataclass
class FrameBandData:
    times_s: np.ndarray
    total_power: np.ndarray
    band_power: np.ndarray
    active: np.ndarray
    ratio_db: np.ndarray


def _db(value: np.ndarray | float) -> np.ndarray | float:
    return 10.0 * np.log10(np.maximum(value, 1e-30))


def frame_band_data(
    signals: list[np.ndarray],
    sample_rate: int,
    band_low_hz: float,
    band_high_hz: float,
) -> FrameBandData:
    if not signals:
        empty = np.zeros(0, dtype=np.float64)
        return FrameBandData(empty, empty, empty, np.zeros(0, dtype=bool), empty)
    length = min(signal.shape[0] for signal in signals)
    if length < FRAME_SIZE:
        raise ValueError("audio is shorter than one analysis frame")
    frequencies = np.fft.rfftfreq(FRAME_SIZE, 1.0 / sample_rate)
    band_mask = (frequencies >= band_low_hz) & (frequencies <= band_high_hz)
    if not np.any(band_mask):
        raise ValueError("breakpoint band contains no FFT bin")

    total_power: np.ndarray | None = None
    band_power: np.ndarray | None = None
    for signal in signals:
        array = np.asarray(signal, dtype=np.float64)
        if array.ndim == 1:
            array = array[:, None]
        for channel in range(array.shape[1]):
            _, _, spectrum = stft(
                array[:length, channel],
                fs=sample_rate,
                window="hann",
                nperseg=FRAME_SIZE,
                noverlap=FRAME_SIZE - HOP_SIZE,
                boundary=None,
                padded=False,
            )
            power = np.abs(spectrum) ** 2
            frame_total = np.sum(power, axis=0)
            frame_band = np.sum(power[band_mask, :], axis=0)
            if total_power is None:
                total_power = frame_total
                band_power = frame_band
            else:
                total_power += frame_total
                band_power += frame_band
    assert total_power is not None and band_power is not None
    frames = total_power.size
    times = (np.arange(frames) * HOP_SIZE + FRAME_SIZE / 2.0) / sample_rate
    median_total = float(np.median(total_power)) if frames else 0.0
    active_threshold = max(median_total * 0.10, 1e-14)
    active = total_power > active_threshold
    ratio_db = _db((band_power + 1e-30) / (total_power + 1e-30))
    if frames >= 3:
        ratio_db = median_filter(ratio_db, size=3, mode="nearest")
    return FrameBandData(
        times_s=times,
        total_power=total_power,
        band_power=band_power,
        active=active,
        ratio_db=np.asarray(ratio_db, dtype=np.float64),
    )


def _contiguous_segments(mask: np.ndarray, close_gap: int) -> list[tuple[int, int]]:
    indices = np.flatnonzero(mask)
    if indices.size == 0:
        return []
    segments: list[tuple[int, int]] = []
    first = int(indices[0])
    last = int(indices[0])
    for index in indices[1:]:
        current = int(index)
        if current - last <= close_gap + 1:
            last = current
        else:
            segments.append((first, last))
            first = current
            last = current
    segments.append((first, last))
    return segments


def detect_voice_breakpoints(
    voice: str,
    signals: list[np.ndarray],
    sample_rate: int,
    band_low_hz: float,
    band_high_hz: float,
    *,
    threshold_db: float = 4.0,
    minimum_duration_s: float = 0.12,
) -> tuple[list[BreakpointInterval], FrameBandData]:
    data = frame_band_data(signals, sample_rate, band_low_hz, band_high_hz)
    if not np.any(data.active):
        return [], data
    baseline_ratio = float(np.median(data.ratio_db[data.active]))
    candidate = data.active & (data.ratio_db < baseline_ratio - threshold_db)
    candidate = median_filter(candidate.astype(np.uint8), size=3, mode="nearest") > 0
    segments = _contiguous_segments(candidate, close_gap=2)
    minimum_frames = max(1, int(math.ceil(minimum_duration_s * sample_rate / HOP_SIZE)))
    accepted: list[tuple[int, int]] = []
    for first, last in segments:
        # Follow the 1.5 dB shoulder so interval boundaries track the injected
        # transition rather than only the deep centre of the notch.
        while first > 0 and data.active[first - 1] and data.ratio_db[first - 1] < baseline_ratio - 1.5:
            first -= 1
        while (
            last + 1 < data.active.size
            and data.active[last + 1]
            and data.ratio_db[last + 1] < baseline_ratio - 1.5
        ):
            last += 1
        if last - first + 1 >= minimum_frames:
            accepted.append((first, last))

    candidate_after_refine = np.zeros_like(candidate)
    for first, last in accepted:
        candidate_after_refine[first : last + 1] = True
    baseline_mask = data.active & ~candidate_after_refine
    if not np.any(baseline_mask):
        baseline_mask = data.active
    baseline_band_power = float(np.median(data.band_power[baseline_mask]))
    baseline_band_db = float(_db(baseline_band_power))

    intervals: list[BreakpointInterval] = []
    for first, last in accepted:
        segment_band_power = float(np.median(data.band_power[first : last + 1]))
        interval_band_db = float(_db(segment_band_power))
        interval_ratio = float(np.median(data.ratio_db[first : last + 1]))
        start = max(0.0, float(data.times_s[first] - HOP_SIZE / (2.0 * sample_rate)))
        end = float(data.times_s[last] + HOP_SIZE / (2.0 * sample_rate))
        intervals.append(
            BreakpointInterval(
                voice=voice,
                start_s=start,
                end_s=end,
                band_low_hz=band_low_hz,
                band_high_hz=band_high_hz,
                baseline_band_energy_db=baseline_band_db,
                interval_band_energy_db=interval_band_db,
                band_energy_drop_db=baseline_band_db - interval_band_db,
                band_ratio_drop_db=baseline_ratio - interval_ratio,
            )
        )
    return intervals, data


def detect_all_breakpoints(
    voice_signals: dict[str, list[np.ndarray]],
    sample_rate: int,
    band_by_voice: dict[str, tuple[float, float]],
) -> tuple[dict[str, list[BreakpointInterval]], dict[str, FrameBandData]]:
    intervals: dict[str, list[BreakpointInterval]] = {}
    frames: dict[str, FrameBandData] = {}
    for voice in ("女高", "女低", "男高", "男低"):
        low, high = band_by_voice[voice]
        found, data = detect_voice_breakpoints(
            voice,
            voice_signals[voice],
            sample_rate,
            low,
            high,
        )
        intervals[voice] = found
        frames[voice] = data
    return intervals, frames


def _band_component(signals: np.ndarray, sample_rate: int, low: float, high: float) -> np.ndarray:
    nyquist = sample_rate / 2.0
    high = min(high, nyquist * 0.98)
    sos = butter(4, [low / nyquist, high / nyquist], btype="bandpass", output="sos")
    return sosfiltfilt(sos, signals, axis=0)


def _raised_cosine_mask(length: int, sample_rate: int, edge_seconds: float = 0.025) -> np.ndarray:
    mask = np.ones(length, dtype=np.float64)
    edge = min(int(round(edge_seconds * sample_rate)), length // 2)
    if edge <= 1:
        return mask
    ramp = 0.5 - 0.5 * np.cos(np.pi * np.arange(edge, dtype=np.float64) / (edge - 1))
    mask[:edge] = ramp
    mask[-edge:] = ramp[::-1]
    return mask


def compensate_intervals(
    voice: str,
    signals: np.ndarray,
    sample_rate: int,
    band_low_hz: float,
    band_high_hz: float,
    intervals: list[BreakpointInterval],
) -> tuple[np.ndarray, list[CompensationStats]]:
    """Compensate only samples inside detected intervals.

    The operation is ``x + (gain_envelope - 1) * bandpass(x)`` with a gain of
    exactly one outside every interval, so untouched samples remain bit exact.
    """
    if signals.ndim == 1:
        signals = signals[:, None]
    original = np.asarray(signals, dtype=np.float64)
    if not intervals:
        return original.copy(), []
    band_signal = _band_component(original, sample_rate, band_low_hz, band_high_hz)
    output = original.copy()
    data = frame_band_data(
        [original[:, channel] for channel in range(original.shape[1])],
        sample_rate,
        band_low_hz,
        band_high_hz,
    )
    interval_mask = np.zeros(original.shape[0], dtype=bool)
    for interval in intervals:
        start = max(0, int(round(interval.start_s * sample_rate)))
        end = min(original.shape[0], int(round(interval.end_s * sample_rate)))
        interval_mask[start:end] = True
    baseline_mask = data.active & ~interval_mask[
        np.minimum(
            (data.times_s * sample_rate).astype(int),
            max(0, original.shape[0] - 1),
        )
    ]
    if not np.any(baseline_mask):
        baseline_mask = data.active
    baseline_band_power = float(np.median(data.band_power[baseline_mask]))
    baseline_band_db = float(_db(baseline_band_power))

    stats: list[CompensationStats] = []
    for interval in intervals:
        start = max(0, int(round(interval.start_s * sample_rate)))
        end = min(original.shape[0], int(round(interval.end_s * sample_rate)))
        if end <= start:
            continue
        center_lo = start + (end - start) // 4
        center_hi = end - (end - start) // 4
        center_frames = (
            (data.times_s >= center_lo / sample_rate)
            & (data.times_s <= center_hi / sample_rate)
            & data.active
        )
        if not np.any(center_frames):
            center_frames = (data.times_s >= start / sample_rate) & (
                data.times_s <= end / sample_rate
            )
        before_power = float(np.median(data.band_power[center_frames]))
        before_db = float(_db(before_power))
        desired_gain = math.sqrt(max(baseline_band_power, 1e-30) / max(before_power, 1e-30))
        desired_gain = min(desired_gain, 20.0)
        gain_db = 20.0 * math.log10(max(desired_gain, 1e-12))
        mask = _raised_cosine_mask(end - start, sample_rate)
        gain_envelope = 1.0 + (desired_gain - 1.0) * mask
        delta = (gain_envelope[:, None] - 1.0) * band_signal[start:end]
        output[start:end] += delta

        processed_frames = (
            (data.times_s >= center_lo / sample_rate)
            & (data.times_s <= center_hi / sample_rate)
            & data.active
        )
        # Re-measure only the processed interval with the same STFT geometry.
        processed_frame_data = frame_band_data(
            [output[:, channel] for channel in range(output.shape[1])],
            sample_rate,
            band_low_hz,
            band_high_hz,
        )
        after_power = float(np.median(processed_frame_data.band_power[processed_frames]))
        after_db = float(_db(after_power))
        before_drop = abs(baseline_band_db - before_db)
        after_drop = abs(baseline_band_db - after_db)
        stats.append(
            CompensationStats(
                voice=voice,
                start_s=interval.start_s,
                end_s=interval.end_s,
                band_low_hz=band_low_hz,
                band_high_hz=band_high_hz,
                before_band_energy_db=before_db,
                after_band_energy_db=after_db,
                baseline_band_energy_db=baseline_band_db,
                before_drop_db=before_drop,
                after_drop_db=after_drop,
                improvement_db=before_drop - after_drop,
                applied_gain_db=gain_db,
                max_modification_amplitude=float(np.max(np.abs(delta))),
            )
        )
    return output, stats

