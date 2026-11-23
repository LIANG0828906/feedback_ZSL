"""工程格式 v1.0（legacy，蛇形命名、毫秒分帧、阶段名列表风格）。"""

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

VERSION = "1.0"

_LEGACY_STAGE_NAMES = {
    StageKind.SPECTRAL_SUBTRACTION: "spectral_sub",
    StageKind.MASK_SMOOTHING: "mask_smooth",
    StageKind.NOISE_GATE: "noise_gate",
}


def parse(data: dict[str, Any]) -> RestoreProject:
    project_id = require(data, "project_id")
    framing_raw = require(data, "frame")
    frame_ms = require(framing_raw, "frame_len_ms")
    hop_ms = require(framing_raw, "hop_ms")
    sample_rate = require(data, "sample_rate")
    pipeline = require(data, "pipeline")
    source = require(data, "source")
    pcm_path = require(source, "pcm_file")
    channels = require(source, "channels")

    frame_size = int(round(frame_ms * sample_rate / 1000.0))
    hop_size = int(round(hop_ms * sample_rate / 1000.0))
    fft_size = next_pow2(frame_size)
    window_seconds, window_default = UPGRADE_DEFAULTS["framing.window_seconds"]

    stages: list[StageConfig] = []
    traces: list[dict[str, Any]] = []

    # 谱减参数以扁平字段表达
    strength = require(data, "subtraction_strength")
    noise_frames = data.get("noise_frames", 8)
    stages.append(
        StageConfig(
            StageKind.SPECTRAL_SUBTRACTION,
            True,
            {"strength": float(strength), "noise_frames": int(noise_frames)},
        )
    )
    # v1 仅以阶段名声明掩蔽平滑，参数全部由升级默认值补齐
    smoothing, smooth_src = UPGRADE_DEFAULTS["stages[mask_smoothing].config.smoothing"]
    floor_db, floor_src = UPGRADE_DEFAULTS["stages[mask_smoothing].config.floor_db"]
    stages.append(
        StageConfig(
            StageKind.MASK_SMOOTHING,
            _LEGACY_STAGE_NAMES[StageKind.MASK_SMOOTHING] in pipeline,
            {"smoothing": smoothing, "floor_db": floor_db},
        )
    )
    traces.append(
        trace_entry("stages[mask_smoothing].config.smoothing", None, smoothing, smooth_src)
    )
    traces.append(trace_entry("stages[mask_smoothing].config.floor_db", None, floor_db, floor_src))

    gate_db = require(data, "gate_threshold_db")
    attack, attack_src = UPGRADE_DEFAULTS["stages[noise_gate].config.attack_ms"]
    release, release_src = UPGRADE_DEFAULTS["stages[noise_gate].config.release_ms"]
    stages.append(
        StageConfig(
            StageKind.NOISE_GATE,
            _LEGACY_STAGE_NAMES[StageKind.NOISE_GATE] in pipeline,
            {"threshold_db": float(gate_db), "attack_ms": attack, "release_ms": release},
        )
    )
    traces.append(trace_entry("stages[noise_gate].config.attack_ms", None, attack, attack_src))
    traces.append(trace_entry("stages[noise_gate].config.release_ms", None, release, release_src))

    traces.append(trace_entry("framing.fft_size", None, fft_size, "next_pow2(frame_size)"))
    traces.append(
        trace_entry("framing.window_seconds", None, window_seconds, window_default)
    )
    seed, seed_src = UPGRADE_DEFAULTS["seed"]
    traces.append(trace_entry("seed", None, seed, seed_src))

    return RestoreProject(
        project_id=str(project_id),
        source_version=VERSION,
        framing=Framing(frame_size, hop_size, float(window_seconds), fft_size),
        stages=tuple(stages),
        audio=AudioRef(
            path=str(pcm_path),
            sample_rate=int(sample_rate),
            channels=int(channels),
            sample_width_bytes=2,
        ),
        window_seconds=float(window_seconds),
        seed=int(seed),
        conversion_trace=tuple(traces),
    )


def export(project: RestoreProject) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """降级导出 v1；无法表达的字段进入损失清单，阶段不丢弃。"""
    losses: list[dict[str, Any]] = []
    frame_ms = project.framing.frame_size * 1000.0 / project.audio.sample_rate
    hop_ms = project.framing.hop_size * 1000.0 / project.audio.sample_rate
    sub = project.stage(StageKind.SPECTRAL_SUBTRACTION)
    gate = project.stage(StageKind.NOISE_GATE)
    smooth = project.stage(StageKind.MASK_SMOOTHING)

    if project.framing.fft_size != next_pow2(project.framing.frame_size):
        losses.append(
            {
                "field_path": "frame.fft_size",
                "value": project.framing.fft_size,
                "reason": "v1 无 fft_size 字段，仅能隐式推导",
                "category": "field",
            }
        )
    for key in ("smoothing", "floor_db"):
        losses.append(
            {
                "field_path": f"stages[mask_smoothing].config.{key}",
                "value": smooth.config.get(key),
                "reason": "v1 掩蔽平滑仅能声明阶段名，参数无法表达",
                "category": "stage_param",
            }
        )
    for key in ("attack_ms", "release_ms"):
        losses.append(
            {
                "field_path": f"stages[noise_gate].config.{key}",
                "value": gate.config.get(key),
                "reason": "v1 噪声门仅支持门限，包络参数无法表达",
                "category": "stage_param",
            }
        )
    losses.append(
        {
            "field_path": "framing.window_seconds",
            "value": project.framing.window_seconds,
            "reason": "v1 无窗口状态机概念",
            "category": "field",
        }
    )

    doc = {
        "format_version": "1.0",
        "project_id": project.project_id,
        "sample_rate": project.audio.sample_rate,
        "frame": {
            "frame_len_ms": frame_ms,
            "hop_ms": hop_ms,
        },
        "subtraction_strength": sub.config["strength"],
        "noise_frames": sub.config.get("noise_frames", 8),
        "gate_threshold_db": gate.config["threshold_db"],
        "pipeline": [
            _LEGACY_STAGE_NAMES[s.kind] for s in project.stages if s.enabled
        ],
        "source": {
            "pcm_file": project.audio.path,
            "channels": project.audio.channels,
        },
    }
    return doc, losses
