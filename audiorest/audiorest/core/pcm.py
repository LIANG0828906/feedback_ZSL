"""PCM16 小端采样的确定性读写（单声道/立体声）。"""

from __future__ import annotations

from pathlib import Path

import numpy as np


def read_pcm(path: str | Path, channels: int) -> np.ndarray:
    """读取 int16 LE PCM，返回 float64 数组，形状 (samples, channels)，范围 [-1, 1)。"""
    raw = np.fromfile(str(path), dtype="<i2")
    if raw.size % channels != 0:
        raise ValueError(f"PCM 采样数 {raw.size} 不能被声道数 {channels} 整除: {path}")
    frames = raw.reshape(-1, channels).astype(np.float64) / 32768.0
    return frames


def write_pcm(path: str | Path, samples: np.ndarray) -> None:
    """把 (samples, channels) float 数组裁剪并写成 int16 LE PCM，原子替换。"""
    clipped = np.clip(samples, -1.0, 1.0)
    ints = np.round(clipped * 32768.0).astype("<i2")
    path = Path(path)
    tmp = path.with_suffix(path.suffix + ".tmp")
    ints.tofile(tmp)
    tmp.replace(path)


def write_wav(path: str | Path, samples: np.ndarray, sample_rate: int) -> None:
    """导出标准 WAV（便于桌面端试听），采样数据仍由 PCM 路径权威产出。"""
    import wave

    clipped = np.clip(samples, -1.0, 1.0)
    ints = np.round(clipped * 32768.0).astype("<i2")
    channels = ints.shape[1]
    path = Path(path)
    with wave.open(str(path), "wb") as wav:
        wav.setnchannels(channels)
        wav.setsampwidth(2)
        wav.setframerate(sample_rate)
        wav.writeframes(ints.tobytes())
