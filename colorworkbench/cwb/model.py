"""统一的内部颜色模型。

三个不同版本的 ICC 风格配置文件经解析器归一化为本模块的数据结构，
转换管线只依赖内部模型，不直接感知源版本字段命名差异。
"""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any


# 内部统一渲染意图枚举（各版本的字符串/数字枚举先归一到这里）
INTENT_RELATIVE = "relative_colorimetric"
INTENT_ABSOLUTE = "absolute_colorimetric"
INTENT_PERCEPTUAL = "perceptual"
INTENTS = (INTENT_RELATIVE, INTENT_ABSOLUTE, INTENT_PERCEPTUAL)


@dataclass(frozen=True)
class TRC:
    """色调响应曲线（设备编码 <-> 线性光）。

    kind:
        - "parametric": 由曲线类型 + 参数描述（gamma / sRGB 分段）。
        - "table": 由等间距采样表描述，线性化时做表插值反演。
    """

    kind: str
    curve: str = "gamma"          # parametric: gamma | srgb
    gamma: float = 2.2
    table: tuple[float, ...] = ()  # table: 0..1 等间距编码 -> 线性值

    def with_changes(self, **changes: Any) -> "TRC":
        return replace(self, **changes)


@dataclass(frozen=True)
class LUT3D:
    """三维查找表（可选），grid 为 N^3 x 3 的归一化 PCS 输出。"""

    size: int
    grid: tuple[tuple[float, float, float], ...]


@dataclass
class ColorProfile:
    """归一化后的颜色配置文件内部模型。"""

    source_version: str                     # 保留源版本标识
    profile_id: str
    color_space: str                        # rgb | gray
    trc: TRC                                # 设备 TRC（输入侧）
    matrix: tuple[float, ...]               # 长度 9，行主序 device-linear -> PCS
    lut3d: LUT3D | None = None
    white_point: tuple[float, float, float] | None = None
    # 白点缺省时配置文件声明的默认值（不允许管线内部猜测）
    declared_default_white_point: tuple[float, float, float] | None = None
    default_intent: str = INTENT_RELATIVE
    intent_map: dict[str, str] = field(default_factory=dict)
    # 原始标签名 -> 归一化字段名，便于越界时报出源标签名
    label_map: dict[str, str] = field(default_factory=dict)

    def clone(self) -> "ColorProfile":
        return ColorProfile(
            source_version=self.source_version,
            profile_id=self.profile_id,
            color_space=self.color_space,
            trc=self.trc,
            matrix=tuple(self.matrix),
            lut3d=self.lut3d,
            white_point=tuple(self.white_point) if self.white_point else None,
            declared_default_white_point=(
                tuple(self.declared_default_white_point)
                if self.declared_default_white_point else None
            ),
            default_intent=self.default_intent,
            intent_map=dict(self.intent_map),
            label_map=dict(self.label_map),
        )


@dataclass
class TransformConfig:
    """一次完整转换使用的源/目标配置与输出位深、渲染意图。"""

    source: ColorProfile
    target: ColorProfile
    intent: str
    output_depth: int
    batch_rows: int = 8


@dataclass
class TraceRecord:
    """转换痕迹（白点缺省取值等关键决策可审计）。"""

    stage: str
    message: str
    data: dict[str, Any] = field(default_factory=dict)
