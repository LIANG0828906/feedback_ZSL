from __future__ import annotations

import hashlib
from pathlib import Path

import numpy as np
from scipy.io import wavfile


def read_wav(path: Path) -> tuple[np.ndarray, int, int]:
    """Read a WAV as float64 in [-1, 1].

    Returns (samples, sample_rate, channels). Samples are always two
    dimensional with shape (frames, channels).
    """

    sample_rate, raw = wavfile.read(str(path), mmap=False)
    if raw.ndim == 1:
        raw = raw[:, None]
    if raw.dtype == np.int16:
        samples = raw.astype(np.float64) / 32768.0
    elif raw.dtype == np.int32:
        samples = raw.astype(np.float64) / 2147483648.0
    elif raw.dtype == np.uint8:
        samples = (raw.astype(np.float64) - 128.0) / 128.0
    elif raw.dtype in (np.float32, np.float64):
        samples = raw.astype(np.float64)
    else:
        raise ValueError(f"unsupported WAV sample type: {raw.dtype}")
    if not np.isfinite(samples).all():
        raise ValueError("WAV contains NaN or infinite samples")
    return np.ascontiguousarray(samples), int(sample_rate), int(samples.shape[1])


def write_wav_pcm24(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    """Write deterministic 24-bit PCM from float64 samples."""

    if samples.ndim != 2:
        raise ValueError("samples must be two dimensional")
    scale = float(2**31)
    quantized = np.rint(np.clip(samples, -1.0, 0.999999999) * scale)
    # 24-bit PCM in scipy is stored left-justified in int32.
    quantized = np.rint(quantized / 256.0) * 256.0
    quantized = np.clip(quantized, -(2**31), 2**31 - 256).astype(np.int32)
    wavfile.write(str(path), int(sample_rate), quantized)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

