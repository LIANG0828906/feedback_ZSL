from __future__ import annotations

import os
import struct
from pathlib import Path

import numpy as np


def write_float32_wav(
    path: str | Path,
    audio: np.ndarray,
    sample_rate: int,
    *,
    atomic: bool = True,
) -> Path:
    """Write a byte-deterministic IEEE-float 32-bit WAV without timestamp chunks."""
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    values = np.asarray(audio, dtype="<f4")
    if values.ndim == 1:
        channels = 1
        frames = int(values.shape[0])
    elif values.ndim == 2:
        frames, channels = values.shape
    else:
        raise ValueError("audio must be mono or interleaved stereo")
    if channels not in (1, 2):
        raise ValueError("only mono and stereo WAV output is supported")
    payload = np.ascontiguousarray(values).tobytes(order="C")
    data_size = len(payload)
    if data_size > 0xFFFFFFFF - 64:
        raise ValueError("WAV payload exceeds the RIFF 4 GiB limit")
    block_align = channels * 4
    byte_rate = sample_rate * block_align
    riff_size = 4 + (8 + 16) + (8 + 4) + (8 + data_size)
    header = b"".join(
        (
            b"RIFF",
            struct.pack("<I", riff_size),
            b"WAVE",
            b"fmt ",
            struct.pack("<I", 16),
            struct.pack("<HHIIHH", 3, channels, sample_rate, byte_rate, block_align, 32),
            b"fact",
            struct.pack("<I", 4),
            struct.pack("<I", frames),
            b"data",
            struct.pack("<I", data_size),
        )
    )
    temp = output.with_suffix(output.suffix + ".tmp") if atomic else output
    with temp.open("wb") as handle:
        handle.write(header)
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    if atomic:
        os.replace(temp, output)
    return output

