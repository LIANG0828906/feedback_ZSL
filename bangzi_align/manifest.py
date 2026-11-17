from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .constants import (
    E_BIT_DEPTH,
    E_DUPLICATE_CHANNEL,
    E_DUPLICATE_VOICE,
    E_ENTRY_RANGE,
    E_FADE_TOO_LONG,
    E_FILE_MISSING,
    E_HIT_ORDER,
    E_HIT_RANGE,
    E_INVALID_VOICE,
    E_MANIFEST_FIELD,
    E_MANIFEST_JSON,
    E_MANIFEST_SCHEMA,
    E_SEGMENT_OVERLAP,
    E_SEGMENT_RANGE,
    E_SEPARATION_RANGE,
    E_VOICE_SET,
    VOICE_ORDER,
    VOICES,
)
from .errors import Diagnostic, DiagnosticError
from .wavio import WavData, file_sha256, read_wav, validate_wav_header


@dataclass(frozen=True)
class ChannelSpec:
    id: str
    voice: str
    path: Path
    path_text: str
    start_offset_samples: int
    pan: float


@dataclass(frozen=True)
class SegmentSpec:
    id: str
    ban_style: str
    start_sample: int
    end_sample: int
    bangzi_reference_samples: tuple[int, ...]
    bangqiang_entry_sample: int
    separation_window_samples: tuple[int, int]
    channel_gains_db: dict[str, float]


@dataclass(frozen=True)
class Manifest:
    path: Path
    raw_sha256: str
    schema: str
    seed: int
    sample_rate: int
    bit_depth: int
    channels: tuple[ChannelSpec, ...]
    segments: tuple[SegmentSpec, ...]
    crossfade_samples: int
    target_lufs: float
    true_peak_max_dbtp: float
    block_samples: int

    def channel_by_voice(self, voice: str) -> ChannelSpec:
        for channel in self.channels:
            if channel.voice == voice:
                return channel
        raise KeyError(voice)

    @property
    def bangzi(self) -> ChannelSpec:
        return self.channel_by_voice("梆子")

    @property
    def laosheng(self) -> ChannelSpec:
        return self.channel_by_voice("老生")

    @property
    def bangqiang(self) -> ChannelSpec:
        return self.channel_by_voice("帮腔")


class _FieldProblem(Exception):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message


def _require_dict(value: Any, name: str, channel: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, channel, f"{name} 必须是 JSON 对象")]
        )
    return value


def _require_list(value: Any, name: str, channel: str) -> list[Any]:
    if not isinstance(value, list):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, channel, f"{name} 必须是 JSON 数组")]
        )
    return value


def _integer(value: Any, name: str, channel: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, channel, f"{name} 必须是整数采样点")]
        )
    return value


def _number(value: Any, name: str, channel: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, channel, f"{name} 必须是数值")]
        )
    result = float(value)
    if not math.isfinite(result):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, channel, f"{name} 必须是有限数值")]
        )
    return result


def _position(
    obj: dict[str, Any],
    sample_key: str,
    seconds_key: str,
    sample_rate: int,
    name: str,
    channel: str,
) -> int:
    if sample_key in obj:
        return _integer(obj[sample_key], name, channel)
    if seconds_key in obj:
        return int(round(_number(obj[seconds_key], name, channel) * sample_rate))
    raise DiagnosticError(
        [Diagnostic(E_MANIFEST_FIELD, channel, f"缺少 {sample_key} 或 {seconds_key}")]
    )


def _load_json(path: Path) -> dict[str, Any]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise DiagnosticError(
            [Diagnostic(E_FILE_MISSING, "manifest", f"清单文件不可读: {exc}")]
        ) from None
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_JSON, "manifest", f"JSON 解析失败: {exc}")]
        ) from None
    if not isinstance(payload, dict):
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_JSON, "manifest", "清单顶层必须是 JSON 对象")]
        )
    return payload


