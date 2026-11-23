"""本地音频文件 IO 与内容哈希。

仅使用 scipy.io.wavfile 读写本地 WAV，无外部音频服务/网络依赖。
试听与导出全部基于本地文件路径。
"""
from __future__ import annotations

import hashlib
import os
import warnings

import numpy as np
from scipy.io import wavfile
from scipy.io.wavfile import WavFileWarning

from ..core.errors import DecodeError


def content_hash(x: np.ndarray) -> str:
    """对样本字节做确定性 SHA-256（float64 原始字节）。"""
    return hashlib.sha256(np.ascontiguousarray(x, dtype=np.float64).tobytes()
                          ).hexdigest()


def read_wav(path: str) -> tuple[int, np.ndarray]:
    """读取 WAV 为 float64，单声道返回 (N,)，立体声返回 (N, 2)。"""
    if not os.path.exists(path):
        raise DecodeError(f"音频文件不存在: {path}", field="path")
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", WavFileWarning)
            fs, raw = wavfile.read(path)
    except Exception as exc:  # scipy 抛 ValueError 等
        raise DecodeError(f"WAV 解码失败: {path} ({exc})",
                          field="samples") from exc
    if raw.size == 0:
        raise DecodeError(f"WAV 内容为空: {path}", field="samples")
    if raw.ndim == 1:
        x = raw.astype(np.float64) / 32768.0
    elif raw.ndim == 2 and raw.shape[1] == 2:
        x = raw.astype(np.float64) / 32768.0
    else:
        raise DecodeError(f"不支持的声道数: {raw.shape}",
                          field="channels")
    if not np.isfinite(x).all():
        raise DecodeError("音频包含非有限样本", field="samples")
    return int(fs), x


def write_wav(path: str, x: np.ndarray, fs: int) -> None:
    """以 16-bit PCM WAV 写出，自动建目录。"""
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    clipped = np.clip(x, -1.0, 1.0)
    pcm = np.round(clipped * 32767.0).astype(np.int16)
    wavfile.write(path, fs, pcm)


def write_wav_float32(path: str, x: np.ndarray, fs: int) -> None:
    """32-bit float WAV，用于无损/逐样本比对导出。"""
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    wavfile.write(path, fs, x.astype(np.float32))
