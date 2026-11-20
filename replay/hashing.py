"""确定性序列化、哈希与原子文件 IO。

所有 JSON 均以 sort_keys + 固定分隔符 + UTF-8 + LF 换行写出，
保证 Windows / Linux 上同输入逐字节一致；输出中禁止出现时间戳。
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any


def canonical_bytes(obj: Any) -> bytes:
    """跨平台稳定的规范化 JSON 字节串。"""
    return json.dumps(
        obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_json(obj: Any) -> str:
    return sha256_hex(canonical_bytes(obj))


def chain_hash(prev_hex: str, obj: Any) -> str:
    """哈希链：新哈希依赖上一环节点与当前内容。"""
    return sha256_hex(prev_hex.encode("utf-8") + b"|" + canonical_bytes(obj))


GENESIS_HASH = sha256_hex(b"replay-genesis")


def write_json_atomic(path: str | Path, obj: Any) -> Path:
    """临时文件 + os.replace 原子提交，未提交内容不会出现在正式路径。"""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    payload = json.dumps(obj, sort_keys=True, indent=2, ensure_ascii=False) + "\n"
    with open(tmp, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    return path


def read_json(path: str | Path) -> Any:
    with open(path, "r", encoding="utf-8", newline="") as handle:
        return json.load(handle)


def write_text_atomic(path: str | Path, text: str) -> Path:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with open(tmp, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    return path
