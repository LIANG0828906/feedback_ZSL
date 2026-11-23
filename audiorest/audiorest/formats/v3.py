"""工程格式 v3.0（当前内部模型的直接序列化格式）。"""

from __future__ import annotations

from typing import Any

from ..errors import raise_ver
from ..model import (
    AudioRef,
    Framing,
    RestoreProject,
    StageConfig,
    StageKind,
)
from .common import require

VERSION = "3.0"


def parse(data: dict[str, Any]) -> RestoreProject:
    project_id = require(data, "project_id")
    framing_raw = require(data, "framing")
    framing = Framing(
        frame_size=int(require(framing_raw, "frame_size")),
        hop_size=int(require(framing_raw, "hop_size")),
        window_seconds=float(require(framing_raw, "window_seconds")),
        fft_size=int(require(framing_raw, "fft_size")),
    )
    audio_raw = require(data, "audio")
    audio = AudioRef(
        path=str(require(audio_raw, "path")),
        sample_rate=int(require(audio_raw, "sample_rate")),
        channels=int(require(audio_raw, "channels")),
        sample_width_bytes=int(require(audio_raw, "sample_width_bytes")),
        encoding=str(audio_raw.get("encoding", "pcm_s16le")),
    )
    stage_docs = require(data, "stages")
    if not isinstance(stage_docs, list):
        raise_ver("stages 必须是数组", "stages")
    stages: list[StageConfig] = []
    for index, doc in enumerate(stage_docs):
        kind_value = require(doc, "kind")
        try:
            kind = StageKind(kind_value)
        except ValueError:
            raise_ver(f"未知阶段类型: {kind_value}", f"stages[{index}].kind")
        stages.append(
            StageConfig(
                kind=kind,
                enabled=bool(doc.get("enabled", True)),
                config=dict(require(doc, "config")),
            )
        )
    return RestoreProject(
        project_id=str(project_id),
        source_version=VERSION,
        framing=framing,
        stages=tuple(stages),
        audio=audio,
        window_seconds=float(require(data, "window_seconds")),
        seed=int(require(data, "seed")),
        conversion_trace=tuple(data.get("conversion_trace", ())),
    )


def export(project: RestoreProject) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    doc = {
        "format_version": "3.0",
        "project_id": project.project_id,
        "framing": {
            "frame_size": project.framing.frame_size,
            "hop_size": project.framing.hop_size,
            "window_seconds": project.framing.window_seconds,
            "fft_size": project.framing.fft_size,
        },
        "stages": [
            {
                "kind": stage.kind.value,
                "enabled": stage.enabled,
                "config": dict(stage.config),
            }
            for stage in project.stages
        ],
        "audio": {
            "path": project.audio.path,
            "sample_rate": project.audio.sample_rate,
            "channels": project.audio.channels,
            "sample_width_bytes": project.audio.sample_width_bytes,
            "encoding": project.audio.encoding,
        },
        "window_seconds": project.window_seconds,
        "seed": project.seed,
        "conversion_trace": list(project.conversion_trace),
    }
    return doc, []
