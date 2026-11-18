from __future__ import annotations

import math

import numpy as np
from scipy.signal import resample_poly

from .manifest import DriftAnchor


def resample_to_rate(
    samples: np.ndarray,
    source_rate: int,
    output_rate: int,
) -> np.ndarray:
    """Band-limited rational resampling with the first input sample mapped to output sample zero."""
    if source_rate == output_rate:
        return np.asarray(samples, dtype=np.float64).copy()
    divisor = math.gcd(int(source_rate), int(output_rate))
    up = int(output_rate) // divisor
    down = int(source_rate) // divisor
    return resample_poly(
        np.asarray(samples, dtype=np.float64),
        up,
        down,
        axis=0,
        window=("kaiser", 5.0),
    )


def output_to_source_positions(
    output_samples: np.ndarray,
    anchors: tuple[DriftAnchor, ...] | list[DriftAnchor],
    source_rate: int,
    output_rate: int,
) -> np.ndarray:
    """Map master-timeline samples to fractional source-sample coordinates."""
    query = np.asarray(output_samples, dtype=np.float64)
    nominal_slope = float(source_rate) / float(output_rate)
    if not anchors:
        return query * nominal_slope

    anchor_output = np.asarray([anchor.output_sample for anchor in anchors], dtype=np.float64)
    anchor_source = np.asarray([anchor.source_sample for anchor in anchors], dtype=np.float64)
    if len(anchors) == 1:
        return anchor_source[0] + (query - anchor_output[0]) * nominal_slope

    slopes = np.diff(anchor_source) / np.diff(anchor_output)
    result = np.empty_like(query)
    before = query <= anchor_output[0]
    after = query >= anchor_output[-1]
    middle = ~(before | after)
    result[before] = anchor_source[0] + (query[before] - anchor_output[0]) * slopes[0]
    result[after] = anchor_source[-1] + (query[after] - anchor_output[-1]) * slopes[-1]
    if np.any(middle):
        result[middle] = np.interp(query[middle], anchor_output, anchor_source)
    return result


def source_to_output_sample(
    source_sample: float,
    anchors: tuple[DriftAnchor, ...] | list[DriftAnchor],
    source_rate: int,
    output_rate: int,
) -> float:
    """Invert the monotonic output-to-source clock map at one source position."""
    if not anchors:
        return float(source_sample) * float(output_rate) / float(source_rate)

    anchor_output = np.asarray([anchor.output_sample for anchor in anchors], dtype=np.float64)
    anchor_source = np.asarray([anchor.source_sample for anchor in anchors], dtype=np.float64)
    nominal_slope = float(source_rate) / float(output_rate)
    if len(anchors) == 1:
        return float(
            anchor_output[0] + (source_sample - anchor_source[0]) / nominal_slope
        )

    slopes = np.diff(anchor_source) / np.diff(anchor_output)
    if source_sample <= anchor_source[0]:
        return float(anchor_output[0] + (source_sample - anchor_source[0]) / slopes[0])
    if source_sample >= anchor_source[-1]:
        return float(anchor_output[-1] + (source_sample - anchor_source[-1]) / slopes[-1])

    index = int(np.searchsorted(anchor_source, source_sample, side="right") - 1)
    index = min(max(index, 0), len(anchor_source) - 2)
    fraction = (source_sample - anchor_source[index]) / (
        anchor_source[index + 1] - anchor_source[index]
    )
    return float(anchor_output[index] + fraction * (anchor_output[index + 1] - anchor_output[index]))


def warp_to_output_timeline(
    resampled: np.ndarray,
    anchors: tuple[DriftAnchor, ...] | list[DriftAnchor],
    source_rate: int,
    output_rate: int,
    frames: int,
) -> np.ndarray:
    """Apply the piecewise clock map with streaming cubic reconstruction."""
    data = np.asarray(resampled, dtype=np.float64)
    if not anchors:
        return fit_length(data, frames)
    if data.shape[0] == 0:
        return np.zeros((frames, data.shape[1]), dtype=np.float64)

    result = np.zeros((frames, data.shape[1]), dtype=np.float64)
    chunk_size = 262_144
    frame_axis = np.arange(data.shape[0], dtype=np.float64)
    for chunk_start in range(0, frames, chunk_size):
        chunk_end = min(frames, chunk_start + chunk_size)
        output_index = np.arange(chunk_start, chunk_end, dtype=np.float64)
        source_position = output_to_source_positions(
            output_index, anchors, source_rate, output_rate
        )
        nominal_position = source_position * float(output_rate) / float(source_rate)
        valid = (
            np.isfinite(nominal_position)
            & (nominal_position >= 0.0)
            & (nominal_position <= data.shape[0] - 1)
        )
        if not np.any(valid):
            continue
        positions = nominal_position[valid]
        if data.shape[0] < 4:
            for channel in range(data.shape[1]):
                result[chunk_start:chunk_end, channel][valid] = np.interp(
                    positions, frame_axis, data[:, channel]
                )
            continue

        base = np.floor(positions).astype(np.int64)
        fraction = positions - base
        index0 = np.clip(base - 1, 0, data.shape[0] - 1)
        index1 = np.clip(base, 0, data.shape[0] - 1)
        index2 = np.clip(base + 1, 0, data.shape[0] - 1)
        index3 = np.clip(base + 2, 0, data.shape[0] - 1)
        p0, p1, p2, p3 = data[index0], data[index1], data[index2], data[index3]
        t = fraction[:, None]
        interpolated = 0.5 * (
            2.0 * p1
            + (-p0 + p2) * t
            + (2.0 * p0 - 5.0 * p1 + 4.0 * p2 - p3) * t * t
            + (-p0 + 3.0 * p1 - 3.0 * p2 + p3) * t * t * t
        )
        destination = result[chunk_start:chunk_end]
        destination[valid] = interpolated
    return result


def fit_length(samples: np.ndarray, frames: int) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    if data.shape[0] == frames:
        return data.copy()
    if data.shape[0] > frames:
        return data[:frames].copy()
    result = np.zeros((frames, data.shape[1]), dtype=np.float64)
    result[: data.shape[0]] = data
    return result


def apply_integer_shift(samples: np.ndarray, shift: int, frames: int) -> np.ndarray:
    data = fit_length(samples, frames) if abs(shift) >= frames else samples
    result = np.zeros((frames, data.shape[1]), dtype=np.float64)
    if shift == 0:
        result[: min(frames, data.shape[0])] = data[: min(frames, data.shape[0])]
    elif shift > 0:
        count = max(0, min(frames - shift, data.shape[0]))
        if count:
            result[shift : shift + count] = data[:count]
    else:
        left = -shift
        count = max(0, min(frames, data.shape[0] - left))
        if count:
            result[:count] = data[left : left + count]
    return result


def map_channels(samples: np.ndarray, output_channels: int) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2:
        raise ValueError("samples must be two-dimensional")
    input_channels = data.shape[1]
    if input_channels == output_channels:
        return data
    if input_channels == 1:
        return np.repeat(data, output_channels, axis=1)
    if input_channels == 2 and output_channels == 1:
        return data.mean(axis=1, keepdims=True)
    raise ValueError(f"unsupported channel mapping from {input_channels} to {output_channels}")
