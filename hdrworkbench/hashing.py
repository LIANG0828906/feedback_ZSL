"""确定性内容哈希工具。

所有哈希都基于规范化字节（C 连续、明确字节序），
不使用时间戳、文件大小或任何环境相关量，保证同种子逐字节复现。
"""

from __future__ import annotations

import hashlib
import json

import numpy as np


def array_hash(array: np.ndarray) -> str:
    """浮点/整数数组内容哈希。"""
    arr = np.asarray(array)
    if arr.dtype.kind == "f":
        canonical = np.ascontiguousarray(arr, dtype="<f8").tobytes()
    else:
        native = "<" if arr.dtype.byteorder in ("|", "=") else arr.dtype.byteorder
        canonical = np.ascontiguousarray(
            arr, dtype=f"{native}{arr.dtype.kind}{arr.dtype.itemsize}"
        ).tobytes()
    return hashlib.sha256(canonical).hexdigest()


def bytes_hash(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def stable_json(data) -> str:
    """带排序键、固定分隔符的规范 JSON。"""
    return json.dumps(
        data, sort_keys=True, ensure_ascii=False, separators=(",", ":")
    )


def dict_hash(data) -> str:
    return hashlib.sha256(stable_json(data).encode("utf-8")).hexdigest()

