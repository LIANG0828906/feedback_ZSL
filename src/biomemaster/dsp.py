"""Deterministic sample-domain rendering and dynamics."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .constants import SOFT_LIMIT_KNEE_DB, SOFT_LIMIT_THRESHOLD_DBFS
from .model import Biome, LayerSpec


@dataclass(frozen=True)
class LimiterStats:
    trigger_samples: int
    trigger_frames: int


def db_to_linear(db: np.ndarray | float) -> np.ndarray | float:
    return np.power(10.0, np.asarray(db, dtype=np.float64) / 20.0)


def linear_to_db(
    value: np.ndarray | float, *, floor: float = 1e-30
) -> np.ndarray | float:
    return 20.0 * np.log10(np.maximum(np.asarray(value, dtype=np.float64), floor))


def soft_limit(
    audio: np.ndarray,
    *,
    threshold_db: float = SOFT_LIMIT_THRESHOLD_DBFS,
    knee_db: float = SOFT_LIMIT_KNEE_DB,
) -> tuple[np.ndarray, LimiterStats]:
    """Apply the fixed soft-knee peak limiter.

    The knee spans ``threshold +/- knee_db / 2``. The transfer curve is unity
    below the knee, reaches ``threshold`` at the top of the knee, and remains
    at that ceiling above it. All samples therefore stay at or below
    ``10 ** (threshold_db / 20)``.
    """

    data = np.asarray(audio, dtype=np.float64)
    magnitude = np.abs(data)
    knee_start_db = threshold_db - knee_db / 2.0
    knee_start = float(db_to_linear(knee_start_db))
    trigger = magnitude > knee_start
    result = data.copy()
    if np.any(trigger):
        mag = magnitude[trigger]
        input_db = 20.0 * np.log10(mag)
        offset = input_db - knee_start_db
        output_db = np.minimum(
            knee_start_db + offset - (offset * offset) / (2.0 * knee_db),
            threshold_db,
        )
        result[trigger] = np.sign(data[trigger]) * np.power(10.0, output_db / 20.0)
    trigger_frames = int(
        np.count_nonzero(np.any(trigger, axis=1))
        if trigger.ndim == 2
        else np.count_nonzero(trigger)
    )
    return result, LimiterStats(
        trigger_samples=int(np.count_nonzero(trigger)),
        trigger_frames=trigger_frames,
    )


def true_peak_limiter(
    audio: np.ndarray,
    *,
    ceiling_db: float,
    knee_db: float = 3.0,
) -> tuple[np.ndarray, LimiterStats]:
    """Apply a smooth, sample-domain safety limiter below a linear ceiling."""

    data = np.asarray(audio, dtype=np.float64)
    ceiling = float(db_to_linear(ceiling_db))
    knee_start = ceiling * float(db_to_linear(-knee_db))
    magnitude = np.abs(data)
    trigger = magnitude > knee_start
    result = data.copy()
    if np.any(trigger):
        span = ceiling - knee_start
        shaped = knee_start + span * np.tanh((magnitude[trigger] - knee_start) / span)
        result[trigger] = np.sign(data[trigger]) * shaped
    trigger_frames = int(
        np.count_nonzero(np.any(trigger, axis=1))
        if trigger.ndim == 2
        else np.count_nonzero(trigger)
    )
    return result, LimiterStats(
        trigger_samples=int(np.count_nonzero(trigger)),
        trigger_frames=trigger_frames,
    )


def crossfade_count(
    loop_samples: int, crossfade_samples: int, total_samples: int
) -> int:
    period = loop_samples - crossfade_samples
    if period <= 0 or total_samples <= period:
        return 0
    return (total_samples - 1) // period


def render_layer_chunk(
    spec: LayerSpec,
    loop_audio: np.ndarray,
    start: int,
    count: int,
    sample_rate: int,
    crossfade_samples: int,
) -> np.ndarray:
    """Render one layer block with overlap-add equal-power loop seams."""

    if count <= 0:
        return np.zeros((0, 2), dtype=np.float64)

    loop_samples = spec.loop_samples
    period = loop_samples - crossfade_samples
    if period <= 0:
        raise ValueError("loop must be longer than the crossfade")

    end = start + count
    output = np.zeros((count, 2), dtype=np.float64)

    # Only segments whose source interval intersects this output block matter.
    first_segment = max(0, math.floor((start - loop_samples) / period) + 1)
    last_segment = (end - 1) // period
    for segment_index in range(first_segment, last_segment + 1):
        segment_start = segment_index * period
        low = max(start, segment_start)
        high = min(end, segment_start + loop_samples)
        if low >= high:
            continue
        absolute = np.arange(low, high, dtype=np.int64)
        local = absolute - segment_start
        envelope = np.ones(local.shape, dtype=np.float64)
        if segment_index > 0:
            head = local < crossfade_samples
            if np.any(head):
                envelope[head] *= np.sin(
                    0.5 * math.pi * (local[head] + 0.5) / crossfade_samples
                )
        tail = local >= period
        if np.any(tail):
            tail_offset = local[tail] - period
            envelope[tail] *= np.cos(
                0.5 * math.pi * (tail_offset + 0.5) / crossfade_samples
            )

        dynamic_db = np.interp(
            absolute.astype(np.float64) / float(sample_rate),
            spec.curve.times_seconds,
            spec.curve.gains_db,
            left=float(spec.curve.gains_db[0]),
            right=float(spec.curve.gains_db[-1]),
        )
        gain = np.power(
            10.0,
            (spec.base_gain_db + dynamic_db) / 20.0,
        )
        source = loop_audio[local]
        contribution = source * (gain * envelope)[:, None]

        output_slice = output[low - start : high - start]
        if contribution.shape[1] == 1:
            output_slice += contribution[:, :1]
        else:
            output_slice += contribution
    return output


def render_biome_chunk(
    biome: Biome,
    start: int,
    count: int,
    sample_rate: int,
    crossfade_samples: int,
) -> np.ndarray:
    """Render a biome block; layers are intentionally serial."""

    output = np.zeros((count, 2), dtype=np.float64)
    for layer in biome.layers:
        output += render_layer_chunk(
            layer.spec,
            layer.loop_audio,
            start,
            count,
            sample_rate,
            crossfade_samples,
        )
    return output


def mix_biomes_in_order(
    chunks: list[np.ndarray],
    biomes: tuple[Biome, ...],
) -> np.ndarray:
    if len(chunks) != len(biomes):
        raise ValueError("one biome chunk per biome is required")
    total = np.zeros_like(chunks[0])
    for chunk, biome in zip(chunks, biomes):
        total += chunk * biome.weight
    return total
