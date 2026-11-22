from __future__ import annotations

from dataclasses import asdict, dataclass
import hashlib
import json
import math
from typing import Any

from .errors import ParameterError
from .stages import Stage


ALGORITHM_VERSION = "edgeforge.pipeline.v1"


@dataclass(frozen=True)
class PipelineConfig:
    gradient_operator: str = "sobel"
    low_threshold: float = 8.0
    high_threshold: float = 18.0
    open_size: int = 3
    close_size: int = 3
    thin_iterations: int = 8
    prune_length: int = 6
    structure: str = "cross"
    tile_size: int = 192
    max_parallel: int = 4
    thread_buffer_pixels: int = 1_500_000
    memory_budget_bytes: int = 64 * 1024 * 1024
    band_override: int | None = None

    def validate(self) -> "PipelineConfig":
        if self.gradient_operator not in {"sobel", "prewitt", "scharr"}:
            self._para(
                "gradient_operator",
                "梯度算子必须是 sobel、prewitt 或 scharr",
            )
        for name, value, lo, hi in (
            ("low_threshold", self.low_threshold, 0.0, 255.0),
            ("high_threshold", self.high_threshold, 0.0, 255.0),
        ):
            if not isinstance(value, (int, float)) or not math.isfinite(float(value)):
                self._para(name, "阈值必须是有限数值")
            if float(value) < lo or float(value) > hi:
                self._para(name, f"阈值必须位于 [{lo:g}, {hi:g}]")
        if float(self.low_threshold) > float(self.high_threshold):
            self._para(
                "low_threshold",
                "低阈值不得大于高阈值",
                details={
                    "low_threshold": self.low_threshold,
                    "high_threshold": self.high_threshold,
                },
            )
        for name, value in (
            ("open_size", self.open_size),
            ("close_size", self.close_size),
        ):
            if not isinstance(value, int) or value < 1 or value > 31 or value % 2 == 0:
                self._para(name, "结构元素尺寸必须是 1..31 的奇数")
        if self.structure not in {"cross", "square"}:
            self._para("structure", "结构元素必须是 cross 或 square")
        if (
            not isinstance(self.thin_iterations, int)
            or self.thin_iterations < 1
            or self.thin_iterations > 64
        ):
            self._para("thin_iterations", "细化迭代次数必须位于 1..64")
        if (
            not isinstance(self.prune_length, int)
            or self.prune_length < 1
            or self.prune_length > 256
        ):
            self._para("prune_length", "剪枝长度阈值必须位于 1..256")
        if (
            not isinstance(self.tile_size, int)
            or self.tile_size < 16
            or self.tile_size > 4096
        ):
            self._para("tile_size", "分块尺寸必须位于 16..4096")
        if (
            not isinstance(self.max_parallel, int)
            or self.max_parallel < 1
            or self.max_parallel > 64
        ):
            self._para("max_parallel", "最大并行度必须位于 1..64")
        if (
            not isinstance(self.thread_buffer_pixels, int)
            or self.thread_buffer_pixels < 4096
        ):
            self._para("thread_buffer_pixels", "每线程像素缓冲上限不得小于 4096")
        if (
            not isinstance(self.memory_budget_bytes, int)
            or self.memory_budget_bytes < 64 * 1024
        ):
            self._para("memory_budget_bytes", "边带内存预算不得小于 64 KiB")
        if self.band_override is not None and (
            not isinstance(self.band_override, int)
            or self.band_override < 0
            or self.band_override > 1024
        ):
            self._para("band_override", "边带覆盖值必须位于 0..1024")
        return self

    @staticmethod
    def _para(
        field_name: str,
        message: str,
        *,
        details: dict[str, Any] | None = None,
    ) -> None:
        raise ParameterError(message, field=field_name, details=details)

    def required_band(self, stage: Stage) -> int:
        if stage in {Stage.GRADIENT, Stage.THRESHOLD}:
            return 1
        if stage is Stage.OPEN:
            return 2 * (self.open_size // 2)
        if stage is Stage.CLOSE:
            return 2 * (self.close_size // 2)
        if stage is Stage.THIN:
            return self.thin_iterations + 1
        if stage is Stage.PRUNE:
            return self.prune_length + 1
        return 1

    def effective_band(self, stage: Stage) -> int:
        required = self.required_band(stage)
        if self.band_override is None:
            return required
        return self.band_override

    def params_payload(self) -> dict[str, Any]:
        payload = asdict(self)
        for key in (
            "max_parallel",
            "thread_buffer_pixels",
            "memory_budget_bytes",
        ):
            payload.pop(key, None)
        return payload

    def params_version(self) -> str:
        raw = json.dumps(
            self.params_payload(),
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        ).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()

    def describe(self) -> dict[str, Any]:
        return {
            "gradient_operator": self.gradient_operator,
            "low_threshold": float(self.low_threshold),
            "high_threshold": float(self.high_threshold),
            "open_size": self.open_size,
            "close_size": self.close_size,
            "thin_iterations": self.thin_iterations,
            "prune_length": self.prune_length,
            "structure": self.structure,
            "tile_size": self.tile_size,
            "max_parallel": self.max_parallel,
            "thread_buffer_pixels": self.thread_buffer_pixels,
            "memory_budget_bytes": self.memory_budget_bytes,
            "band_override": self.band_override,
            "params_version": self.params_version(),
            "algorithm_version": ALGORITHM_VERSION,
        }
