"""媒体工程加载与结构校验。"""
from __future__ import annotations

import hashlib
import json
import os

from .errors import E_PARA, E_REF, E_SCHEMA, PipelineError
from .model import Presentation, Project, Segment, Subtitle

_MANIFEST_FIELDS = ["project_id", "sample_rate", "frame_len", "hop",
                    "audio_file", "segments", "presentation", "subtitles"]
_PRESENTATION_FIELDS = ["max_chars_per_line", "line_height", "margin",
                        "screen_height", "wrap_weight", "param_version"]
_SEGMENT_FIELDS = ["id", "start", "end"]
_SUBTITLE_FIELDS = ["id", "text", "segment_id", "max_cps"]


def _require(obj, field, ctx):
    if field not in obj:
        raise PipelineError(E_SCHEMA, f"{ctx} 缺少必需字段", field=field)
    return obj[field]


def _require_int(value, field):
    if not isinstance(value, int) or isinstance(value, bool):
        raise PipelineError(E_SCHEMA, "字段必须为整数", field=field)
    return value


def _require_positive_int(value, field):
    value = _require_int(value, field)
    if value <= 0:
        raise PipelineError(E_PARA, "参数必须为合规正整数", field=field)
    return value


def _require_number(value, field):
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise PipelineError(E_SCHEMA, "字段必须为数值", field=field)
    return float(value)


def load_project(manifest_path: str) -> Project:
    """读取清单并完成结构校验，返回可查询的 Project。"""
    manifest_dir = os.path.dirname(os.path.abspath(manifest_path))
    try:
        with open(manifest_path, "r", encoding="utf-8") as fh:
            raw = json.load(fh)
    except json.JSONDecodeError as exc:
        raise PipelineError(E_SCHEMA, f"清单不是合法 JSON: {exc}", field="manifest")

    for fld in _MANIFEST_FIELDS:
        _require(raw, fld, "manifest")

    sample_rate = _require_positive_int(raw["sample_rate"], "sample_rate")
    frame_len = _require_positive_int(raw["frame_len"], "frame_len")
    hop = _require_positive_int(raw["hop"], "hop")

    pres_raw = raw["presentation"]
    for fld in _PRESENTATION_FIELDS:
        _require(pres_raw, fld, "presentation")
    presentation = Presentation(
        max_chars_per_line=_require_positive_int(pres_raw["max_chars_per_line"],
                                                 "presentation.max_chars_per_line"),
        line_height=_require_positive_int(pres_raw["line_height"], "presentation.line_height"),
        margin=_require_int(pres_raw["margin"], "presentation.margin"),
        screen_height=_require_positive_int(pres_raw["screen_height"], "presentation.screen_height"),
        wrap_weight=_require_number(pres_raw["wrap_weight"], "presentation.wrap_weight"),
        param_version=str(pres_raw["param_version"]),
    )
    if presentation.margin < 0:
        raise PipelineError(E_PARA, "留白必须为非负整数", field="presentation.margin")
    if presentation.wrap_weight < 0:
        raise PipelineError(E_PARA, "换行权重必须非负", field="presentation.wrap_weight")

    segments = []
    seen_seg_ids = set()
    for idx, seg_raw in enumerate(raw["segments"]):
        for fld in _SEGMENT_FIELDS:
            _require(seg_raw, fld, f"segments[{idx}]")
        seg = Segment(id=_require_int(seg_raw["id"], f"segments[{idx}].id"),
                      start=_require_int(seg_raw["start"], f"segments[{idx}].start"),
                      end=_require_int(seg_raw["end"], f"segments[{idx}].end"))
        if seg.end <= seg.start:
            raise PipelineError(E_PARA, "语音段结束必须大于起点", field=f"segments[{idx}].end")
        if seg.id in seen_seg_ids:
            raise PipelineError(E_SCHEMA, "语音段编号重复", field=f"segments[{idx}].id")
        seen_seg_ids.add(seg.id)
        segments.append(seg)
    segments.sort(key=lambda s: (s.start, s.id))

    subtitles = []
    seen_line_ids = set()
    for idx, sub_raw in enumerate(raw["subtitles"]):
        for fld in _SUBTITLE_FIELDS:
            _require(sub_raw, fld, f"subtitles[{idx}]")
        seg_id = _require_int(sub_raw["segment_id"], f"subtitles[{idx}].segment_id")
        if seg_id not in seen_seg_ids:
            raise PipelineError(E_REF, "台词引用的语音段不存在（悬空引用）",
                                field=f"subtitles[{idx}].segment_id")
        max_cps = _require_number(sub_raw["max_cps"], f"subtitles[{idx}].max_cps")
        if max_cps <= 0:
            raise PipelineError(E_PARA, "阅读速度上限必须为正",
                                field=f"subtitles[{idx}].max_cps")
        text = sub_raw["text"]
        if not isinstance(text, str):
            raise PipelineError(E_SCHEMA, "台词文本必须为字符串", field=f"subtitles[{idx}].text")
        window = None
        if "window" in sub_raw and sub_raw["window"] is not None:
            win = sub_raw["window"]
            if (not isinstance(win, list) or len(win) != 2
                    or not all(isinstance(v, int) for v in win)):
                raise PipelineError(E_SCHEMA, "显式显示窗口必须为 [start, end]",
                                    field=f"subtitles[{idx}].window")
            seg = next(s for s in segments if s.id == seg_id)
            if win[0] < seg.start or win[1] > seg.end or win[1] <= win[0]:
                raise PipelineError(E_REF, "显式显示窗口超出所在语音段（跨段）",
                                    field=f"subtitles[{idx}].window")
            window = (win[0], win[1])
        line_id = str(sub_raw["id"])
        if line_id in seen_line_ids:
            raise PipelineError(E_SCHEMA, "台词编号重复", field=f"subtitles[{idx}].id")
        seen_line_ids.add(line_id)
        subtitles.append(Subtitle(id=line_id, text=text, segment_id=seg_id,
                                  max_cps=max_cps, window=window))

    audio_path = os.path.join(manifest_dir, raw["audio_file"])
    if not os.path.exists(audio_path):
        raise PipelineError(E_REF, "清单引用的音频文件不存在", field="audio_file")

    content_hash = hashlib.sha256(
        json.dumps(raw, sort_keys=True, ensure_ascii=False).encode("utf-8")
    ).hexdigest()

    return Project(
        project_id=str(raw["project_id"]),
        sample_rate=sample_rate,
        frame_len=frame_len,
        hop=hop,
        audio_path=audio_path,
        segments=segments,
        presentation=presentation,
        subtitles=subtitles,
        ground_truth=raw.get("ground_truth") or {},
        manifest_dir=manifest_dir,
        content_hash=content_hash,
    )
