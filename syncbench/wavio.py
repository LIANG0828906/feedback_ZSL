"""Multichannel 16-bit PCM WAV reading and atomic writing."""

from __future__ import annotations

import os

import numpy as np
import soundfile as sf

from .errors import E_CONFIG, SyncError

NOMINAL_FS = 48000


def read_channel_wavs(paths, expect_fs=NOMINAL_FS):
    """Read per-channel WAV files, return (signals, fs) as float64 in [-1, 1)."""
    signals = []
    fs = None
    for path in paths:
        info = sf.info(path)
        if info.samplerate != expect_fs:
            raise SyncError(
                E_CONFIG,
                f"unexpected samplerate {info.samplerate} in {path}",
                {"path": path, "samplerate": info.samplerate},
            )
        if info.subtype not in ("PCM_16",):
            raise SyncError(
                E_CONFIG,
                f"unexpected subtype {info.subtype} in {path} (need 16-bit PCM)",
                {"path": path, "subtype": info.subtype},
            )
        data, _ = sf.read(path, dtype="float64", always_2d=True)
        if data.shape[1] != 1:
            raise SyncError(
                E_CONFIG,
                f"expected mono per-channel file, got {data.shape[1]} ch",
                {"path": path},
            )
        signals.append(np.ascontiguousarray(data[:, 0]))
        fs = info.samplerate
    return signals, fs


def write_multichannel_wav(path, matrix, fs, subtype="PCM_16"):
    """Write (channels, samples) matrix atomically; caller guarantees alignment."""
    matrix = np.asarray(matrix, dtype=np.float64)
    if matrix.ndim != 2:
        raise SyncError(E_CONFIG, "matrix must be 2-D (channels, samples)")
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    tmp = path + ".tmp"
    sf.write(tmp, matrix.T, fs, subtype=subtype, format="WAV")
    os.replace(tmp, path)
