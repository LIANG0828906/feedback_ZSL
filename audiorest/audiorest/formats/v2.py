"""工程格式 v2.0（驼峰命名、采样点分帧、阶段对象数组、独立音频引用块）。"""

from __future__ import annotations

from typing import Any

from ..model import (
    AudioRef,
    Framing,
    RestoreProject,
    StageConfig,
    StageKind,
    UPGRADE_DEFAULTS,
)
from .common import next_pow2, require, trace_entry

VERSION = "2.0"

_KIND_MAP = {
    "spectralSub": StageKind.SPECTRAL_SUBTRACTION,
    "maskSmooth": StageKind.MASK_SMOOTHING,
    "noiseGate": StageKind.NOISE_GATE,
}
_KIND_RMAP = {value: key for key, value in _KIND_MAP.items()}


def parse(data: dict[str, Any]) -> RestoreProject:
    project_id = require(data, "projectId")
    framing_raw = require(data, "framing")
    frame_size = int(require(framing_raw, "frameSamples"))
    hop_size = int(require(framing_raw, "hopSamples"))
    window_seconds = float(require(framing_raw, "windowSec"))
    fft_size = next_pow2(frame_size)

    audio_raw = require(data, "audioRef")
    audio = AudioRef(
        path=str(require(audio_raw, "uri")),
        sample_rate=int(require(audio_raw, "rate")),
        channels=int(require(audio_raw, "ch")),
        sample_width_bytes=int(audio_raw.get("widthBytes", 2)),
        encoding=str(audio_raw.get("codec", "pcm_s16le")),
    )

    stage_docs = require(data, "stageList")
    if not isinstance(stage_docs, list):
        from ..errors import raise_ver

        raise_ver("stageList 必须是数组", "stageList")
    stages: list[StageConfig] = []
    traces: list[dict[str, Any]] = []

    for index, doc in enumerate(stage_docs):
        raw_kind = require(doc, "type")
        if raw_kind not in _KIND_MAP:
            from ..errors import raise_ver

            raise_ver(f"未知阶段类型: {raw_kind}", f"stageList[{index}].type")
        kind = _KIND_MAP[raw_kind]
        params = doc.get("params", {})
        cfg = dict(params)
        if kind is StageKind.SPECTRAL_SUBTRACTION:
            if "strength" not in cfg:
                from ..errors import raise_ver

                raise_ver("谱减强度缺失", f"stageList[{index}].params.strength")
            cfg.setdefault("noise_frames", 8)
        elif kind is StageKind.MASK_SMOOTHING:
            cfg.setdefault("smoothing", 0.5)
            cfg.setdefault("floor_db", -12.0)
        elif kind is StageKind.NOISE_GATE:
            if "threshold_db" not in cfg:
                from ..errors import raise_ver

                raise_ver("噪声门限缺失", f"stageList[{index}].params.threshold_db")
            for key in ("attack_ms", "release_ms"):
                if key not in cfg:
                    default, source = UPGRADE_DEFAULTS[f"stages[noise_gate].config.{key}"]
                    cfg[key] = default
                    traces.append(
                        trace_entry(
                            f"stages[noise_gate].config.{key}", None, default, source
                        )
                    )
        stages.append(StageConfig(kind, bool(doc.get("enabled", True)), cfg))

    # 保证内部阶段顺序稳定（谱减 -> 掩蔽平滑 -> 噪声门）
    order = list(StageKind)
    stages.sort(key=lambda s: order.index(s.kind))

    traces.append(trace_entry("framing.fft_size", None, fft_size, "next_pow2(frame_size)"))
    seed, seed_src = UPGRADE_DEFAULTS["seed"]
    traces.append(trace_entry("seed", None, seed, seed_src))

    return RestoreProject(
        project_id=str(project_id),
        source_version=VERSION,
        framing=Framing(frame_size, hop_size, window_seconds, fft_size),
        stages=tuple(stages),
        audio=audio,
        window_seconds=window_seconds,
        seed=int(seed),
        conversion_trace=tuple(traces),
    )


def export(project: RestoreProject) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    losses: list[dict[str, Any]] = []
    if project.framing.fft_size != next_pow2(project.framing.frame_size):
        losses.append(
            {
                "field_path": "framing.fft_size",
                "value": project.framing.fft_size,
                "reason": "v2 无 fft_size 字段，仅能隐式推导",
                "category": "field",
            }
        )
    losses.append(
        {
            "field_path": "seed",
            "value": project.seed,
            "reason": "v2 无确定性种子字段",
            "category": "field",
        }
    )
    stage_list = []
    for stage in project.stages:
        stage_list.append(
            {
                "type": _KIND_RMAP[stage.kind],
                "enabled": stage.enabled,
                "params": dict(stage.config),
            }
        )
    doc = {
        "formatVersion": "2.0",
        "projectId": project.project_id,
        "framing": {
            "frameSamples": project.framing.frame_size,
            "hopSamples": project.framing.hop_size,
            "windowSec": project.framing.window_seconds,
        },
        "stageList": stage_list,
        "audioRef": {
            "uri": project.audio.path,
            "rate": project.audio.sample_rate,
            "ch": project.audio.channels,
            "widthBytes": project.audio.sample_width_bytes,
            "codec": project.audio.encoding,
        },
    }
    return doc, losses
