from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import para_error


def _as_int(value: Any, field_name: str) -> int:
    if isinstance(value, bool):
        raise para_error("boolean is not a valid integer parameter", field_name)
    try:
        integer = int(value)
    except (TypeError, ValueError) as exc:
        raise para_error("parameter must be an integer", field_name) from exc
    if float(value) != float(integer):
        raise para_error("parameter must be an integer", field_name)
    return integer


def _as_float(value: Any, field_name: str) -> float:
    if isinstance(value, bool):
        raise para_error("boolean is not a valid numeric parameter", field_name)
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise para_error("parameter must be numeric", field_name) from exc
    if result != result or result in (float("inf"), float("-inf")):
        raise para_error("parameter must be finite", field_name)
    return result


@dataclass(frozen=True)
class AnalysisConfig:
    frame_len: int = 2048
    hop: int = 1024
    window_len: int = 768
    interpolation_order: int = 2
    energy_threshold_db: float = -80.0
    direct_half_width: int = 10
    clip_ceiling: float = 1.5

    def validate(self) -> None:
        frame_len = _as_int(self.frame_len, "frame_len")
        hop = _as_int(self.hop, "hop")
        window_len = _as_int(self.window_len, "window_len")
        order = _as_int(self.interpolation_order, "interpolation_order")
        direct_half = _as_int(self.direct_half_width, "direct_half_width")
        threshold = _as_float(self.energy_threshold_db, "energy_threshold_db")
        ceiling = _as_float(self.clip_ceiling, "clip_ceiling")

        if not 64 <= frame_len <= 1_048_576:
            raise para_error("frame_len must be in [64, 1048576]", "frame_len")
        if not 32 <= hop <= frame_len:
            raise para_error("hop must be in [32, frame_len]", "hop")
        if not 64 <= window_len <= hop:
            raise para_error("window_len must be in [64, hop]", "window_len")
        if not 1 <= order <= 8:
            raise para_error("interpolation_order must be in [1, 8]", "interpolation_order")
        if not 2 <= direct_half <= window_len // 4:
            raise para_error(
                "direct_half_width must be in [2, window_len/4]",
                "direct_half_width",
            )
        if not -160.0 <= threshold <= 0.0:
            raise para_error(
                "energy_threshold_db must be in [-160, 0]",
                "energy_threshold_db",
            )
        if not 0.0 < ceiling <= 32.0:
            raise para_error("clip_ceiling must be in (0, 32]", "clip_ceiling")
        if window_len >= hop:
            raise para_error(
                "window_len must be smaller than hop to leave a late-energy region",
                "window_len",
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_len": int(self.frame_len),
            "hop": int(self.hop),
            "window_len": int(self.window_len),
            "interpolation_order": int(self.interpolation_order),
            "energy_threshold_db": float(self.energy_threshold_db),
            "direct_half_width": int(self.direct_half_width),
            "clip_ceiling": float(self.clip_ceiling),
        }


@dataclass(frozen=True)
class RenderConfig:
    conv_window_len: int = 1024
    block_size: int = 512
    interpolation_order: int = 2
    channel_map: tuple[int, ...] | None = None

    def validate(self, analysis_order: int = 2) -> None:
        conv_window = _as_int(self.conv_window_len, "conv_window_len")
        block = _as_int(self.block_size, "block_size")
        order = _as_int(self.interpolation_order, "interpolation_order")
        if not 16 <= conv_window <= 1_048_576:
            raise para_error(
                "conv_window_len must be in [16, 1048576]",
                "conv_window_len",
            )
        if not 16 <= block <= conv_window:
            raise para_error(
                "block_size must be in [16, conv_window_len]",
                "block_size",
            )
        if not 1 <= order <= 8:
            raise para_error("interpolation_order must be in [1, 8]", "interpolation_order")
        if order != analysis_order:
            raise para_error(
                "render and analysis interpolation_order must agree",
                "interpolation_order",
            )
        if self.channel_map is not None:
            try:
                values = tuple(int(value) for value in self.channel_map)
            except (TypeError, ValueError) as exc:
                raise para_error("channel_map must contain integers", "channel_map") from exc
            for value in values:
                if value < 0:
                    raise para_error(
                        "channel_map indices must be non-negative",
                        "channel_map",
                    )

    def to_dict(self) -> dict[str, Any]:
        return {
            "conv_window_len": int(self.conv_window_len),
            "block_size": int(self.block_size),
            "interpolation_order": int(self.interpolation_order),
            "channel_map": (
                None if self.channel_map is None else [int(v) for v in self.channel_map]
            ),
        }


@dataclass(frozen=True)
class WorkbenchConfig:
    analysis: AnalysisConfig = field(default_factory=AnalysisConfig)
    render: RenderConfig = field(default_factory=RenderConfig)
    max_workers: int = 2
    per_thread_buffer_samples: int = 2_000_000
    algorithm_version: str = "spatial-workbench-1.0.0"

    def validate(self) -> None:
        self.analysis.validate()
        self.render.validate(self.analysis.interpolation_order)
        workers = _as_int(self.max_workers, "max_workers")
        buffer_samples = _as_int(
            self.per_thread_buffer_samples,
            "per_thread_buffer_samples",
        )
        if not 1 <= workers <= 8:
            raise para_error("max_workers must be in [1, 8]", "max_workers")
        if not 4096 <= buffer_samples <= 100_000_000:
            raise para_error(
                "per_thread_buffer_samples must be in [4096, 100000000]",
                "per_thread_buffer_samples",
            )
        required = self.analysis.frame_len * 8
        if required > buffer_samples:
            raise para_error(
                "frame requires more samples than per_thread_buffer_samples",
                "per_thread_buffer_samples",
                details={"required_samples": required, "limit": buffer_samples},
            )
        if not isinstance(self.algorithm_version, str) or not self.algorithm_version:
            raise para_error(
                "algorithm_version must be a non-empty string",
                "algorithm_version",
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "analysis": self.analysis.to_dict(),
            "render": self.render.to_dict(),
            "max_workers": int(self.max_workers),
            "per_thread_buffer_samples": int(self.per_thread_buffer_samples),
            "algorithm_version": self.algorithm_version,
        }
