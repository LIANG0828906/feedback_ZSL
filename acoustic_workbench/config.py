"""Validated analysis and parallelism configuration."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .codes import E_PARA
from .errors import error
from .models import TolerancePolicy

ALGORITHM_VERSION = "acoustic-workbench/1.0.0"


@dataclass(frozen=True)
class AnalysisConfig:
    frame_length: int = 4096
    hop_length: int = 1024
    estimation_window: int = 2048
    early_window_ms: float = 20.0
    mode: str = "auto"
    auto_tradeoff: bool = True
    max_parallelism: int = 2
    thread_buffer_mb: int = 16
    tolerance: TolerancePolicy = TolerancePolicy()

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_length": self.frame_length,
            "hop_length": self.hop_length,
            "estimation_window": self.estimation_window,
            "early_window_ms": self.early_window_ms,
            "mode": self.mode,
            "auto_tradeoff": self.auto_tradeoff,
            "max_parallelism": self.max_parallelism,
            "thread_buffer_mb": self.thread_buffer_mb,
            "tolerance": self.tolerance.to_dict(),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AnalysisConfig":
        tolerance_data = data.get("tolerance", {})
        return cls(
            frame_length=int(data.get("frame_length", 4096)),
            hop_length=int(data.get("hop_length", 1024)),
            estimation_window=int(data.get("estimation_window", 2048)),
            early_window_ms=float(data.get("early_window_ms", 20.0)),
            mode=str(data.get("mode", "auto")),
            auto_tradeoff=bool(data.get("auto_tradeoff", True)),
            max_parallelism=int(data.get("max_parallelism", 2)),
            thread_buffer_mb=int(data.get("thread_buffer_mb", 16)),
            tolerance=TolerancePolicy(
                delay_samples=float(tolerance_data.get("delay_samples", 1.0)),
                energy_relative=float(tolerance_data.get("energy_relative", 0.08)),
                drr_db=float(tolerance_data.get("drr_db", 0.75)),
            ),
        )

    def with_mode(self, mode: str) -> "AnalysisConfig":
        return AnalysisConfig(
            frame_length=self.frame_length,
            hop_length=self.hop_length,
            estimation_window=self.estimation_window,
            early_window_ms=self.early_window_ms,
            mode=mode,
            auto_tradeoff=self.auto_tradeoff,
            max_parallelism=self.max_parallelism,
            thread_buffer_mb=self.thread_buffer_mb,
            tolerance=self.tolerance,
        )


def _para(field: str, message: str, value: Any) -> Exception:
    return error(
        E_PARA,
        message,
        stage="config.validate",
        field=field,
        context={"value": value},
    )


def validate_analysis_config(config: AnalysisConfig) -> None:
    if config.mode not in {"auto", "calibration", "preview"}:
        raise _para("mode", "mode must be auto, calibration, or preview", config.mode)
    if not isinstance(config.frame_length, int) or not 64 <= config.frame_length <= 65536:
        raise _para("frame_length", "frame_length must be an integer in [64, 65536]", config.frame_length)
    if not isinstance(config.hop_length, int) or not 16 <= config.hop_length <= config.frame_length:
        raise _para("hop_length", "hop_length must be in [16, frame_length]", config.hop_length)
    if (
        not isinstance(config.estimation_window, int)
        or not 16 <= config.estimation_window <= config.frame_length
    ):
        raise _para(
            "estimation_window",
            "estimation_window must be in [16, frame_length]",
            config.estimation_window,
        )
    if config.early_window_ms <= 0.0 or config.early_window_ms > 5000.0:
        raise _para(
            "early_window_ms",
            "early_window_ms must be in (0, 5000]",
            config.early_window_ms,
        )
    if not isinstance(config.max_parallelism, int) or not 1 <= config.max_parallelism <= 16:
        raise _para(
            "max_parallelism",
            "max_parallelism must be an integer in [1, 16]",
            config.max_parallelism,
        )
    if not isinstance(config.thread_buffer_mb, int) or not 1 <= config.thread_buffer_mb <= 1024:
        raise _para(
            "thread_buffer_mb",
            "thread_buffer_mb must be an integer in [1, 1024]",
            config.thread_buffer_mb,
        )
    tolerance = config.tolerance
    if tolerance.delay_samples < 0.0:
        raise _para("tolerance.delay_samples", "delay tolerance cannot be negative", tolerance.delay_samples)
    if not 0.0 <= tolerance.energy_relative <= 10.0:
        raise _para(
            "tolerance.energy_relative",
            "energy tolerance must be in [0, 10]",
            tolerance.energy_relative,
        )
    if tolerance.drr_db < 0.0:
        raise _para("tolerance.drr_db", "DRR tolerance cannot be negative", tolerance.drr_db)


def frame_count(sample_count: int, frame_length: int, hop_length: int) -> int:
    if sample_count <= 0:
        return 0
    return max(1, (sample_count + hop_length - 1) // hop_length)
