"""分割参数模型与确定性校验。"""
from __future__ import annotations

from dataclasses import asdict, dataclass, field
from typing import Any, Dict, Optional, Tuple

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.core.versioning import PARAM_SCHEMA_VERSION


@dataclass(frozen=True)
class SegmentationParameters:
    """区域生长与后处理参数。

    :param color_tol: 欧氏颜色距离阈值（0..441.67，RGB 立方体对角线）。
    :param gradient_tol: 邻域颜色差阈值，超过则禁止跨越该边。
    :param struct_radius: 形态学方块结构元素半径（>=0）。
    :param min_region_pixels: 有效区域最小像素数。
    :param seed_points: 可选的显式种子点 ``{block_id: (x, y)}``。
    :param injections: 注入配置，键为注入类型，值为注入载荷。
    :param spill_area_ratio: 外溢判定面积比上限。
    """

    color_tol: float = 42.0
    gradient_tol: float = 70.0
    struct_radius: int = 1
    min_region_pixels: int = 200
    spill_area_ratio: float = 1.35
    seed_points: Optional[Dict[str, Tuple[int, int]]] = field(default=None)
    injections: Dict[str, Any] = field(default_factory=dict)
    param_version: str = PARAM_SCHEMA_VERSION

    def validate(self) -> None:
        if not (0.0 < float(self.color_tol) <= 442.0):
            raise fail(ErrorCode.E_PARA, "color_tol 越界", field="color_tol", value=self.color_tol)
        if not (0.0 < float(self.gradient_tol) <= 765.0):
            raise fail(ErrorCode.E_PARA, "gradient_tol 越界", field="gradient_tol", value=self.gradient_tol)
        if int(self.struct_radius) < 0 or int(self.struct_radius) > 32:
            raise fail(ErrorCode.E_PARA, "struct_radius 越界", field="struct_radius", value=self.struct_radius)
        if int(self.min_region_pixels) <= 0:
            raise fail(ErrorCode.E_PARA, "min_region_pixels 必须为正", field="min_region_pixels",
                       value=self.min_region_pixels)
        if not (1.0 < float(self.spill_area_ratio) <= 10.0):
            raise fail(ErrorCode.E_PARA, "spill_area_ratio 越界", field="spill_area_ratio",
                       value=self.spill_area_ratio)
        if self.seed_points:
            for block_id, point in self.seed_points.items():
                if (not isinstance(point, (tuple, list))) or len(point) != 2:
                    raise fail(ErrorCode.E_PARA, "种子点必须为 (x, y)", field=f"seed_points.{block_id}")
                x, y = point
                if int(x) < 0 or int(y) < 0:
                    raise fail(ErrorCode.E_PARA, "种子点坐标不能为负", field=f"seed_points.{block_id}",
                               value=list(point))

    def to_dict(self) -> Dict[str, Any]:
        payload = asdict(self)
        if self.seed_points:
            payload["seed_points"] = {key: list(value) for key, value in self.seed_points.items()}
        return payload

    @classmethod
    def from_dict(cls, payload: Dict[str, Any]) -> "SegmentationParameters":
        seed_points = payload.get("seed_points")
        if seed_points:
            seed_points = {key: tuple(value) for key, value in seed_points.items()}
        return cls(
            color_tol=float(payload["color_tol"]),
            gradient_tol=float(payload["gradient_tol"]),
            struct_radius=int(payload["struct_radius"]),
            min_region_pixels=int(payload["min_region_pixels"]),
            spill_area_ratio=float(payload.get("spill_area_ratio", 1.35)),
            seed_points=seed_points,
            injections=dict(payload.get("injections", {})),
            param_version=payload.get("param_version", PARAM_SCHEMA_VERSION),
        )