def load_manifest(path: str | Path) -> Manifest:
    manifest_path = Path(path)
    try:
        raw = manifest_path.read_bytes()
    except OSError as exc:
        raise DiagnosticError(
            [Diagnostic(E_FILE_MISSING, "manifest", f"清单文件不可读: {exc}")]
        ) from None
    payload = _load_json(manifest_path)
    schema = payload.get("schema")
    if schema != "bangzi-session/v1":
        raise DiagnosticError(
            [
                Diagnostic(
                    E_MANIFEST_SCHEMA,
                    "manifest",
                    "schema 必须为 bangzi-session/v1",
                )
            ]
        )
    seed = _integer(payload.get("seed"), "seed", "manifest")
    audio = _require_dict(payload.get("audio"), "audio", "manifest")
    sample_rate = _integer(audio.get("sample_rate"), "audio.sample_rate", "manifest")
    bit_depth = _integer(audio.get("bit_depth"), "audio.bit_depth", "manifest")
    if sample_rate <= 0:
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, "manifest", "audio.sample_rate 必须大于 0")]
        )
    if bit_depth not in (16, 24, 32):
        raise DiagnosticError(
            [Diagnostic(E_BIT_DEPTH, "manifest", "audio.bit_depth 仅支持 16/24/32")]
        )

    channel_items = _require_list(payload.get("channels"), "channels", "manifest")
    channels: list[ChannelSpec] = []
    diagnostics: list[Diagnostic] = []
    seen_ids: set[str] = set()
    seen_voices: dict[str, list[str]] = {}
    for index, item in enumerate(channel_items):
        try:
            obj = _require_dict(item, f"channels[{index}]", "manifest")
            channel_id = obj.get("id")
            if not isinstance(channel_id, str) or not channel_id:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            f"channels[{index}]",
                            "通道 id 必须是非空字符串",
                        )
                    ]
                )
            if channel_id in seen_ids:
                diagnostics.append(
                    Diagnostic(E_DUPLICATE_CHANNEL, channel_id, "通道 id 重复")
                )
            seen_ids.add(channel_id)
            voice = obj.get("voice")
            if not isinstance(voice, str) or voice not in VOICES:
                diagnostics.append(
                    Diagnostic(E_INVALID_VOICE, channel_id, f"未知声部名称: {voice!r}")
                )
                continue
            seen_voices.setdefault(voice, []).append(channel_id)
            path_text = obj.get("path")
            if not isinstance(path_text, str) or not path_text:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            channel_id,
                            "通道 path 必须是非空字符串",
                        )
                    ]
                )
            channel_path = Path(path_text)
            if not channel_path.is_absolute():
                channel_path = manifest_path.parent / channel_path
            start_offset = _integer(
                obj.get("start_offset_samples"),
                f"channels[{index}].start_offset_samples",
                channel_id,
            )
            pan = _number(obj.get("pan", 0.0), f"channels[{index}].pan", channel_id)
            if pan < -1.0 or pan > 1.0:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            channel_id,
                            "pan 必须在 [-1, 1]",
                        )
                    ]
                )
            channels.append(
                ChannelSpec(
                    id=channel_id,
                    voice=voice,
                    path=channel_path,
                    path_text=path_text,
                    start_offset_samples=start_offset,
                    pan=pan,
                )
            )
        except DiagnosticError as exc:
            diagnostics.extend(exc.diagnostics)

    for voice, ids in seen_voices.items():
        if len(ids) > 1:
            for channel_id in ids:
                diagnostics.append(
                    Diagnostic(
                        E_DUPLICATE_VOICE,
                        channel_id,
                        f"声部名称重复: {voice} (通道: {', '.join(ids)})",
                    )
                )
    missing_voices = [voice for voice in VOICES if voice not in seen_voices]
    if missing_voices:
        diagnostics.append(
            Diagnostic(
                E_VOICE_SET,
                "|".join(c.id for c in channels) or "manifest",
                "缺少声部: " + "、".join(missing_voices),
            )
        )
    if len(channels) != 5 and not missing_voices:
        diagnostics.append(
            Diagnostic(
                E_VOICE_SET,
                "|".join(c.id for c in channels),
                "必须恰好包含五路声部",
            )
        )
    if diagnostics:
        diagnostics.sort(key=lambda item: (item.channel, item.code, item.message))
        raise DiagnosticError(diagnostics)

    channels.sort(key=lambda item: VOICE_ORDER[item.voice])
    channel_ids = [channel.id for channel in channels]

    segment_items = _require_list(payload.get("segments"), "segments", "manifest")
    if not segment_items:
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, "segments", "至少需要一个段落")]
        )
    segments: list[SegmentSpec] = []
    segment_diagnostics: list[Diagnostic] = []
    seen_segment_ids: set[str] = set()
    bangzi_channel_id = next(c.id for c in channels if c.voice == "梆子")
    for index, item in enumerate(segment_items):
        try:
            obj = _require_dict(item, f"segments[{index}]", "manifest")
            segment_id = obj.get("id", f"segment_{index + 1}")
            if not isinstance(segment_id, str) or not segment_id:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            f"segments[{index}].id",
                            "段落 id 必须是非空字符串",
                        )
                    ]
                )
            if segment_id in seen_segment_ids:
                segment_diagnostics.append(
                    Diagnostic(E_MANIFEST_FIELD, "segments", f"段落 id 重复: {segment_id}")
                )
            seen_segment_ids.add(segment_id)
            ban_style = obj.get("ban_style")
            if not isinstance(ban_style, str) or not ban_style:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            "segments",
                            f"{segment_id}.ban_style 必须是非空字符串",
                        )
                    ]
                )
            start = _position(
                obj, "start_sample", "start_time", sample_rate, "start", "segments"
            )
            end = _position(obj, "end_sample", "end_time", sample_rate, "end", "segments")
            if start < 0 or end <= start:
                segment_diagnostics.append(
                    Diagnostic(
                        E_SEGMENT_RANGE,
                        "segments",
                        f"段落 {segment_id} 时间范围非法: [{start}, {end})",
                    )
                )
            if "bangzi_reference_samples" in obj:
                refs_raw = _require_list(
                    obj["bangzi_reference_samples"],
                    f"segments[{index}].bangzi_reference_samples",
                    bangzi_channel_id,
                )
            elif "bangzi_reference_times" in obj:
                refs_raw = _require_list(
                    obj["bangzi_reference_times"],
                    f"segments[{index}].bangzi_reference_times",
                    bangzi_channel_id,
                )
                refs_raw = [
                    int(round(_number(value, "bangzi_reference_times", bangzi_channel_id) * sample_rate))
                    for value in refs_raw
                ]
            else:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            bangzi_channel_id,
                            f"{segment_id} 缺少梆子击点参考时刻",
                        )
                    ]
                )
            refs: list[int] = []
            for ref_index, value in enumerate(refs_raw):
                refs.append(
                    _integer(
                        value,
                        f"{segment_id}.bangzi_reference_samples[{ref_index}]",
                        bangzi_channel_id,
                    )
                )
            if not refs:
                segment_diagnostics.append(
                    Diagnostic(
                        E_MANIFEST_FIELD,
                        bangzi_channel_id,
                        f"段落 {segment_id} 没有梆子击点参考时刻",
                    )
                )
            if any(right <= left for left, right in zip(refs, refs[1:])):
                segment_diagnostics.append(
                    Diagnostic(
                        E_HIT_ORDER,
                        bangzi_channel_id,
                        f"段落 {segment_id} 的击点参考时刻不是严格升序",
                    )
                )
            if any(ref < start or ref >= end for ref in refs):
                segment_diagnostics.append(
                    Diagnostic(
                        E_HIT_RANGE,
                        bangzi_channel_id,
                        f"段落 {segment_id} 存在段落范围外的击点参考",
                    )
                )
            entry = _position(
                obj,
                "bangqiang_entry_sample",
                "bangqiang_entry_time",
                sample_rate,
                "bangqiang_entry",
                "segments",
            )
            if entry < start or entry >= end:
                segment_diagnostics.append(
                    Diagnostic(
                        E_ENTRY_RANGE,
                        "segments",
                        f"段落 {segment_id} 帮腔进入时刻不在段落内",
                    )
                )
            if "separation_window_samples" in obj:
                window_raw = obj["separation_window_samples"]
            elif "separation_window_times" in obj:
                window_raw = obj["separation_window_times"]
                window_raw = [
                    int(round(_number(value, "separation_window_times", "segments") * sample_rate))
                    for value in window_raw
                ]
            else:
                window_raw = [entry, min(end, entry + int(round(0.5 * sample_rate)))]
            window_list = _require_list(
                window_raw, f"segments[{index}].separation_window", "segments"
            )
            if len(window_list) != 2:
                raise DiagnosticError(
                    [
                        Diagnostic(
                            E_MANIFEST_FIELD,
                            "segments",
                            f"段落 {segment_id} 分离时间窗必须有两个采样点",
                        )
                    ]
                )
            window_start = _integer(window_list[0], "separation_window[0]", "segments")
            window_end = _integer(window_list[1], "separation_window[1]", "segments")
            if (
                window_start > window_end
                or window_start < start
                or window_end > end
                or window_start < entry
            ):
                segment_diagnostics.append(
                    Diagnostic(
                        E_SEPARATION_RANGE,
                        "segments",
                        f"段落 {segment_id} 分离时间窗非法或未覆盖帮腔进入",
                    )
                )
            gains_obj = _require_dict(
                obj.get("channel_gains_db"), f"segments[{index}].channel_gains_db", "segments"
            )
            gains: dict[str, float] = {}
            for channel_id in channel_ids:
                gains[channel_id] = _number(
                    gains_obj.get(channel_id, 0.0),
                    f"segments[{index}].channel_gains_db.{channel_id}",
                    channel_id,
                )
            segments.append(
                SegmentSpec(
                    id=segment_id,
                    ban_style=ban_style,
                    start_sample=start,
                    end_sample=end,
                    bangzi_reference_samples=tuple(refs),
                    bangqiang_entry_sample=entry,
                    separation_window_samples=(window_start, window_end),
                    channel_gains_db=gains,
                )
            )
        except DiagnosticError as exc:
            segment_diagnostics.extend(exc.diagnostics)
    segments.sort(key=lambda item: (item.start_sample, item.end_sample, item.id))
    for left, right in zip(segments, segments[1:]):
        if right.start_sample < left.end_sample:
            segment_diagnostics.append(
                Diagnostic(
                    E_SEGMENT_OVERLAP,
                    "segments",
                    f"段落 {left.id} 与 {right.id} 时间重叠",
                )
            )
    if segment_diagnostics:
        segment_diagnostics.sort(key=lambda item: (item.channel, item.code, item.message))
        raise DiagnosticError(segment_diagnostics)

    crossfade = _integer(
        payload.get("crossfade_samples", int(round(0.08 * sample_rate))),
        "crossfade_samples",
        "manifest",
    )
    if crossfade <= 0:
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, "manifest", "crossfade_samples 必须大于 0")]
        )
    shortest = min(segment.end_sample - segment.start_sample for segment in segments)
    if crossfade > shortest:
        raise DiagnosticError(
            [
                Diagnostic(
                    E_FADE_TOO_LONG,
                    "segments",
                    "crossfade_samples 不能长于最短段落",
                )
            ]
        )
    master = _require_dict(payload.get("master"), "master", "manifest")
    target_lufs = _number(master.get("target_lufs"), "master.target_lufs", "manifest")
    true_peak = _number(
        master.get("true_peak_max_dbtp", -1.0),
        "master.true_peak_max_dbtp",
        "manifest",
    )
    if true_peak > 0.0:
        raise DiagnosticError(
            [
                Diagnostic(
                    E_MANIFEST_FIELD,
                    "manifest",
                    "master.true_peak_max_dbtp 必须不大于 0 dBTP",
                )
            ]
        )
    block_samples = _integer(
        master.get("block_samples", 4096), "master.block_samples", "manifest"
    )
    if block_samples <= 0:
        raise DiagnosticError(
            [Diagnostic(E_MANIFEST_FIELD, "manifest", "master.block_samples 必须大于 0")]
        )

    return Manifest(
        path=manifest_path,
        raw_sha256=hashlib.sha256(raw).hexdigest(),
        schema=schema,
        seed=seed,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=tuple(channels),
        segments=tuple(segments),
        crossfade_samples=crossfade,
        target_lufs=target_lufs,
        true_peak_max_dbtp=true_peak,
        block_samples=block_samples,
    )


@dataclass(frozen=True)
class SourceSet:
    data: dict[str, WavData]
    sha256: dict[str, str]


def load_sources(manifest: Manifest) -> SourceSet:
    diagnostics: list[Diagnostic] = []
    loaded: dict[str, WavData] = {}
    digests: dict[str, str] = {}
    for channel in manifest.channels:
        if not channel.path.is_file():
            diagnostics.append(
                Diagnostic(
                    E_FILE_MISSING,
                    channel.id,
                    f"WAV 文件缺失: {channel.path_text}",
                )
            )
            continue
        try:
            validate_wav_header(
                channel.path,
                channel.id,
                manifest.sample_rate,
                manifest.bit_depth,
            )
            loaded[channel.id] = read_wav(channel.path, channel.id)
            digests[channel.id] = file_sha256(channel.path)
        except DiagnosticError as exc:
            diagnostics.extend(exc.diagnostics)
    if diagnostics:
        diagnostics.sort(key=lambda item: (item.channel, item.code, item.message))
        raise DiagnosticError(diagnostics)
    return SourceSet(data=loaded, sha256=digests)

