"""确定性序列化与内容哈希工具。

哈希只能由切片内容序列化产生，因此这里统一使用排序键的紧凑 JSON，
整数保持整数、二进制缓冲以小写十六进制表达，任何调用方都不得掺入
墙钟时间、随机数或进程标识。
"""

from __future__ import annotations

import hashlib
import json
from typing import Any


def canonical_json(obj: Any) -> bytes:
    """返回跨进程逐字节一致的紧凑 JSON（UTF-8、排序键、无多余空白）。"""
    return json.dumps(
        obj,
        sort_keys=True,
        ensure_ascii=False,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def content_hash(obj: Any) -> str:
    """对任意可 JSON 化对象计算 SHA-256（小写十六进制）。"""
    return hashlib.sha256(canonical_json(obj)).hexdigest()


def bytes_hash(data: bytes) -> str:
    """对原始字节缓冲计算 SHA-256（小写十六进制）。"""
    return hashlib.sha256(data).hexdigest()


def short_digest(digest: str, width: int = 12) -> str:
    """界面与日志用的短摘要，完整哈希始终另存。"""
    return digest[:width]
