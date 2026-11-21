"""Band smoothing and linear-phase FIR compensation design."""

from __future__ import annotations

import math
from typing import Callable, Iterable, Sequence

import numpy as np

from .errors import CalibrationError, E_FILTER_INVALID
from .models import (
    BandSpec,
    CalibrationParams,
    FilterDesign,
    SmoothingResult,
    TargetCurve,
)

_EPS = np.finfo(np.float64).eps


def smoothing_weights(centers_hz: Sequence[float], bandwidth_octaves: float) -> np.ndarray:
    centers = np.asarray(centers_hz, dtype=np.float64)
    log_centers = np.log2(np.maximum(centers, 1e-12))
    distance = log_centers[:, np.newaxis] - log_centers[np.newaxis, :]
    support = max(float(bandwidth_octaves), 1e-6)
    sigma = max(support / 2.3548200450309493, 1e-6)
    weights = np.exp(-0.5 * (distance / sigma) ** 2)
    weights[np.abs(distance) > support * 1.5] = 0.0
    totals = np.sum(weights, axis=1, keepdims=True)
    totals[totals <= 0.0] = 1.0
    return weights / totals


def affected_smoothing_bands(
    bands: Sequence[BandSpec],
    old_fraction: float,
    new_fraction: float,
    tolerance: float = 1e-12,
) -> tuple[int, ...]:
    old_weights = smoothing_weights([band.center_hz for band in bands], old_fraction)
    new_weights = smoothing_weights([band.center_hz for band in bands], new_fraction)
    changed = np.any(np.abs(old_weights - new_weights) > tolerance, axis=1)
    return tuple(int(index) for index in np.flatnonzero(changed))


def affected_target_bands(
    bands: Sequence[BandSpec],
    old_curve: TargetCurve | None,
    new_curve: TargetCurve,
    tolerance: float = 1e-12,
) -> tuple[int, ...]:
    centers = np.asarray([band.center_hz for band in bands], dtype=np.float64)
    new_values = new_curve.evaluate(centers)
    if old_curve is None:
        return tuple(range(len(bands)))
    old_values = old_curve.evaluate(centers)
    return tuple(int(index) for index in np.flatnonzero(np.abs(new_values - old_values) > tolerance))


def smooth_band_powers(
    bands: Sequence[BandSpec],
    power_db: Sequence[float],
    fraction: float,
    channel_index: int = 0,
    band_indices: Iterable[int] | None = None,
    previous: SmoothingResult | None = None,
    checkpoint: Callable[[int], None] | None = None,
) -> SmoothingResult:
    """Smooth band powers, optionally recomputing only selected bands."""

    values = np.asarray(power_db, dtype=np.float64)
    if values.shape != (len(bands),):
        raise ValueError("power_db length must match band count")
    requested = (
        tuple(range(len(bands)))
        if band_indices is None
        else tuple(sorted({int(index) for index in band_indices if 0 <= int(index) < len(bands)}))
    )
    all_weights = smoothing_weights([band.center_hz for band in bands], fraction)
    linear_power = np.power(10.0, values / 10.0)

    if previous is not None:
        if previous.channel_index != channel_index or len(previous.power_db) != len(bands):
            previous = None
        elif len(previous.weights) != len(bands):
            previous = None

    smoothed_linear = linear_power.copy()
    weights = all_weights.copy()
    if previous is not None:
        smoothed_linear = np.power(10.0, np.asarray(previous.power_db, dtype=np.float64) / 10.0)
        weights = np.asarray(previous.weights, dtype=np.float64).copy()

    for band_index in requested:
        if checkpoint is not None:
            checkpoint(band_index)
        row = all_weights[band_index]
        weights[band_index] = row
        smoothed_linear[band_index] = float(np.dot(row, linear_power))

    smoothed_db = 10.0 * np.log10(np.maximum(smoothed_linear, 1e-24))
    return SmoothingResult(
        channel_index=channel_index,
        power_db=tuple(float(value) for value in smoothed_db),
        weights=tuple(tuple(float(value) for value in row) for row in weights),
        recomputed_bands=requested,
    )


def _invalid_filter(message: str, **details: object) -> CalibrationError:
    return CalibrationError(E_FILTER_INVALID, message, dict(details))


def validate_filter_request(params: CalibrationParams) -> None:
    try:
        if not 15 <= int(params.filter_order) <= 8191:
            raise ValueError("filter order must be in [15, 8191]")
        if int(params.filter_order) % 2 == 0:
            raise ValueError("linear-phase Type-I design requires an odd filter order")
        params.target_curve.evaluate(np.asarray([1000.0]))
        if not math.isfinite(float(params.max_boost_db)) or params.max_boost_db <= 0.0:
            raise ValueError("max_boost_db must be positive and finite")
        if not math.isfinite(float(params.max_cut_db)) or params.max_cut_db <= 0.0:
            raise ValueError("max_cut_db must be positive and finite")
    except (TypeError, ValueError) as exc:
        raise _invalid_filter("invalid filter design parameters", reason=str(exc)) from exc


