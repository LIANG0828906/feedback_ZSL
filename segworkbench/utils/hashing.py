"""确定性内容哈希工具。

统一使用 SHA-256，对 ndarray 采用 C 连续内存布局后再摘要，保证不同进程
与不同执行路径下同一数组得到相同哈希；JSON 结构使用排序键、固定分隔符。
"""
from __future__ import annotations

import hashlib
import json
from typing import Any

import numpy as np


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_array(array: np.ndarray) -> str:
    """对数组的 dtype、形状与原始字节做确定性摘要。"""
    array = np.ascontiguousarray(array)
    digest = hashlib.sha256()
    header = f"{array.dtype.str}|{','.join(str(dim) for dim in array.shape)}|".encode("ascii")
    digest.update(header)
    digest.update(array.tobytes(order="C"))
    return digest.hexdigest()


def sha256_json(payload: Any) -> str:
    """对 JSON 可序列化结构做稳定摘要（排序键、无空白差异）。"""
    encoded = json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def sha256_file(path: str, chunk_size: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(chunk_size), b""):
            digest.update(chunk)
    return digest.hexdigest()
