"""格式适配器共用工具：JSON 读取、必填字段校验、内容哈希。"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from ..errors import WorkbenchError, raise_ver


def load_json(path: Path) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise WorkbenchError(
            "E_VER", f"工程文件无法解码: {exc}", "<root>", {"file": str(path)}
        ) from exc
    except OSError as exc:
        raise WorkbenchError(
            "E_VER", f"工程文件无法读取: {exc}", "<root>", {"file": str(path)}
        ) from exc
    if not isinstance(data, dict):
        raise_ver("工程根节点必须是对象", "<root>")
    return data


def require(data: dict[str, Any], path: str) -> Any:
    """按点路径读取必填字段，缺失/为 None 时抛 E_VER 并指出字段路径。"""
    node: Any = data
    for part in path.split("."):
        if not isinstance(node, dict) or part not in node or node[part] is None:
            raise_ver(f"必填字段缺失: {path}", path)
        node = node[part]
    return node


def next_pow2(value: int) -> int:
    result = 1
    while result < value:
        result <<= 1
    return result


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 16), b""):
            digest.update(block)
    return digest.hexdigest()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def trace_entry(path: str, old_value: Any, default_value: Any, source: str) -> dict[str, Any]:
    return {
        "field_path": path,
        "old_value": old_value,
        "default_value": default_value,
        "default_source": source,
    }
