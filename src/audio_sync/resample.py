from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .config import AlignmentConfig
from .drift import DriftModel
from .errors import E_PARA, SyncError


@dataclass(frozen=True)
class ResampleStats:
    max_abs_position: float
    min_source_index: int
    max_source_index: int
    block_samples: int


def interpolation_weights(frac: np.ndarray, half_width: int) -> np.ndarray:
    offsets = np.arange(-half_width + 1, half_width + 1, dtype=np.float64)
    distance = np.asarray(frac, dtype=np.float64)[:, None] - offsets[None, :]
    window_arg = distance / float(half_width)
    window = np.i0(8.6 * np.sqrt(np.maximum(0.0, 1.0 - np.square(window_arg)))) / np.i0(8.6)
    weights = np.sinc(distance) * window
    weights /= np.sum(weights, axis=1, keepdims=True)
    return weights


def validate_resampling(model: DriftModel, config: AlignmentConfig) -> None:
    config.validate()
    if model.rates_ppm.size and float(np.max(np.abs(model.rates_ppm))) > config.max_drift_ppm:
        raise SyncError(
            E_PARA,
            "drift rate is outside the configured interpolation range",
            channel=model.channel_id,
            field="max_drift_ppm",
        )
    if model.coefficients.size and float(np.max(np.abs(model.delay_at(np.array([0.0, model.knots_ref_samples[-1]]))))) > (
        config.max_offset_samples
    ):
        raise SyncError(
            E_PARA,
            "drift curve exceeds max_offset_samples",
            channel=model.channel_id,
            field="max_offset_samples",
        )


def align_channel_range(
    source: np.ndarray,
    model: DriftModel,
    start: int,
    stop: int,
    config: AlignmentConfig,
    *,
    segment_index: int | None = None,
    source_offset: int = 0,
) -> tuple[np.ndarray, ResampleStats]:
    """Correct a half-open reference-time range with no extrapolation or zero fill."""

    validate_resampling(model, config)
    if start < 0 or stop > int(model.knots_ref_samples[-1]) or start >= stop:
        raise SyncError(
            E_PARA,
            "invalid output alignment range",
            channel=model.channel_id,
            segment=segment_index,
            field="segment_length",
        )
    order = int(config.interpolation_order)
    half = int(config.interpolation_half_width)
    positions = (
        float(source_offset)
        + np.arange(start, stop, dtype=np.float64)
        + model.delay_at(np.arange(start, stop, dtype=np.float64))
    )
    base = np.floor(positions).astype(np.int64)
    frac = positions - base.astype(np.float64)
    min_index = int(np.min(base)) - half + 1
    max_index = int(np.max(base)) + half
    if min_index < 0 or max_index >= source.shape[0]:
        raise SyncError(
            E_PARA,
            "interpolation support exceeds channel samples; no zero padding is permitted",
            channel=model.channel_id,
            segment=segment_index,
            field="samples",
            details={
                "required": [min_index, max_index],
                "available": [0, int(source.shape[0]) - 1],
            },
        )
    taps = 2 * half
    block_samples = max(1, int(config.max_buffer_bytes) // max(1, 24 * taps))
    offsets = np.arange(-half + 1, half + 1, dtype=np.int64)
    output = np.empty(stop - start, dtype=np.float64)
    for block_start in range(0, positions.shape[0], block_samples):
        block_stop = min(block_start + block_samples, positions.shape[0])
        local_base = base[block_start:block_stop]
        local_frac = frac[block_start:block_stop]
        weights = interpolation_weights(local_frac, half)
        indices = local_base[:, None] + offsets[None, :]
        gathered = np.asarray(source[indices], dtype=np.float64)
        output[block_start:block_stop] = np.sum(weights * gathered, axis=1, dtype=np.float64)
    stats = ResampleStats(
        max_abs_position=float(np.max(np.abs(positions))),
        min_source_index=min_index,
        max_source_index=max_index,
        block_samples=block_samples,
    )
    return output, stats


def channel_content_hash(
    source: np.ndarray,
    channel_id: str,
    start: int,
    stop: int,
    source_offset: int = 0,
) -> str:
    import hashlib

    digest = hashlib.sha256()
    digest.update(channel_id.encode("utf-8"))
    digest.update(np.int64(start).tobytes())
    digest.update(np.int64(stop).tobytes())
    digest.update(np.int64(source_offset).tobytes())
    digest.update(np.ascontiguousarray(source[source_offset + start : source_offset + stop]).tobytes())
    return digest.hexdigest()

