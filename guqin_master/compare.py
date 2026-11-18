from __future__ import annotations

import hashlib
from pathlib import Path

import numpy as np

from .errors import BusinessError
from .wavio import read_wav_mono


def compare_master_files(
    left_path: Path,
    right_path: Path,
    block_size: int = 4096,
) -> None:
    left_bytes = left_path.read_bytes()
    right_bytes = right_path.read_bytes()
    if left_bytes == right_bytes:
        return
    left, left_info = read_wav_mono(left_path, "left_master")
    right, right_info = read_wav_mono(right_path, "right_master")
    if (
        left_info.sample_rate != right_info.sample_rate
        or left_info.bit_depth != right_info.bit_depth
        or left_info.channels != right_info.channels
        or left_info.frames != right_info.frames
    ):
        raise BusinessError(
            "E_RESUME_DIFF",
            "master WAV formats differ",
            "file",
            {
                "block_index": 0,
                "sample_range": [0, min(left_info.frames, right_info.frames)],
                "channel": "L/R",
                "left_format": {
                    "sample_rate": left_info.sample_rate,
                    "bit_depth": left_info.bit_depth,
                    "channels": left_info.channels,
                    "frames": left_info.frames,
                },
                "right_format": {
                    "sample_rate": right_info.sample_rate,
                    "bit_depth": right_info.bit_depth,
                    "channels": right_info.channels,
                    "frames": right_info.frames,
                },
            },
        )
    left = np.asarray(left)
    right = np.asarray(right)
    if left.ndim == 1:
        left = left[:, None]
    if right.ndim == 1:
        right = right[:, None]
    channel_names = ("L", "R") if left.shape[1] == 2 else tuple(
        f"C{index}" for index in range(left.shape[1])
    )
    for start in range(0, left.shape[0], block_size):
        stop = min(left.shape[0], start + block_size)
        for channel_index, channel_name in enumerate(channel_names):
            difference = left[start:stop, channel_index] - right[start:stop, channel_index]
            if np.any(difference != 0.0):
                first = start + int(np.flatnonzero(difference)[0])
                raise BusinessError(
                    "E_RESUME_DIFF",
                    "recovered master differs from the reference master",
                    channel_name,
                    {
                        "block_index": start // block_size,
                        "sample_range": [start, stop],
                        "channel": channel_name,
                        "first_different_sample": first,
                        "different_samples": int(np.count_nonzero(difference)),
                        "max_abs_difference": float(np.max(np.abs(difference))),
                    },
                )
    raise BusinessError(
        "E_RESUME_DIFF",
        "master WAV bytes differ although decoded samples match",
        "file",
        {
            "block_index": 0,
            "sample_range": [0, left.shape[0]],
            "channel": "FILE",
            "left_sha256": hashlib.sha256(left_bytes).hexdigest(),
            "right_sha256": hashlib.sha256(right_bytes).hexdigest(),
        },
    )
