"""采样内容哈希：确定性字节编码后取 SHA-256。"""

from __future__ import annotations

import hashlib

import numpy as np


def digest_samples(samples: np.ndarray) -> str:
    """对 float64 采样做字节级确定性哈希（含形状，防止维度歧义）。"""
    array = np.ascontiguousarray(samples, dtype=np.float64)
    header = np.array(array.shape, dtype=np.int64).tobytes()
    return hashlib.sha256(header + array.tobytes()).hexdigest()


def digest_spectrum(spectrum: np.ndarray) -> str:
    array = np.ascontiguousarray(spectrum, dtype=np.float64)
    return hashlib.sha256(array.tobytes()).hexdigest()
