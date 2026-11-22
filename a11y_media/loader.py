"""媒体工程加载与结构校验。

校验规则:
- 字段缺失 / 引用越界 / 结构非法        -> E_SCHEMA (指出字段名)
- 参数越界(语速、权重、帧长、帧移等)    -> E_PARA   (指出字段名)
- 包络数据形状与清单声明不一致           -> E_SCHEMA
"""

from __future__ import annotations

import hashlib
import json
import os

import numpy as np

from .errors import para_error, schema_error
from .model import AssistParams, Project, SpeechSegment, Subtitle, Truth
from .schedule import validate_speed, validate_weights

REQUIRED_FIELDS = (
    "project_id", "sample_rate", "frame_len", "hop", "total_samples",
    "tolerance_samples", "segment_duration", "envelope_file",
    "segments", "subtitles", "assist", "truth",
)


def _need(obj: dict, key: str, where: str):
    if key not in obj:
        raise schema_error("字段缺失", field=f"{where}.{key}")
    return obj[key]


def _positive_int(value, field: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise para_error("必须为正整数", field=field, value=value)
    return value


def content_hash(manifest_raw: bytes, envelope: np.ndarray) -> str:
    """工程内容哈希: 清单原文 + 包络数据, 用于检查点快照版本匹配。"""
    h = hashlib.sha256()
    h.update(manifest_raw)
    h.update(envelope.tobytes())
    return h.hexdigest()


def load_project(project_dir: str) -> Project:
    manifest_path = os.path.join(project_dir, "manifest.json")
    if not os.path.isfile(manifest_path):
        raise schema_error("清单文件不存在", path=manifest_path)
    with open(manifest_path, "rb") as f:
        raw = f.read()
    try:
        doc = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise schema_error("清单不是合法 JSON", detail=str(exc))
    if not isinstance(doc, dict):
        raise schema_error("清单根节点必须为对象")
    for key in REQUIRED_FIELDS:
        _need(doc, key, "manifest")

    sample_rate = _positive_int(_need(doc, "sample_rate", "manifest"), "sample_rate")
    frame_len = _positive_int(_need(doc, "frame_len", "manifest"), "frame_len")
    hop = _positive_int(_need(doc, "hop", "manifest"), "hop")
    if hop > frame_len:
        raise para_error("hop 不得大于 frame_len", field="hop", value=hop)
    total_samples = _positive_int(_need(doc, "total_samples", "manifest"), "total_samples")
    tolerance = _positive_int(
        _need(doc, "tolerance_samples", "manifest"), "tolerance_samples"
    )
    segment_duration = _positive_int(
        _need(doc, "segment_duration", "manifest"), "segment_duration"
    )

    envelope_path = os.path.join(project_dir, _need(doc, "envelope_file", "manifest"))
    if not os.path.isfile(envelope_path):
        raise schema_error("包络数据文件不存在", path=envelope_path)
    envelope = np.load(envelope_path)
    if envelope.ndim != 1 or len(envelope) != total_samples:
        raise schema_error(
            "包络数据形状与清单声明不一致",
            field="total_samples",
            declared=total_samples,
            actual=len(envelope),
        )

    segments = []
    for i, s in enumerate(_need(doc, "segments", "manifest")):
        start = _need(s, "start", f"segments[{i}]")
        end = _need(s, "end", f"segments[{i}]")
        if not (0 <= start < end <= total_samples):
            raise schema_error("语音段起止非法", field=f"segments[{i}]", start=start, end=end)
        segments.append(SpeechSegment(seg_id=i, start=start, end=end))

    subtitles = []
    for i, s in enumerate(_need(doc, "subtitles", "manifest")):
        text = _need(s, "text", f"subtitles[{i}]")
        seg_ref = _need(s, "segment", f"subtitles[{i}]")
        if not isinstance(seg_ref, int) or not (0 <= seg_ref < len(segments)):
            raise schema_error(
                "台词引用的语音段越界", field=f"subtitles[{i}].segment", value=seg_ref
            )
        window = s.get("window")
        if window is not None:
            w0, w1 = window
            seg = segments[seg_ref]
            if not (seg.start <= w0 < w1 <= seg.end):
                raise schema_error(
                    "显式显示窗口超出所在语音段",
                    field=f"subtitles[{i}].window",
                    window=window,
                    segment=(seg.start, seg.end),
                )
            window = (int(w0), int(w1))
        subtitles.append(Subtitle(sub_id=i, text=text, segment=seg_ref, window=window))

    a = _need(doc, "assist", "manifest")
    speed = _need(a, "speed", "assist")
    validate_speed(speed)
    mode = _need(a, "highlight_mode", "assist")
    if mode not in ("weighted", "uniform"):
        raise para_error("未知的高亮推进方式", field="assist.highlight_mode", value=mode)
    raw_weights = _need(a, "char_weights", "assist")
    if len(raw_weights) != len(subtitles):
        raise schema_error(
            "逐字权重组数与台词数不一致",
            field="assist.char_weights",
            groups=len(raw_weights),
            subs=len(subtitles),
        )
    char_weights = tuple(tuple(int(w) for w in grp) for grp in raw_weights)
    for i, (grp, sub) in enumerate(zip(char_weights, subtitles)):
        validate_weights(grp, len(sub.text), i)

    t = _need(doc, "truth", "manifest")
    truth = Truth(
        windows=tuple(tuple(w) for w in _need(t, "windows", "truth")),
        char_starts=tuple(tuple(x) for x in _need(t, "char_starts", "truth")),
        total_read_samples=int(_need(t, "total_read_samples", "truth")),
    )
    if len(truth.windows) != len(subtitles) or len(truth.char_starts) != len(subtitles):
        raise schema_error("真值条目数与台词数不一致", field="truth")

    return Project(
        project_id=str(_need(doc, "project_id", "manifest")),
        sample_rate=sample_rate,
        frame_len=frame_len,
        hop=hop,
        total_samples=total_samples,
        tolerance_samples=tolerance,
        segment_duration=segment_duration,
        segments=tuple(segments),
        subtitles=tuple(subtitles),
        assist=AssistParams(
            speed=float(speed), highlight_mode=mode, char_weights=char_weights
        ),
        truth=truth,
        envelope_path=envelope_path,
        manifest_path=manifest_path,
        content_hash=content_hash(raw, envelope),
    )


def load_envelope(project: Project) -> np.ndarray:
    return np.load(project.envelope_path)
