"""轨迹日志：帧号、版本、映射命中、偏差与内容哈希。

日志采用 JSON Lines（每行一条记录），写入使用追加+flush，
并提供确定性的规范化字节（排序键、固定缩进）以便逐字节比对。
"""

from __future__ import annotations

import hashlib
import json
import os
from typing import Any, Iterable

from .codec import canonical_bytes
from .model import AnimDocument
from .validate import ValidationReport


def document_hash(doc: AnimDocument) -> str:
    return hashlib.sha256(canonical_bytes(doc)).hexdigest()


def frame_records(
    label: str,
    version: int,
    mapping,
    report: ValidationReport,
    content_hash: str,
) -> list[dict[str, Any]]:
    """把逐帧偏差单元格展开成轨迹记录。"""

    hit_count = mapping.hit_count
    source_count = mapping.source_count
    per_frame: dict[int, float] = {}
    for cell in report.cells:
        per_frame[cell.frame] = max(per_frame.get(cell.frame, 0.0), cell.angle_rad)
    for skipped in report.skipped_frames:
        per_frame.setdefault(skipped, float("nan"))

    records = []
    for frame in sorted(per_frame):
        angle = per_frame[frame]
        records.append({
            "label": label,
            "version": version,
            "frame": frame,
            "mapping_hits": hit_count,
            "mapping_total": source_count,
            "max_joint_angle_rad": None if angle != angle else round(angle, 12),
            "exceeds_tolerance": bool(angle == angle and angle > report.tolerance_rad),
            "content_hash": content_hash,
        })
    return records


def write_trajectory_log(path: str, records: Iterable[dict[str, Any]]) -> str:
    """原子写入轨迹日志（先临时文件再替换），返回日志内容哈希。"""

    lines = [json.dumps(rec, ensure_ascii=False, sort_keys=True) for rec in records]
    data = ("\n".join(lines) + "\n").encode("utf-8")
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    return hashlib.sha256(data).hexdigest()


def read_trajectory_log(path: str) -> list[dict[str, Any]]:
    with open(path, "r", encoding="utf-8") as handle:
        return [json.loads(line) for line in handle if line.strip()]
