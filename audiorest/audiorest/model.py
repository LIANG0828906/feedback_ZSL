"""内部统一修复模型。

任意受支持的工程版本经加载器映射后都得到 :class:`RestoreProject`，
因此修复链只依赖本模块，与源工程格式解耦。

版本标识分三类，检查点同时记录三者：

* ``PARAM_VERSION``：内部参数模型版本（数据类结构与默认值语义）
* ``ALGO_VERSION``：修复算法版本（分帧/谱减/门限的确定性实现）
* ``source_version``：工程来源格式版本（v1/v2/v3）
"""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from enum import Enum
from typing import Any

PARAM_VERSION = "3.0"
ALGO_VERSION = "1.0.0"

SUPPORTED_VERSIONS = ("1.0", "2.0", "3.0")
INTERNAL_VERSION = "3.0"


class StageKind(str, Enum):
    SPECTRAL_SUBTRACTION = "spectral_subtraction"
    MASK_SMOOTHING = "mask_smoothing"
    NOISE_GATE = "noise_gate"


@dataclass(frozen=True)
class Framing:
    """分帧参数（内部统一表达，单位：采样点）。"""

    frame_size: int
    hop_size: int
    window_seconds: float
    fft_size: int


@dataclass(frozen=True)
class StageConfig:
    """单个修复阶段。``kind`` 决定算法身份，``config`` 为阶段参数。"""

    kind: StageKind
    enabled: bool
    config: dict[str, Any]


@dataclass(frozen=True)
class AudioRef:
    """音频引用：相对工程目录的 PCM 路径与解码描述。"""

    path: str
    sample_rate: int
    channels: int
    sample_width_bytes: int
    encoding: str = "pcm_s16le"


@dataclass(frozen=True)
class RestoreProject:
    """内部修复工程模型。"""

    project_id: str
    source_version: str
    framing: Framing
    stages: tuple[StageConfig, ...]
    audio: AudioRef
    window_seconds: float
    seed: int
    # 仅用于可追溯展示，不参与修复计算
    conversion_trace: tuple[dict[str, Any], ...] = field(default_factory=tuple)

    def stage(self, kind: StageKind) -> StageConfig:
        for stage in self.stages:
            if stage.kind == kind:
                return stage
        raise KeyError(f"stage {kind} not present")

    def with_runtime_params(
        self,
        *,
        subtraction_strength: float | None = None,
        gate_threshold_db: float | None = None,
    ) -> "RestoreProject":
        """返回调整谱减强度/门限后的新工程，未提交窗口使用该快照。"""
        stages = list(self.stages)
        for index, stage in enumerate(stages):
            cfg = dict(stage.config)
            if stage.kind is StageKind.SPECTRAL_SUBTRACTION and subtraction_strength is not None:
                cfg["strength"] = float(subtraction_strength)
            if stage.kind is StageKind.NOISE_GATE and gate_threshold_db is not None:
                cfg["threshold_db"] = float(gate_threshold_db)
            stages[index] = StageConfig(stage.kind, stage.enabled, cfg)
        return replace(self, stages=tuple(stages))

    def canonical(self) -> dict[str, Any]:
        """等价性比对用的规范化字典（不含转换痕迹等元数据）。"""
        return {
            "project_id": self.project_id,
            "framing": {
                "frame_size": self.framing.frame_size,
                "hop_size": self.framing.hop_size,
                "window_seconds": self.framing.window_seconds,
                "fft_size": self.framing.fft_size,
            },
            "stages": [
                {"kind": s.kind.value, "enabled": s.enabled, "config": _stable(s.config)}
                for s in self.stages
            ],
            "audio": {
                "path": self.audio.path,
                "sample_rate": self.audio.sample_rate,
                "channels": self.audio.channels,
                "sample_width_bytes": self.audio.sample_width_bytes,
                "encoding": self.audio.encoding,
            },
            "window_seconds": self.window_seconds,
        }


def _stable(value: Any) -> Any:
    if isinstance(value, dict):
        return {k: _stable(value[k]) for k in sorted(value)}
    if isinstance(value, (list, tuple)):
        return [_stable(v) for v in value]
    return value


# 升级时新增字段的声明式默认值（字段路径 -> 默认值与说明）
UPGRADE_DEFAULTS: dict[str, tuple[Any, str]] = {
    "framing.fft_size": (None, "升级时取不小于 frame_size 的 2 次幂"),
    "framing.window_seconds": (1.0, "v1 无窗口概念，默认固定 1.0 秒"),
    "stages[mask_smoothing].config.smoothing": (0.5, "v1 仅声明阶段，默认平滑系数 0.5"),
    "stages[mask_smoothing].config.floor_db": (-12.0, "掩蔽地板默认 -12 dB"),
    "stages[noise_gate].config.attack_ms": (5.0, "v2 无起音参数，默认 5 ms"),
    "stages[noise_gate].config.release_ms": (50.0, "v2 无释放参数，默认 50 ms"),
    "seed": (0, "旧版本无确定性种子字段，默认 0"),
}
