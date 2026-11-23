"""处理链参数配置、版本标识与参数校验。

所有数值参数均为 dataclass，序列化后参与检查点的参数版本指纹，
保证“参数变化 -> 旧快照整体作废 -> 全量重算”。
"""
from __future__ import annotations

import dataclasses
import hashlib
import json
from dataclasses import dataclass, field

from .errors import ParameterError

#: 算法版本：K 加权/门限/压缩/限幅/归一实现发生语义变化时必须递增。
ALGORITHM_VERSION = "bs1770-dsp-1.0.0"

#: 支持的采样率（K 加权滤波器可对任意 >= 32 kHz 采样率设计，
#: 这里按广播工程惯例白名单约束，越界返回 E_PARA）。
SUPPORTED_SAMPLE_RATES = (32000, 44100, 48000, 96000)

#: 响度门限常量（ITU-R BS.1770-4）。
ABSOLUTE_GATE_LKFS = -70.0
RELATIVE_GATE_OFFSET_LU = -10.0


def _require(condition: bool, message: str, field_name: str) -> None:
    if not condition:
        raise ParameterError(message, field=field_name)


@dataclass(frozen=True)
class FrameConfig:
    """分帧参数（秒）。BS.1770 默认 0.4s 帧长、0.1s 帧移（75% 重叠）。"""

    frame_seconds: float = 0.4
    hop_seconds: float = 0.1
    absolute_gate: float = ABSOLUTE_GATE_LKFS
    relative_gate_offset: float = RELATIVE_GATE_OFFSET_LU
    true_peak_oversample: int = 4

    def validate(self, sample_rate: int) -> None:
        _require(sample_rate in SUPPORTED_SAMPLE_RATES,
                 f"不支持的采样率 {sample_rate}，允许 {SUPPORTED_SAMPLE_RATES}",
                 "sample_rate")
        _require(isinstance(self.frame_seconds, (int, float))
                 and self.frame_seconds > 0,
                 "frame_seconds 必须为正数", "frame_seconds")
        _require(isinstance(self.hop_seconds, (int, float))
                 and self.hop_seconds > 0,
                 "hop_seconds 必须为正数", "hop_seconds")
        _require(self.hop_seconds <= self.frame_seconds,
                 "hop_seconds 不得大于 frame_seconds", "hop_seconds")
        frame_len = int(round(self.frame_seconds * sample_rate))
        _require(frame_len >= 64, "帧长样本数过少（<64）", "frame_seconds")
        _require(self.true_peak_oversample in (1, 2, 4, 8),
                 "true_peak_oversample 仅支持 1/2/4/8",
                 "true_peak_oversample")
        _require(-90.0 <= self.absolute_gate <= -50.0,
                 "absolute_gate 超出合理区间 [-90, -50] LKFS",
                 "absolute_gate")
        _require(-20.0 <= self.relative_gate_offset <= 0.0,
                 "relative_gate_offset 超出区间 [-20, 0] LU",
                 "relative_gate_offset")

@dataclass(frozen=True)
class CompressorConfig:
    """动态范围压缩器参数。

    电平以 dBFS 表示；attack/release 为一阶平滑时间常数（秒）。
    """

    threshold_db: float = -20.0
    ratio: float = 3.0
    attack_seconds: float = 0.01
    release_seconds: float = 0.1
    makeup_gain_db: float = 0.0

    def validate(self) -> None:
        _require(-80.0 <= self.threshold_db <= 0.0,
                 "threshold_db 超出 [-80, 0] dBFS", "threshold_db")
        _require(self.ratio >= 1.0,
                 "ratio 必须 >= 1.0", "ratio")
        _require(self.attack_seconds > 0.0,
                 "attack_seconds 必须为正数", "attack_seconds")
        _require(self.release_seconds > 0.0,
                 "release_seconds 必须为正数", "release_seconds")
        _require(-24.0 <= self.makeup_gain_db <= 24.0,
                 "makeup_gain_db 超出 [-24, 24] dB", "makeup_gain_db")


@dataclass(frozen=True)
class LimiterConfig:
    """前瞻限幅器参数。ceiling 为真峰上限（dBTP）。"""

    ceiling_db: float = -1.0
    lookahead_seconds: float = 0.002
    release_seconds: float = 0.05

    def validate(self, sample_rate: int) -> None:
        _require(sample_rate in SUPPORTED_SAMPLE_RATES,
                 f"不支持的采样率 {sample_rate}", "sample_rate")
        _require(-24.0 <= self.ceiling_db <= 0.0,
                 "ceiling_db 超出 [-24, 0] dBTP", "ceiling_db")
        _require(self.lookahead_seconds > 0.0,
                 "lookahead_seconds 必须为正数", "lookahead_seconds")
        _require(int(round(self.lookahead_seconds * sample_rate)) >= 1,
                 "lookahead 时长不足一个采样点", "lookahead_seconds")
        _require(self.release_seconds > 0.0,
                 "release_seconds 必须为正数", "release_seconds")


@dataclass(frozen=True)
class NormalizeConfig:
    """全局响度归一参数。"""

    target_lufs: float = -23.0
    tolerance_lu: float = 0.5
    max_correction_db: float = 6.0

    def validate(self) -> None:
        _require(-40.0 <= self.target_lufs <= -5.0,
                 "target_lufs 超出 [-40, -5] LUFS", "target_lufs")
        _require(0.0 < self.tolerance_lu <= 2.0,
                 "tolerance_lu 超出 (0, 2] LU", "tolerance_lu")
        _require(0.0 < self.max_correction_db <= 24.0,
                 "max_correction_db 超出 (0, 24] dB", "max_correction_db")


@dataclass(frozen=True)
class SchedulerConfig:
    """并行度与每线程内存缓冲预算（样本数）。"""

    max_workers: int = 2
    per_worker_buffer_samples: int = 512 * 1024

    def validate(self) -> None:
        _require(1 <= self.max_workers <= 16,
                 "max_workers 超出 [1, 16]", "max_workers")
        _require(self.per_worker_buffer_samples >= 8192,
                 "per_worker_buffer_samples 必须 >= 8192",
                 "per_worker_buffer_samples")


@dataclass(frozen=True)
class PipelineConfig:
    """端到端处理链总配置。"""

    frame: FrameConfig = field(default_factory=FrameConfig)
    compressor: CompressorConfig = field(default_factory=CompressorConfig)
    limiter: LimiterConfig = field(default_factory=LimiterConfig)
    normalize: NormalizeConfig = field(default_factory=NormalizeConfig)
    scheduler: SchedulerConfig = field(default_factory=SchedulerConfig)

    def validate(self, sample_rate: int) -> None:
        self.frame.validate(sample_rate)
        self.compressor.validate()
        self.limiter.validate(sample_rate)
        self.normalize.validate()
        self.scheduler.validate()

    def to_dict(self) -> dict:
        return dataclasses.asdict(self)

    def parameter_fingerprint(self) -> str:
        """参数版本指纹：任一参数变化都会改变指纹。"""
        payload = json.dumps(self.to_dict(), sort_keys=True,
                             ensure_ascii=False)
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]
