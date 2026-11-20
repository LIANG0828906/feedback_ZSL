"""内容哈希与哈希链工具。

哈希全部基于原始字节，保证确定性与跨进程可复现。
"""

from __future__ import annotations

import hashlib

import numpy as np


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_array(arr: np.ndarray) -> str:
    """对 numpy 数组的原始字节做哈希（含 dtype/形状由调用方另行约定）。"""
    return hashlib.sha256(np.ascontiguousarray(arr).tobytes()).hexdigest()


def chain_hash(prev_hash: str, payload: bytes) -> str:
    """哈希链：下一个链接依赖上一个链接与本批载荷。"""
    h = hashlib.sha256()
    h.update(prev_hash.encode("ascii"))
    h.update(b"|")
    h.update(payload)
    return h.hexdigest()


GENESIS_HASH = "0" * 64
