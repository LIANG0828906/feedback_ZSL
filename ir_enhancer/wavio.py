"""16-bit PCM WAV 的确定性读写（单声道或 5.1 六声道）。"""

from __future__ import annotations

import hashlib
import wave
from pathlib import Path

import numpy as np

from .config import SAMPLE_RATE
from .errors import WavFormatError

INT16_FULL_SCALE = 32768.0


def read_wav(path) -> tuple[np.ndarray, int]:
    """读取 WAV，返回 (float64 数组 [n_samples, n_channels], 采样率)。"""
    path = Path(path)
    try:
        wf = wave.open(str(path), "rb")
    except (wave.Error, OSError) as exc:
        raise WavFormatError("无法打开 WAV 文件", path=str(path), detail=str(exc))
    with wf:
        n_channels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        rate = wf.getframerate()
        n_frames = wf.getnframes()
        if sampwidth != 2:
            raise WavFormatError(
                "仅支持 16-bit PCM", path=str(path), sampwidth=sampwidth
            )
        if rate != SAMPLE_RATE:
            raise WavFormatError("采样率必须为 48000 Hz", path=str(path), rate=rate)
        if n_channels not in (1, 2, 6):
            raise WavFormatError(
                "仅支持单声道/立体声/5.1 六声道", path=str(path), channels=n_channels
            )
        raw = wf.readframes(n_frames)
    data = np.frombuffer(raw, dtype="<i2").reshape(-1, n_channels)
    return data.astype(np.float64) / INT16_FULL_SCALE, rate


def write_wav(path, data: np.ndarray, rate: int = SAMPLE_RATE) -> None:
    """确定性写出 16-bit PCM WAV（round-to-nearest-even 量化）。"""
    path = Path(path)
    data = np.asarray(data, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    clipped = np.clip(data, -1.0, 1.0 - 1.0 / INT16_FULL_SCALE)
    pcm = np.round(clipped * INT16_FULL_SCALE).astype("<i2")
    with wave.open(str(path), "wb") as wf:
        wf.setnchannels(data.shape[1])
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(pcm.tobytes())


def pcm_bytes(data: np.ndarray) -> bytes:
    """与 write_wav 完全一致的量化结果字节，用于内容哈希。"""
    clipped = np.clip(data, -1.0, 1.0 - 1.0 / INT16_FULL_SCALE)
    return np.round(clipped * INT16_FULL_SCALE).astype("<i2").tobytes()


def data_hash(data: np.ndarray) -> str:
    return hashlib.sha256(pcm_bytes(data)).hexdigest()


def file_hash(path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()
