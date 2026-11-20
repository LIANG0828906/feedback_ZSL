"""处理链路的确定性配置模型。

所有参数均不可变；声道/频带级覆盖用于增量重算时计算受影响集合。
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field, replace
from typing import Dict, Optional, Tuple

SAMPLE_RATE = 48000

# 5.1 声道布局名称（与 WAV 声道顺序一致）
LAYOUT_51 = ("FL", "FR", "FC", "LFE", "SL", "SR")

DEFAULT_BANDS: Tuple[Tuple[str, float, float], ...] = (
    ("b125", 20.0, 177.0),
    ("b250", 177.0, 354.0),
    ("b500", 354.0, 707.0),
    ("b1k", 707.0, 1414.0),
    ("b2k", 1414.0, 2828.0),
    ("b4k", 2828.0, 5657.0),
    ("b8k", 5657.0, 11314.0),
    ("b16k", 11314.0, 24000.0),
)


@dataclass(frozen=True)
class SpectralParams:
    frame_size: int = 1024
    hop: int = 512
    method: str = "wiener"  # "wiener" | "spectral_sub"
    over_subtract: float = 1.5
    gain_floor: float = 0.05


@dataclass(frozen=True)
class GateParams:
    threshold_ratio: float = 3.0  # 门限 = ratio * 谱域处理后残余噪声 RMS
    mode: str = "zero"  # "zero" | "attenuate"
    attenuation: float = 0.1


@dataclass(frozen=True)
class ProtectParams:
    pre: int = 64  # 直达声到达样本之前的保护样本数
    post: int = 256  # 直达声到达样本之后的保护样本数
    amp_tol: float = 0.02  # 保护窗口内峰值幅度声明容差（相对）
    modify_tol: float = 1e-9  # 门限对保护窗口样本的声明修改容差（绝对）


@dataclass(frozen=True)
class NoiseParams:
    guard: int = 32  # 粗检测到到达位置前回退的保护样本
    min_pretrigger: int = 256  # 预触发区间最小样本数
    pretrigger_len: Optional[int] = None  # 固定预触发长度；None 表示用全部可用区间


@dataclass(frozen=True)
class MetricParams:
    signal_window: int = 4096  # 直达声之后用于信号能量估计的样本数


@dataclass(frozen=True)
class ChannelOverride:
    spectral: Optional[SpectralParams] = None
    gate: Optional[GateParams] = None
    protect: Optional[ProtectParams] = None
    bands: Optional[Dict[str, SpectralParams]] = None  # 频带级谱参数覆盖


@dataclass(frozen=True)
class EnhanceConfig:
    sample_rate: int = SAMPLE_RATE
    spectral: SpectralParams = field(default_factory=SpectralParams)
    gate: GateParams = field(default_factory=GateParams)
    protect: ProtectParams = field(default_factory=ProtectParams)
    noise: NoiseParams = field(default_factory=NoiseParams)
    metric: MetricParams = field(default_factory=MetricParams)
    bands: Tuple[Tuple[str, float, float], ...] = DEFAULT_BANDS
    channel_overrides: Tuple[Tuple[int, ChannelOverride], ...] = ()

    def _override(self, channel: int) -> Optional[ChannelOverride]:
        for idx, ov in self.channel_overrides:
            if idx == channel:
                return ov
        return None

    def spectral_for(self, channel: int, band: Optional[str] = None) -> SpectralParams:
        ov = self._override(channel)
        base = ov.spectral if (ov and ov.spectral) else self.spectral
        if band is not None and ov and ov.bands and band in ov.bands:
            return ov.bands[band]
        return base

    def gate_for(self, channel: int) -> GateParams:
        ov = self._override(channel)
        return ov.gate if (ov and ov.gate) else self.gate

    def protect_for(self, channel: int) -> ProtectParams:
        ov = self._override(channel)
        return ov.protect if (ov and ov.protect) else self.protect

    def to_dict(self) -> dict:
        def conv(obj):
            if hasattr(obj, "__dataclass_fields__"):
                return {k: conv(getattr(obj, k)) for k in obj.__dataclass_fields__}
            if isinstance(obj, (tuple, list)):
                return [conv(v) for v in obj]
            if isinstance(obj, dict):
                return {
                    str(k): conv(v)
                    for k, v in sorted(obj.items(), key=lambda kv: str(kv[0]))
                }
            return obj

        return conv(self)

    def content_hash(self) -> str:
        blob = json.dumps(self.to_dict(), sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(blob.encode("utf-8")).hexdigest()

    def channel_hash(self, channel: int) -> str:
        """该声道全部有效处理参数的哈希（谱参数按频带展开）。"""
        payload = {
            "spectral_global": self.spectral_for(channel).__dict__,
            "spectral_bands": {
                name: self.spectral_for(channel, name).__dict__
                for name, _, _ in self.bands
            },
            "gate": self.gate_for(channel).__dict__,
            "protect": self.protect_for(channel).__dict__,
            "noise": self.noise.__dict__,
            "metric": self.metric.__dict__,
            "bands": [list(b) for b in self.bands],
            "sample_rate": self.sample_rate,
        }
        blob = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(blob.encode("utf-8")).hexdigest()

    def with_channel_override(
        self, channel: int, override: ChannelOverride
    ) -> "EnhanceConfig":
        others = tuple((i, o) for i, o in self.channel_overrides if i != channel)
        return replace(self, channel_overrides=others + ((channel, override),))