def _affected_frequency_bins(
    frequencies: np.ndarray,
    bands: Sequence[BandSpec],
    affected_band_indices: Sequence[int],
) -> np.ndarray:
    mask = np.zeros(frequencies.shape, dtype=bool)
    for index in affected_band_indices:
        band = bands[index]
        mask |= (frequencies >= band.low_hz) & (frequencies < band.high_hz)
    if not np.any(mask):
        mask[0] = True
    return mask


def design_compensation_filter(
    bands: Sequence[BandSpec],
    smoothed_power_db: Sequence[float],
    sample_rate: int,
    params: CalibrationParams,
    channel_index: int = 0,
    previous: FilterDesign | None = None,
    affected_band_indices: Sequence[int] | None = None,
) -> FilterDesign:
    """Design a linear-phase FIR filter over the band response grid."""

    validate_filter_request(params)
    order = int(params.filter_order)
    band_count = len(bands)
    source_db = np.asarray(smoothed_power_db, dtype=np.float64)
    if source_db.shape != (band_count,):
        raise _invalid_filter("smoothed response length does not match bands")
    centers = np.asarray([band.center_hz for band in bands], dtype=np.float64)
    target_db = params.target_curve.evaluate(centers)
    desired = np.clip(target_db - source_db, -float(params.max_cut_db), float(params.max_boost_db))

    if previous is not None and len(previous.desired_gain_db) == band_count:
        desired = np.asarray(previous.desired_gain_db, dtype=np.float64).copy()
        if affected_band_indices is None:
            affected = tuple(range(band_count))
        else:
            affected = tuple(sorted({int(index) for index in affected_band_indices if 0 <= int(index) < band_count}))
        desired[list(affected)] = np.clip(
            target_db[list(affected)] - source_db[list(affected)],
            -float(params.max_cut_db),
            float(params.max_boost_db),
        )
    else:
        affected = tuple(range(band_count))

    frequencies = np.fft.rfftfreq(order, d=1.0 / sample_rate)
    log_frequencies = np.log2(np.maximum(frequencies, centers[0] * 0.5))
    desired_grid = np.interp(
        log_frequencies,
        np.log2(centers),
        desired,
        left=desired[0],
        right=desired[-1],
    )
    magnitude = np.power(10.0, desired_grid / 20.0)
    delay = (order - 1) / 2.0
    coefficients = np.fft.irfft(magnitude, n=order)

    # irfft yields a zero-phase (non-causal) impulse; centering it creates an
    # exact linear-phase FIR with group delay (order - 1) / 2 samples.
    coefficients = np.roll(coefficients, int(round(delay)))
    if order % 2 == 1:
        middle = order // 2
        mirror = coefficients[:middle][::-1]
        coefficients[middle + 1 :] = mirror
    coefficients = np.asarray(coefficients, dtype=np.float64)

    dense_size = max(8192, 1 << int(math.ceil(math.log2(max(order * 16, 8192)))))
    dense_frequencies = np.fft.rfftfreq(dense_size, d=1.0 / sample_rate)
    dense_response = np.abs(np.fft.rfft(coefficients, n=dense_size))
    valid = (dense_frequencies >= bands[0].low_hz) & (dense_frequencies <= bands[-1].high_hz)
    dense_ideal_db = np.interp(
        np.log2(np.maximum(dense_frequencies[valid], centers[0] * 0.5)),
        np.log2(centers),
        desired,
        left=desired[0],
        right=desired[-1],
    )
    dense_ideal = np.power(10.0, dense_ideal_db / 20.0)
    if dense_ideal.size:
        mismatch = np.abs(dense_response[valid] / np.maximum(dense_ideal, 1e-9) - 1.0)
        truncation_rel = float(min(1.0, np.percentile(mismatch, 95)))
    else:
        truncation_rel = 0.0
    tail_count = max(1, order // 40)
    total_abs = float(np.sum(np.abs(coefficients)))
    tail_abs = float(np.sum(np.abs(coefficients[:tail_count])) + np.sum(np.abs(coefficients[-tail_count:])))
    if total_abs > 0.0:
        truncation_rel = min(1.0, truncation_rel + 0.25 * tail_abs / total_abs)

    affected_bins = _affected_frequency_bins(frequencies, bands, affected)
    recomputed_bins = int(np.count_nonzero(affected_bins))
    reused_bins = int(frequencies.size - recomputed_bins)
    return FilterDesign(
        channel_index=int(channel_index),
        order=order,
        coefficients=tuple(float(value) for value in coefficients),
        group_delay_samples=float(delay),
        group_delay_ms=float(delay * 1000.0 / sample_rate),
        desired_gain_db=tuple(float(value) for value in desired),
        truncation_rel=float(truncation_rel),
        recomputed_bands=tuple(affected),
        reused_frequency_bins=reused_bins,
        recomputed_frequency_bins=recomputed_bins,
        valid=True,
    )


def filter_response(
    design: FilterDesign,
    frequencies: np.ndarray,
    sample_rate: int,
) -> np.ndarray:
    coefficients = np.asarray(design.coefficients, dtype=np.float64)
    response = np.fft.rfft(coefficients, n=32768)
    grid = np.fft.rfftfreq(32768, d=1.0 / sample_rate)
    magnitude = np.abs(response)
    return np.interp(frequencies, grid, magnitude)
