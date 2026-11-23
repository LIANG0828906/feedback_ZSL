"""多版本工程统一加载器、等价性校验与版本转换入口。"""

from __future__ import annotations
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from ..errors import WorkbenchError, raise_ver
from ..model import SUPPORTED_VERSIONS, RestoreProject
from . import v1, v2, v3
from .common import load_json, sha256_file

_PARSERS = {"1.0": v1.parse, "2.0": v2.parse, "3.0": v3.parse}
_EXPORTERS = {"1.0": v1.export, "2.0": v2.export, "3.0": v3.export}
_VERSION_KEYS = ("format_version", "formatVersion", "version")


def detect_version(data: dict[str, Any]) -> str:
    for key in _VERSION_KEYS:
        if key in data and data[key] is not None:
            return str(data[key])
    raise_ver("缺少工程版本号字段(format_version/formatVersion/version)", "format_version")
    raise  # 类型检查器兜底


def load_project(path: str | Path) -> RestoreProject:
    """读取任意受支持版本工程并映射为内部模型，保留 source_version。"""
    path = Path(path)
    data = load_json(path)
    version = detect_version(data)
    if version not in SUPPORTED_VERSIONS:
        raise_ver(
            f"未知工程版本: {version}，受支持版本: {', '.join(SUPPORTED_VERSIONS)}",
            "format_version",
            supported=list(SUPPORTED_VERSIONS),
            actual=version,
        )
    try:
        return _PARSERS[version](data)
    except WorkbenchError:
        raise
    except (TypeError, ValueError) as exc:
        raise_ver(f"工程字段类型不合法: {exc}", "<root>")


def export_project(project: RestoreProject, target_version: str) -> dict[str, Any]:
    """导出到目标版本；存在无法表达的字段时抛 E_LOSSY 并附完整损失清单。"""
    if target_version not in SUPPORTED_VERSIONS:
        raise_ver(f"未知导出目标版本: {target_version}", "format_version")
    doc, losses = _EXPORTERS[target_version](project)
    if losses:
        raise WorkbenchError(
            "E_LOSSY",
            f"降级到 v{target_version} 存在 {len(losses)} 项无法表达的字段，"
            "未静默丢弃任何修复阶段",
            losses[0]["field_path"],
            {"losses": losses, "target_version": target_version},
        )
    return doc


def export_project_lenient(project: RestoreProject, target_version: str) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """与 :func:`export_project` 相同，但返回损失清单而非抛错（GUI 预览用）。"""
    if target_version not in SUPPORTED_VERSIONS:
        raise_ver(f"未知导出目标版本: {target_version}", "format_version")
    return _EXPORTERS[target_version](project)


def write_project(project: RestoreProject, target_version: str, out_path: str | Path) -> None:
    doc = export_project(project, target_version)
    out_path = Path(out_path)
    tmp = out_path.with_suffix(out_path.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(doc, handle, ensure_ascii=False, indent=2)
    tmp.replace(out_path)


def _walk_diff(left: Any, right: Any, path: str) -> dict[str, Any] | None:
    """深度优先比较，返回首个不等价字段路径与两侧取值。"""
    if isinstance(left, dict) and isinstance(right, dict):
        keys = sorted(set(left) | set(right))
        for key in keys:
            child = f"{path}.{key}" if path else key
            if key not in left:
                return {"path": child, "left": "<missing>", "right": right[key]}
            if key not in right:
                return {"path": child, "left": left[key], "right": "<missing>"}
            found = _walk_diff(left[key], right[key], child)
            if found:
                return found
        return None
    if isinstance(left, list) and isinstance(right, list):
        for index in range(max(len(left), len(right))):
            child = f"{path}[{index}]"
            if index >= len(left):
                return {"path": child, "left": "<missing>", "right": right[index]}
            if index >= len(right):
                return {"path": child, "left": left[index], "right": "<missing>"}
            found = _walk_diff(left[index], right[index], child)
            if found:
                return found
        return None
    if left != right:
        return {"path": path or "<root>", "left": left, "right": right}
    return None


@dataclass(frozen=True)
class EquivalenceReport:
    equivalent: bool
    first_diff: dict[str, Any] | None
    audio_hash_left: str
    audio_hash_right: str


def compare_projects(
    left: RestoreProject,
    right: RestoreProject,
    *,
    left_audio_root: str | Path,
    right_audio_root: str | Path,
) -> EquivalenceReport:
    """比较两个内部模型的分帧参数、阶段配置与音频内容哈希。

    源版本标识与转换痕迹属于元数据，不参与等价性判定。
    音频按引用路径解析后比较 SHA-256 内容哈希（要求 4）。
    """
    left_hash = sha256_file(Path(left_audio_root) / left.audio.path)
    right_hash = sha256_file(Path(right_audio_root) / right.audio.path)
    report_first: dict[str, Any] | None = None
    if left_hash != right_hash:
        report_first = {
            "path": "audio.content_sha256",
            "left": left_hash,
            "right": right_hash,
        }
    model_diff = _walk_diff(left.canonical(), right.canonical(), "")
    # 音频引用的路径字符串在不同版本中允许不同命名，但内容哈希必须一致
    if model_diff is not None and model_diff["path"] == "audio.path":
        model_diff = None
    if report_first is None:
        report_first = model_diff
    return EquivalenceReport(
        equivalent=report_first is None,
        first_diff=report_first,
        audio_hash_left=left_hash,
        audio_hash_right=right_hash,
    )


def assert_equivalent(left: RestoreProject, right: RestoreProject, **roots: Any) -> EquivalenceReport:
    report = compare_projects(left, right, **roots)
    if not report.equivalent:
        diff = report.first_diff or {}
        raise WorkbenchError(
            "E_XVER",
            "跨版本内部模型不等价",
            diff.get("path"),
            {"first_diff": diff, "diffs": [diff]},
        )
    return report
