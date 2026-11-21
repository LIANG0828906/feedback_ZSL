"""Observation-block slicing and block-level metering.

Each block records: window start/end samples, a deterministic sync marker
(a logical sequence number, never wall-clock time), a content hash, the
K-weighted energy, the short-term loudness and the 4x-oversampled true peak.

Re-slicing after a block-length / oversampling change reports the block
count delta and the maximum window offset shift.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, asdict

import numpy as np

# BS.1770 K-weighting, stage 1 (high shelf) and stage 2 (RLB high-pass),
# coefficients for 48 kHz.
K_SHELF_B = (1.53512485958697, -2.69169618940638, 1.19839281085285)
K_SHELF_A = (1.0, -1.69065929318241, 0.73248077421585)
K_RLB_B = (1.0, -2.0, 1.0)
K_RLB_A = (1.0, -1.99004745483398, 0.99007225036621)

LOUDNESS_OFFSET = -0.691  # BS.1770 absolute scale offset


def _biquad(x: np.ndarray, b, a) -> np.ndarray:
    """Direct-form-II-transposed biquad (deterministic, float64)."""
    y = np.empty_like(x)
    z1 = 0.0
    z2 = 0.0
    b0, b1, b2 = b
    _, a1, a2 = a
    for i in range(x.shape[0]):
        xi = x[i]
        yi = b0 * xi + z1
        z1 = b1 * xi - a1 * yi + z2
        z2 = b2 * xi - a2 * yi
        y[i] = yi
    return y


def k_weight(x: np.ndarray) -> np.ndarray:
    """Apply the two-stage K-weighting filter to a mono signal."""
    return _biquad(_biquad(x, K_SHELF_B, K_SHELF_A), K_RLB_B, K_RLB_A)


def oversample_true_peak(x: np.ndarray, factor: int) -> float:
    """4x-oversampled true peak of a mono signal via FFT resampling.

    Deterministic; returns the peak in dBTP (20*log10 of max |sample|).
    """
    n = x.shape[0]
    if n == 0:
        return float("-inf")
    spec = np.fft.rfft(x)
    m = spec.shape[0]
    up = np.zeros(n * factor // 2 + 1, dtype=spec.dtype)
    up[:m] = spec
    y = np.fft.irfft(up, n * factor) * factor
    peak = float(np.max(np.abs(y))) if y.size else 0.0
    if peak <= 0.0:
        return float("-inf")
    return 20.0 * np.log10(peak)


@dataclass(frozen=True)
class BlockWindow:
    index: int
    start: int
    end: int  # exclusive


def slice_blocks(n_samples: int, block_samples: int) -> list[BlockWindow]:
    """Slice ``n_samples`` into consecutive, non-overlapping observation blocks."""
    windows = []
    idx = 0
    start = 0
    while start < n_samples:
        end = min(start + block_samples, n_samples)
        windows.append(BlockWindow(index=idx, start=start, end=end))
        idx += 1
        start = end
    return windows


def reslice_diff(n_samples: int, old_block: int, new_block: int) -> dict:
    """Compare two slicings: block-count delta and max window offset shift."""
    old_w = slice_blocks(n_samples, old_block)
    new_w = slice_blocks(n_samples, new_block)
    max_shift = 0
    for ow, nw in zip(old_w, new_w):
        max_shift = max(max_shift, abs(ow.start - nw.start), abs(ow.end - nw.end))
    if len(old_w) != len(new_w):
        tail = old_w if len(old_w) > len(new_w) else new_w
        for w in tail[min(len(old_w), len(new_w)):]:
            max_shift = max(max_shift, w.end - w.start)
    return {
        "old_block_count": len(old_w),
        "new_block_count": len(new_w),
        "block_count_delta": len(new_w) - len(old_w),
        "max_window_offset_shift": max_shift,
    }


@dataclass(frozen=True)
class BlockMeasurement:
    asset_id: str
    index: int
    start: int
    end: int
    sync_marker: int          # deterministic logical sequence number
    content_hash: str         # sha256 of the block's PCM content
    k_weighted_energy: float  # weighted sum of per-channel mean squares
    short_term_lufs: float
    true_peak_dbtp: float

    def to_dict(self) -> dict:
        return asdict(self)


def measure_block(asset_id: str, window: BlockWindow, sync_marker: int,
                  pcm: np.ndarray, weights, oversample_factor: int) -> BlockMeasurement:
    """Meter one observation block.

    ``pcm`` is the block's (n, 6) float64 samples. Energy follows BS.1770:
    K-weight each channel, mean-square it, then apply channel weights.
    True peak is the max over channels of the oversampled peak.
    """
    content_hash = hashlib.sha256(pcm.tobytes()).hexdigest()
    weights = np.asarray(weights, dtype=np.float64)
    energy = 0.0
    tp = float("-inf")
    for ch in range(pcm.shape[1]):
        weighted = k_weight(pcm[:, ch])
        mean_sq = float(np.mean(weighted * weighted)) if weighted.size else 0.0
        energy += float(weights[ch]) ** 2 * mean_sq
        tp = max(tp, oversample_true_peak(pcm[:, ch], oversample_factor))
    short_term = float("-inf") if energy <= 0.0 else LOUDNESS_OFFSET + 10.0 * np.log10(energy)
    return BlockMeasurement(
        asset_id=asset_id,
        index=window.index,
        start=window.start,
        end=window.end,
        sync_marker=sync_marker,
        content_hash=content_hash,
        k_weighted_energy=energy,
        short_term_lufs=float(short_term),
        true_peak_dbtp=float(tp),
    )
