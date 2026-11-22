from __future__ import annotations

from dataclasses import asdict, dataclass, field
from typing import Any

from .errors import E_PARA, SyncError


ALGORITHM_NAME = "pwl-windowed-sinc"
ALGORITHM_VERSION = "1.0.0"
CHECKPOINT_SCHEMA_VERSION = 1


@dataclass(frozen=True)
class AlgorithmInfo:
    name: str = ALGORITHM_NAME
    version: str = ALGORITHM_VERSION
    checkpoint_schema: int = CHECKPOINT_SCHEMA_VERSION


@dataclass
class AlignmentConfig:
    interpolation_order: int = 7
    window_half_width: int | None = None
    segment_length: int = 64_000
    tolerance_samples: float = 0.25
    max_drift_ppm: float = 250.0
    max_offset_samples: int = 64
    max_workers: int = 2
    max_buffer_bytes: int = 4 * 1024 * 1024
    marker_uncertainty_samples: float = 0.025
    confidence_limit_samples: float = 0.20
    confidence_limit_ppm: float = 5.0
    residual_bound_margin: float = 0.02
    preserve_committed: bool = True
    output_dtype: str = "float32"
    layout: str = "quad"
    extra: dict[str, Any] = field(default_factory=dict)

    def validate(self) -> None:
        if isinstance(self.interpolation_order, bool) or not 1 <= int(self.interpolation_order) <= 15:
            raise SyncError(E_PARA, "interpolation_order must be in [1, 15]", field="interpolation_order")
        if int(self.interpolation_order) != self.interpolation_order:
            raise SyncError(E_PARA, "interpolation_order must be an integer", field="interpolation_order")
        if self.window_half_width is not None:
            if isinstance(self.window_half_width, bool) or not 2 <= int(self.window_half_width) <= 32:
                raise SyncError(E_PARA, "window_half_width must be in [2, 32]", field="window_half_width")
            if int(self.window_half_width) != self.window_half_width:
                raise SyncError(E_PARA, "window_half_width must be an integer", field="window_half_width")
        if isinstance(self.segment_length, bool) or int(self.segment_length) < 256:
            raise SyncError(E_PARA, "segment_length must be >= 256", field="segment_length")
        if int(self.segment_length) != self.segment_length:
            raise SyncError(E_PARA, "segment_length must be an integer", field="segment_length")
        if not 0.0 < float(self.tolerance_samples) <= 2.0:
            raise SyncError(E_PARA, "tolerance_samples must be in (0, 2]", field="tolerance_samples")
        if not 0.0 < float(self.max_drift_ppm) <= 10_000.0:
            raise SyncError(E_PARA, "max_drift_ppm must be in (0, 10000]", field="max_drift_ppm")
        if isinstance(self.max_offset_samples, bool) or int(self.max_offset_samples) < 2:
            raise SyncError(E_PARA, "max_offset_samples must be >= 2", field="max_offset_samples")
        if int(self.max_workers) < 1:
            raise SyncError(E_PARA, "max_workers must be >= 1", field="max_workers")
        if int(self.max_buffer_bytes) < 4096:
            raise SyncError(E_PARA, "max_buffer_bytes must be >= 4096", field="max_buffer_bytes")
        if not 0.0 < float(self.marker_uncertainty_samples) <= 1.0:
            raise SyncError(
                E_PARA,
                "marker_uncertainty_samples must be in (0, 1]",
                field="marker_uncertainty_samples",
            )
        if not 0.0 < float(self.confidence_limit_samples) <= 10.0:
            raise SyncError(
                E_PARA,
                "confidence_limit_samples must be in (0, 10]",
                field="confidence_limit_samples",
            )
        if not 0.0 < float(self.confidence_limit_ppm) <= 1_000.0:
            raise SyncError(
                E_PARA,
                "confidence_limit_ppm must be in (0, 1000]",
                field="confidence_limit_ppm",
            )
        if not 0.0 <= float(self.residual_bound_margin) <= 1.0:
            raise SyncError(
                E_PARA,
                "residual_bound_margin must be in [0, 1]",
                field="residual_bound_margin",
            )
        if self.output_dtype not in {"float32", "float64"}:
            raise SyncError(E_PARA, "output_dtype must be float32 or float64", field="output_dtype")
        if not self.layout:
            raise SyncError(E_PARA, "layout must not be empty", field="layout")

    @property
    def interpolation_half_width(self) -> int:
        if self.window_half_width is not None:
            return int(self.window_half_width)
        return (int(self.interpolation_order) + 1) // 2

    def parameter_version(self) -> str:
        import hashlib
        import json

        payload = asdict(self)
        text = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
        return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

