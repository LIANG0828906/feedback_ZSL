"""BS.1770-style 4x oversampled true-peak measurement."""

from __future__ import annotations

import math
from pathlib import Path

import numpy as np
import soundfile as sf
from scipy.signal import resample_poly

TRUE_PEAK_BLOCK = 65536
TRUE_PEAK_PAD = 128


def _db(value: float) -> float:
    return 20.0 * math.log10(value) if value > 0.0 else float("-inf")


def true_peak_dbtp_array(audio: np.ndarray) -> float:
    data = np.asarray(audio, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.shape[0] == 0:
        return float("-inf")
    peak = 0.0
    for channel in range(data.shape[1]):
        oversampled = resample_poly(data[:, channel], 4, 1)
        peak = max(peak, float(np.max(np.abs(oversampled), initial=0.0)))
    return _db(peak)


def true_peak_dbtp_wav(path: Path, *, block_frames: int = TRUE_PEAK_BLOCK) -> float:
    peak = 0.0
    with sf.SoundFile(str(path)) as handle:
        total = len(handle)
        if total == 0:
            return float("-inf")
        for start in range(0, total, block_frames):
            end = min(total, start + block_frames)
            low = max(0, start - TRUE_PEAK_PAD)
            high = min(total, end + TRUE_PEAK_PAD)
            handle.seek(low)
            segment = handle.read(high - low, dtype="float64", always_2d=True)
            for channel in range(segment.shape[1]):
                oversampled = resample_poly(segment[:, channel], 4, 1)
                left = (start - low) * 4
                right = oversampled.size - (high - end) * 4
                interior = oversampled[left:right]
                if interior.size:
                    peak = max(
                        peak,
                        float(np.max(np.abs(interior), initial=0.0)),
                    )
    return _db(peak)
