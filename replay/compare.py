"""基线逐帧比对与分叉诊断。"""

from __future__ import annotations

from .canon import canonical_bytes
from .errors import BaselineVersionError
from .models import BASELINE_VERSION, STATE_VERSION


def _first_diff(local, base, path):
    """递归定位第一个不一致的字段路径。"""
    if isinstance(local, dict) and isinstance(base, dict):
        for key in sorted(set(local) | set(base)):
            if key not in local:
                return (f"{path}.{key}", "<missing>", base[key])
            if key not in base:
                return (f"{path}.{key}", local[key], "<missing>")
            if canonical_bytes(local[key]) != canonical_bytes(base[key]):
                return _first_diff(local[key], base[key], f"{path}.{key}")
        return None
    if isinstance(local, list) and isinstance(base, list):
        for i in range(max(len(local), len(base))):
            if i >= len(local):
                return (f"{path}[{i}]", "<missing>", base[i])
            if i >= len(base):
                return (f"{path}[{i}]", local[i], "<missing>")
            if canonical_bytes(local[i]) != canonical_bytes(base[i]):
                return _first_diff(local[i], base[i], f"{path}[{i}]")
        return None
    return (path, local, base)


COMPARE_FIELDS = ("state_hash", "chain_hash", "events", "quota_used",
                  "buffer_watermark", "applied")


def compare_trace(trace: list[dict], baseline: dict) -> dict:
    """逐帧比对本地轨迹与权威基线。

    返回 {"status": "match", ...} 或首个分叉点的完整诊断；
    基线版本不匹配时抛 E_BASELINE_VERSION 并终止比对。
    """
    if baseline.get("baseline_version") != BASELINE_VERSION:
        raise BaselineVersionError(BASELINE_VERSION,
                                   baseline.get("baseline_version"))
    if baseline.get("state_version") != STATE_VERSION:
        raise BaselineVersionError(STATE_VERSION,
                                   baseline.get("state_version"))
    base_frames = baseline["frames"]
    total = max(len(trace), len(base_frames))
    for i in range(total):
        if i >= len(trace):
            return {"status": "diverged", "frame": i,
                    "field": "frames", "local": "<missing>",
                    "baseline": "present"}
        if i >= len(base_frames):
            return {"status": "diverged", "frame": i,
                    "field": "frames", "local": "present",
                    "baseline": "<missing>"}
        local_frame, base_frame = trace[i], base_frames[i]
        for field_name in COMPARE_FIELDS:
            if canonical_bytes(local_frame.get(field_name)) != \
                    canonical_bytes(base_frame.get(field_name)):
                diff = _first_diff(local_frame.get(field_name),
                                   base_frame.get(field_name), field_name)
                return {
                    "status": "diverged",
                    "frame": i,
                    "field": diff[0],
                    "local": diff[1],
                    "baseline": diff[2],
                    "applied": local_frame.get("applied"),
                    "buffer_watermark": local_frame.get("buffer_watermark"),
                }
    return {"status": "match", "frames_compared": total}
