"""Deterministic loudness, peak-protection and boundary DSP."""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass

import numpy as np

LOUDNESS_OFFSET = -0.691
ABSOLUTE_GATE_LUFS = -70.0
LOUDNESS_BLOCK_SECONDS = 0.4
LOUDNESS_STEP_SECONDS = 0.1
DB_FLOOR = -120.0


def _k_weighting_coefficients(
    sample_rate: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    # BS.1770 K-weighting, parameterised for arbitrary sample rates.
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * f0 / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf_b = np.array(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    shelf_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )

    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = math.tan(math.pi * f0 / sample_rate)
    a0 = 1.0 + k / q + k * k
    highpass_b = np.array([1.0 / a0, -2.0 / a0, 1.0 / a0], dtype=np.float64)
    highpass_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


def _biquad_response(
    b: np.ndarray, a: np.ndarray, omega: np.ndarray
) -> np.ndarray:
    z1 = np.exp(-1j * omega)
    z2 = z1 * z1
    return (b[0] + b[1] * z1 + b[2] * z2) / (1.0 + a[1] * z1 + a[2] * z2)


def k_weight(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("audio must be frames x channels")
    frames = values.shape[0]
    if frames == 0:
        return values.copy()
    nfft = 1 << max(14, int(math.ceil(math.log2(frames + 8192))))
    omega = 2.0 * math.pi * np.arange(nfft // 2 + 1, dtype=np.float64) / nfft
    shelf_b, shelf_a, hp_b, hp_a = _k_weighting_coefficients(sample_rate)
    response = _biquad_response(shelf_b, shelf_a, omega) * _biquad_response(
        hp_b, hp_a, omega
    )
    spectrum = np.fft.rfft(values, n=nfft, axis=0)
    weighted = np.fft.irfft(spectrum * response[:, None], n=nfft, axis=0)
    return np.ascontiguousarray(weighted[:frames], dtype=np.float64)


def integrated_loudness(
    samples: np.ndarray,
    sample_rate: int,
    *,
    relative_gate_lu: float = 10.0,
    absolute_gate_lufs: float = ABSOLUTE_GATE_LUFS,
) -> float:
    """ITU-R BS.1770 style gated integrated loudness in LUFS."""

    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.size == 0:
        return DB_FLOOR
    weighted = k_weight(values, sample_rate)
    block_frames = max(1, int(round(LOUDNESS_BLOCK_SECONDS * sample_rate)))
    step_frames = max(1, int(round(LOUDNESS_STEP_SECONDS * sample_rate)))
    if weighted.shape[0] < block_frames:
        weighted = np.pad(
            weighted, ((0, block_frames - weighted.shape[0]), (0, 0))
        )
    starts = list(range(0, weighted.shape[0] - block_frames + 1, step_frames))
    if not starts:
        starts = [0]
    energies = np.empty(len(starts), dtype=np.float64)
    for index, start in enumerate(starts):
        block = weighted[start : start + block_frames]
        energies[index] = float(np.mean(np.square(block)))
    energies = np.maximum(energies, 1e-30)
    block_loudness = LOUDNESS_OFFSET + 10.0 * np.log10(energies)

    absolute_mask = block_loudness > absolute_gate_lufs
    if not np.any(absolute_mask):
        return DB_FLOOR
    absolute_mean = float(np.mean(energies[absolute_mask]))
    relative_threshold = (
        LOUDNESS_OFFSET
        + 10.0 * math.log10(max(absolute_mean, 1e-30))
        - abs(relative_gate_lu)
    )
    final_mask = block_loudness > relative_threshold
    if not np.any(final_mask):
        return DB_FLOOR
    final_mean = float(np.mean(energies[final_mask]))
    return max(
        DB_FLOOR,
        LOUDNESS_OFFSET + 10.0 * math.log10(max(final_mean, 1e-30)),
    )


def sample_peak_dbfs(samples: np.ndarray) -> float:
    values = np.asarray(samples)
    if values.size == 0:
        return DB_FLOOR
    peak = float(np.max(np.abs(values)))
    if peak <= 0.0:
        return DB_FLOOR
    return max(DB_FLOOR, 20.0 * math.log10(peak))


def _hermite_oversampled_peak(samples: np.ndarray) -> np.ndarray:
    """Return Catmull-Rom 4x oversampled channel peaks."""

    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.shape[0] == 0:
        return np.zeros(values.shape[1], dtype=np.float64)
    peaks = np.max(np.abs(values), axis=0)
    if values.shape[0] < 2:
        return peaks
    padded = np.concatenate([values[:1], values, values[-1:]], axis=0)
    p0 = padded[0:-3]
    p1 = padded[1:-2]
    p2 = padded[2:-1]
    p3 = padded[3:]
    for t in (0.25, 0.5, 0.75):
        t2 = t * t
        t3 = t2 * t
        interpolated = 0.5 * (
            2.0 * p1
            + (-p0 + p2) * t
            + (2.0 * p0 - 5.0 * p1 + 4.0 * p2 - p3) * t2
            + (-p0 + 3.0 * p1 - 3.0 * p2 + p3) * t3
        )
        peaks = np.maximum(peaks, np.max(np.abs(interpolated), axis=0))
    return peaks


def true_peak_dbtp(samples: np.ndarray) -> float:
    peaks = _hermite_oversampled_peak(samples)
    peak = float(np.max(peaks)) if peaks.size else 0.0
    if peak <= 0.0:
        return DB_FLOOR
    return max(DB_FLOOR, 20.0 * math.log10(peak))


def noise_floor_dbfs(
    samples: np.ndarray,
    sample_rate: int,
    *,
    window_seconds: float = 0.1,
    percentile: float = 10.0,
) -> float:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    if values.size == 0:
        return DB_FLOOR
    requested_window = max(1, int(round(window_seconds * sample_rate)))
    window = max(1, min(requested_window, values.shape[0] // 8))
    step = max(1, window // 2)
    if values.shape[0] <= window:
        rms = math.sqrt(max(float(np.mean(np.square(values))), 0.0))
        return max(DB_FLOOR, 20.0 * math.log10(rms)) if rms > 0 else DB_FLOOR
    levels: list[float] = []
    for start in range(0, values.shape[0] - window + 1, step):
        rms = math.sqrt(
            max(float(np.mean(np.square(values[start : start + window]))), 0.0)
        )
        levels.append(20.0 * math.log10(rms) if rms > 0.0 else DB_FLOOR)
    audible = [level for level in levels if level > DB_FLOOR + 10.0]
    selected = audible if audible else levels
    return max(DB_FLOOR, float(np.percentile(selected, percentile)))


def apply_noise_safe_gain(
    samples: np.ndarray,
    sample_rate: int,
    requested_gain_db: float,
    max_noise_rise_db: float,
) -> tuple[np.ndarray, float, float]:
    """Apply static gain to room tone and extra gain only to active regions.

    The returned static gain is bounded by ``max_noise_rise_db`` for positive
    gain.  The additional gain follows a smooth 20 ms activity envelope and is
    zero at the measured noise floor, so it does not lift quiet passages.
    """

    values = np.asarray(samples, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("audio must be frames x channels")
    requested_gain_db = float(requested_gain_db)
    noise_cap_db = float(max_noise_rise_db)
    if requested_gain_db < 0.0:
        static_db = requested_gain_db
    elif requested_gain_db >= noise_cap_db:
        # Leave a small deterministic margin for percentile/filter rounding.
        margin = 1e-4 if noise_cap_db > 0.0 else 0.0
        static_db = max(0.0, noise_cap_db - margin)
    else:
        static_db = requested_gain_db
    extra_db = float(requested_gain_db) - static_db
    if extra_db <= 1e-9 or values.shape[0] == 0:
        return values * (10.0 ** (static_db / 20.0)), static_db, 0.0

    power = np.mean(np.square(values), axis=1)
    window = max(1, int(round(0.020 * sample_rate)))
    if values.shape[0] <= window:
        envelope_power = np.full(
            values.shape[0], max(float(np.mean(power)), 1e-30)
        )
    else:
        cumulative = np.concatenate(
            [np.zeros(1, dtype=np.float64), np.cumsum(power)]
        )
        moving = (cumulative[window:] - cumulative[:-window]) / window
        envelope_power = np.empty(values.shape[0], dtype=np.float64)
        half = window // 2
        envelope_power[:half] = moving[0]
        envelope_power[half : half + moving.shape[0]] = moving
        envelope_power[half + moving.shape[0] :] = moving[-1]
    envelope_db = 10.0 * np.log10(np.maximum(envelope_power, 1e-30))
    floor = noise_floor_dbfs(values, sample_rate)
    threshold = floor + 4.0
    activity = np.clip((envelope_db - threshold) / 12.0, 0.0, 1.0)
    activity = activity * activity * (3.0 - 2.0 * activity)
    gain_db = static_db + extra_db * activity
    return values * np.power(10.0, gain_db / 20.0)[:, None], static_db, extra_db


def window_level_dbfs(samples: np.ndarray) -> float:
    values = np.asarray(samples, dtype=np.float64)
    if values.size == 0:
        return DB_FLOOR
    rms = math.sqrt(max(float(np.mean(np.square(values))), 0.0))
    return max(DB_FLOOR, 20.0 * math.log10(rms)) if rms > 0.0 else DB_FLOOR


@dataclass(slots=True, frozen=True)
class LimitResult:
    samples: np.ndarray
    protected_samples: int
    triggered_samples: int
    safety_clamp_runs: int
    maximum_safety_clamp_run: int
    peak_attenuation_db: float


def _sliding_max_abs(values: np.ndarray, width: int) -> np.ndarray:
    """Maximum of values[n:n+width], with a monotonic deque."""

    count = values.shape[0]
    if count == 0:
        return values.copy()
    width = max(1, width)
    output = np.empty(count, dtype=np.float64)
    candidates: deque[int] = deque()
    for index in range(count):
        value = float(values[index])
        while candidates and values[candidates[-1]] <= value:
            candidates.pop()
        candidates.append(index)
        if candidates[0] < index - width + 1:
            candidates.popleft()
        output[index] = values[candidates[0]]
    return output


def limit_true_peak(
    samples: np.ndarray,
    sample_rate: int,
    true_peak_dbtp_limit: float,
) -> LimitResult:
    """Look-ahead peak protection followed by an oversampled validation pass."""

    values = np.ascontiguousarray(samples, dtype=np.float64)
    frames = values.shape[0]
    if frames == 0:
        return LimitResult(values, 0, 0, 0, 0, 0.0)
    ceiling = 10.0 ** ((true_peak_dbtp_limit - 0.02) / 20.0)
    absolute = np.max(np.abs(values), axis=1)
    lookahead = max(1, int(round(0.020 * sample_rate)))
    future_peak = _sliding_max_abs(absolute[::-1], lookahead + 1)[::-1]
    desired = np.minimum(1.0, ceiling / np.maximum(future_peak, 1e-30))
    per_sample_limit = np.minimum(
        1.0, ceiling / np.maximum(absolute, 1e-30)
    )

    attack = math.exp(-1.0 / (0.004 * sample_rate))
    release = math.exp(-1.0 / (0.080 * sample_rate))
    gain = np.empty(frames, dtype=np.float64)
    previous = 1.0
    for index in range(frames):
        target = float(desired[index])
        coefficient = attack if target < previous else release
        smoothed = coefficient * previous + (1.0 - coefficient) * target
        value = min(smoothed, float(per_sample_limit[index]))
        if value > 1.0 - 1e-12:
            value = 1.0
        gain[index] = value
        previous = value

    output = values * gain[:, None]
    triggered = int(np.count_nonzero(gain < 1.0 - 1e-12))
    clamp_mask = (per_sample_limit < 1.0) & (
        gain >= per_sample_limit - 1e-15
    )
    runs = 0
    maximum_run = 0
    current_run = 0
    for active in clamp_mask:
        if active:
            current_run += 1
            maximum_run = max(maximum_run, current_run)
        else:
            if current_run:
                runs += 1
            current_run = 0
    if current_run:
        runs += 1

    # Catmull-Rom intersample peaks are validated on the actual output.  A tiny
    # whole-region trim is safer than clipping individual samples and remains
    # deterministic.
    attenuation = 1.0
    for _ in range(4):
        measured = true_peak_dbtp(output)
        if measured <= true_peak_dbtp_limit:
            break
        ratio = 10.0 ** ((true_peak_dbtp_limit - measured) / 20.0)
        ratio *= 0.999999
        output *= ratio
        attenuation *= ratio
    measured = true_peak_dbtp(output)
    if measured > true_peak_dbtp_limit:
        ratio = 10.0 ** ((true_peak_dbtp_limit - measured) / 20.0)
        output *= ratio * 0.999999
        attenuation *= ratio * 0.999999

    return LimitResult(
        samples=output,
        protected_samples=triggered,
        triggered_samples=int(np.count_nonzero(absolute > ceiling)),
        safety_clamp_runs=runs,
        maximum_safety_clamp_run=maximum_run,
        peak_attenuation_db=20.0 * math.log10(max(attenuation, 1e-30)),
    )


def boundary_levels(
    left: np.ndarray,
    right: np.ndarray,
    sample_rate: int,
) -> tuple[float, float, int]:
    window = max(1, int(round(0.1 * sample_rate)))
    window = min(window, left.shape[0], right.shape[0])
    left_level = window_level_dbfs(left[-window:])
    right_level = window_level_dbfs(right[:window])
    return left_level, right_level, window


def apply_boundary_level_match(
    chapters: list[np.ndarray],
    sample_rate: int,
    tolerance_db: float,
) -> tuple[list[np.ndarray], dict[int, float], list[dict[str, float]]]:
    """Attenuate only the louder side of a chapter seam.

    The attenuation is ramped before the seam and released after it, preserving
    every frame.  Attenuation cannot create a new true-peak violation.
    """

    result = [np.array(chapter, dtype=np.float64, copy=True) for chapter in chapters]
    trims_db: dict[int, float] = {index: 0.0 for index in range(len(result))}
    measurements: list[dict[str, float]] = []
    for index in range(len(result) - 1):
        left = result[index]
        right = result[index + 1]
        left_level, right_level, window = boundary_levels(
            left, right, sample_rate
        )
        for _ in range(2):
            delta = left_level - right_level
            if abs(delta) <= tolerance_db:
                break
            if delta > 0.0:
                target_index = index
                segment = left
                at_start = False
                other_level = right_level
                candidate_level = left_level
            else:
                target_index = index + 1
                segment = right
                at_start = True
                other_level = left_level
                candidate_level = right_level
            gain = 10.0 ** ((other_level - candidate_level) / 20.0)
            if not math.isfinite(gain):
                gain = 1.0
            gain = min(1.0, max(gain, 0.0))
            total = segment.shape[0]
            length = min(total, max(4 * window, window + 1))
            ramp_length = max(1, length - window)
            if at_start:
                factor = np.ones(length, dtype=np.float64)
                factor[:window] = gain
                if ramp_length > 0:
                    factor[window:length] = np.linspace(
                        gain, 1.0, ramp_length, endpoint=True
                    )
                segment[:length] *= factor[:, None]
            else:
                factor = np.ones(length, dtype=np.float64)
                if ramp_length > 0:
                    factor[:ramp_length] = np.linspace(
                        1.0, gain, ramp_length, endpoint=False
                    )
                factor[ramp_length:] = gain
                segment[-length:] *= factor[:, None]
            trim = 20.0 * math.log10(max(gain, 1e-30))
            trims_db[target_index] = min(
                trims_db[target_index], trim
            )
            left_level, right_level, _ = boundary_levels(
                left, right, sample_rate
            )
        measurements.append(
            {
                "left_level_dbfs": left_level,
                "right_level_dbfs": right_level,
                "delta_db": left_level - right_level,
                "window_frames": float(window),
            }
        )
    return result, trims_db, measurements
