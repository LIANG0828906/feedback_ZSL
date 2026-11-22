"""Pipeline configuration and strict parameter validation."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass, fields
from typing import Any, Dict, Mapping, Optional, Tuple

import numpy as np

from .errors import EdgePipeError, ErrorCode

ALGORITHM_VERSION = "edgepipe-1.0.0"

STAGES: Tuple[str, ...] = ("gradient", "open", "close", "thin", "prune")
STAGE_LABELS = {
    "gradient": "梯度/双阈值",
    "open": "开运算",
    "close": "闭运算",
    "thin": "细化",
    "prune": "短线段剪枝",
}
GRADIENT_OPERATORS = ("sobel", "scharr", "prewitt")


@dataclass(frozen=True)
class PipelineConfig:
    """All parameters that can affect the produced masks.

    Runtime-only limits (``max_workers`` and ``thread_buffer_bytes``) are kept
    here so reports and checkpoint invalidation are explicit, although they do
    not change pixel output.
    """

    gradient_operator: str = "sobel"
    low_threshold: float = 12.0
    high_threshold: float = 30.0
    open_enabled: bool = True
    open_kernel_size: int = 3
    open_iterations: int = 1
    close_enabled: bool = True
    close_kernel_size: int = 3
    close_iterations: int = 1
    thin_enabled: bool = True
    thinning_iterations: int = 2
    prune_enabled: bool = True
    prune_length: int = 6
    block_size: int = 48
    band_width: Optional[int] = None
    max_workers: int = 4
    thread_buffer_bytes: int = 64 * 1024 * 1024

    def __post_init__(self) -> None:
        self.validate()

    @classmethod
    def from_mapping(cls, values: Mapping[str, Any]) -> "PipelineConfig":
        allowed = {item.name for item in fields(cls)}
        unknown = sorted(set(values) - allowed)
        if unknown:
            raise EdgePipeError(
                ErrorCode.PARA,
                f"未知参数: {', '.join(unknown)}",
                field=unknown[0],
            )
        return cls(**dict(values))

    def to_dict(self) -> Dict[str, Any]:
        result = asdict(self)
        result["required_band_width"] = self.required_band_width
        result["effective_band_width"] = self.effective_band_width
        return result

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), sort_keys=True, ensure_ascii=False)

    @property
    def channel_count(self) -> int:
        return 0

    @property
    def max_kernel_radius(self) -> int:
        open_radius = (self.open_kernel_size // 2) * self.open_iterations
        close_radius = (self.close_kernel_size // 2) * self.close_iterations
        return max(open_radius, close_radius)

    @property
    def required_band_width(self) -> int:
        """Halo needed to make per-stage chunk results equal whole-image work.

        Zhang-Suen thinning propagates one pixel per iteration. Short-component
        pruning needs the whole component visible, so its maximum diameter is
        the other propagation bound. One extra deterministic safety pixel also
        covers the local dual-threshold hysteresis support.
        """

        thin_radius = self.thinning_iterations if self.thin_enabled else 0
        prune_radius = self.prune_length if self.prune_enabled else 0
        return max(1, self.max_kernel_radius, thin_radius, prune_radius) + 1

    @property
    def effective_band_width(self) -> int:
        if self.band_width is None:
            return self.required_band_width
        return self.band_width

    @property
    def stage_buffer_side(self) -> int:
        return self.block_size + 2 * self.effective_band_width

    @property
    def estimated_thread_buffer_bytes(self, ) -> int:
        """Conservative working-set estimate for one 3-channel block."""

        side = self.stage_buffer_side
        return side * side * 3 * 10 * 8

    @property
    def params_version(self) -> str:
        payload = self.to_dict()
        payload.pop("required_band_width", None)
        payload.pop("effective_band_width", None)
        raw = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()

    def validate(self) -> None:
        if self.gradient_operator not in GRADIENT_OPERATORS:
            raise EdgePipeError(
                ErrorCode.PARA,
                f"gradient_operator 必须为 {GRADIENT_OPERATORS} 之一",
                field="gradient_operator",
                details={"allowed": list(GRADIENT_OPERATORS)},
            )

        low = _finite_number(self.low_threshold, "low_threshold")
        high = _finite_number(self.high_threshold, "high_threshold")
        if not 0.0 <= low <= 4096.0:
            raise EdgePipeError(
                ErrorCode.PARA,
                "low_threshold 必须在 [0, 4096] 范围内",
                field="low_threshold",
                details={"value": low},
            )
        if not 0.0 <= high <= 4096.0:
            raise EdgePipeError(
                ErrorCode.PARA,
                "high_threshold 必须在 [0, 4096] 范围内",
                field="high_threshold",
                details={"value": high},
            )
        if low > high:
            raise EdgePipeError(
                ErrorCode.PARA,
                "low_threshold 不得大于 high_threshold",
                field="low_threshold",
                details={"low_threshold": low, "high_threshold": high},
            )

        _boolean(self.open_enabled, "open_enabled")
        _boolean(self.close_enabled, "close_enabled")
        _boolean(self.thin_enabled, "thin_enabled")
        _boolean(self.prune_enabled, "prune_enabled")

        _odd_int(self.open_kernel_size, "open_kernel_size", 1, 31)
        _odd_int(self.close_kernel_size, "close_kernel_size", 1, 31)
        _int(self.open_iterations, "open_iterations", 0, 8)
        _int(self.close_iterations, "close_iterations", 0, 8)
        _int(self.thinning_iterations, "thinning_iterations", 0, 16)
        _int(self.prune_length, "prune_length", 0, 128)
        _int(self.block_size, "block_size", 16, 512)
        _int(self.max_workers, "max_workers", 1, 4)
        _int(self.thread_buffer_bytes, "thread_buffer_bytes", 1024 * 1024, 1024**3)

        if self.band_width is not None:
            _int(self.band_width, "band_width", 0, 512)
            if self.band_width < self.required_band_width:
                raise EdgePipeError(
                    ErrorCode.BAND,
                    (
                        f"band_width={self.band_width} 小于确定性边界所需值 "
                        f"{self.required_band_width}"
                    ),
                    field="band_width",
                    details={
                        "provided": self.band_width,
                        "required": self.required_band_width,
                    },
                )

        estimate = self.estimated_thread_buffer_bytes
        if estimate > self.thread_buffer_bytes:
            raise EdgePipeError(
                ErrorCode.PARA,
                "分块处理工作集超过 thread_buffer_bytes 上限",
                field="thread_buffer_bytes",
                details={
                    "estimated_bytes": estimate,
                    "limit_bytes": self.thread_buffer_bytes,
                },
            )

    def validate_image(self, image: np.ndarray) -> None:
        if image is None:
            raise EdgePipeError(ErrorCode.PARA, "图像不能为空", field="image")
        if image.dtype != np.uint8:
            raise EdgePipeError(
                ErrorCode.PARA,
                "图像 dtype 必须为 uint8",
                field="image.dtype",
                details={"dtype": str(image.dtype)},
            )
        if image.ndim == 2:
            return
        if image.ndim == 3 and image.shape[2] in (1, 3):
            return
        raise EdgePipeError(
            ErrorCode.PARA,
            "图像必须是 HxW 单通道或 HxWx1/HxWx3 三通道",
            field="image.shape",
            details={"shape": list(image.shape)},
        )


def _finite_number(value: Any, field_name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须为有限数值",
            field=field_name,
            details={"value": repr(value)},
        )
    result = float(value)
    if not math.isfinite(result):
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须为有限数值",
            field=field_name,
            details={"value": repr(value)},
        )
    return result


def _boolean(value: Any, field_name: str) -> None:
    if not isinstance(value, bool):
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须为 bool",
            field=field_name,
            details={"value": repr(value)},
        )


def _int(value: Any, field_name: str, minimum: int, maximum: int) -> None:
    if isinstance(value, bool) or not isinstance(value, int):
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须为整数",
            field=field_name,
            details={"value": repr(value)},
        )
    if not minimum <= value <= maximum:
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须在 [{minimum}, {maximum}] 范围内",
            field=field_name,
            details={"value": value, "minimum": minimum, "maximum": maximum},
        )


def _odd_int(value: Any, field_name: str, minimum: int, maximum: int) -> None:
    _int(value, field_name, minimum, maximum)
    if value % 2 == 0:
        raise EdgePipeError(
            ErrorCode.PARA,
            f"{field_name} 必须为奇数",
            field=field_name,
            details={"value": value},
        )
