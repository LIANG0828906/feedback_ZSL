"""Algorithm parameter validation and stable snapshots."""

from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any

import numpy as np

from .errors import WorkbenchError, require
from .io_utils import value_version


@dataclass(frozen=True)
class WorkbenchParams:
    interpolation_order: int = 3
    exposure_weights: tuple[float, ...] = (0.25, 0.35, 0.40)
    guide_radius: int = 3
    guide_eps: float = 5.0e-4
    contrast_gain: float = 1.35
    contrast_threshold: float = 4.0e-3
    gamut_intent: str = "perceptual"
    gamut_tolerance: float = 1.0e-6
    max_gamut_iterations: int = 8
    segment_size: tuple[int, int] = (32, 32)
    max_workers: int = 2
    max_pixels_per_thread: int = 250_000

    def with_updates(self, **changes: Any) -> "WorkbenchParams":
        return replace(self, **changes)

    def algorithm_snapshot(self) -> dict[str, Any]:
        return {
            "interpolation_order": int(self.interpolation_order),
            "exposure_weights": [float(value) for value in self.exposure_weights],
            "guide_radius": int(self.guide_radius),
            "guide_eps": float(self.guide_eps),
            "contrast_gain": float(self.contrast_gain),
            "contrast_threshold": float(self.contrast_threshold),
            "gamut_intent": str(self.gamut_intent),
            "gamut_tolerance": float(self.gamut_tolerance),
            "max_gamut_iterations": int(self.max_gamut_iterations),
            "segment_size": [int(self.segment_size[0]), int(self.segment_size[1])],
        }

    def version(self) -> str:
        return value_version(self.algorithm_snapshot())

    def validate(self, scene_meta: dict[str, Any] | None = None, frame_count: int | None = None) -> None:
        require(
            isinstance(self.interpolation_order, int) and not isinstance(self.interpolation_order, bool),
            "E_PARA",
            "interpolation_order must be an integer",
            field="interpolation_order",
        )
        max_order = 5
        if scene_meta is not None:
            max_order = min(max_order, int(scene_meta.get("max_interpolation_order", max_order)))
        require(
            0 <= self.interpolation_order <= max_order,
            "E_PARA",
            f"interpolation_order must be in [0, {max_order}]",
            field="interpolation_order",
            value=self.interpolation_order,
            allowed=[0, max_order],
        )
        weights = np.asarray(self.exposure_weights, dtype=np.float64)
        require(weights.ndim == 1, "E_PARA", "exposure_weights must be a one-dimensional list", field="exposure_weights")
        expected_count = frame_count if frame_count is not None else len(weights)
        require(
            weights.size == expected_count,
            "E_PARA",
            "exposure_weights length must equal the exposure frame count",
            field="exposure_weights",
            expected=expected_count,
            actual=int(weights.size),
        )
        require(
            np.all(np.isfinite(weights)),
            "E_PARA",
            "exposure_weights must contain finite values",
            field="exposure_weights",
        )
        require(
            np.all(weights >= 0.0),
            "E_PARA",
            "exposure_weights must be non-negative",
            field="exposure_weights",
        )
        require(
            float(weights.sum()) > 0.0,
            "E_PARA",
            "at least one exposure weight must be positive",
            field="exposure_weights",
        )
        require(
            isinstance(self.guide_radius, int) and not isinstance(self.guide_radius, bool),
            "E_PARA",
            "guide_radius must be an integer",
            field="guide_radius",
        )
        require(1 <= self.guide_radius <= 64, "E_PARA", "guide_radius must be in [1, 64]", field="guide_radius")
        require(
            np.isfinite(self.guide_eps) and 0.0 < self.guide_eps <= 1.0,
            "E_PARA",
            "guide_eps must be finite and in (0, 1]",
            field="guide_eps",
        )
        require(
            np.isfinite(self.contrast_gain) and 0.0 <= self.contrast_gain <= 8.0,
            "E_PARA",
            "contrast_gain must be finite and in [0, 8]",
            field="contrast_gain",
        )
        require(
            np.isfinite(self.contrast_threshold) and 0.0 <= self.contrast_threshold <= 0.5,
            "E_PARA",
            "contrast_threshold must be finite and in [0, 0.5]",
            field="contrast_threshold",
        )
        require(
            self.gamut_intent in {"perceptual", "relative_colorimetric"},
            "E_PARA",
            "gamut_intent must be perceptual or relative_colorimetric",
            field="gamut_intent",
        )
        require(
            np.isfinite(self.gamut_tolerance) and 0.0 <= self.gamut_tolerance <= 1.0e-2,
            "E_PARA",
            "gamut_tolerance must be finite and in [0, 0.01]",
            field="gamut_tolerance",
        )
        require(
            isinstance(self.max_gamut_iterations, int)
            and not isinstance(self.max_gamut_iterations, bool)
            and 1 <= self.max_gamut_iterations <= 100,
            "E_PARA",
            "max_gamut_iterations must be in [1, 100]",
            field="max_gamut_iterations",
        )
        require(
            isinstance(self.segment_size, tuple)
            and len(self.segment_size) == 2
            and all(isinstance(v, int) and not isinstance(v, bool) and v > 0 for v in self.segment_size),
            "E_PARA",
            "segment_size must be a (height, width) tuple of positive integers",
            field="segment_size",
        )
        require(
            isinstance(self.max_workers, int)
            and not isinstance(self.max_workers, bool)
            and 1 <= self.max_workers <= 64,
            "E_PARA",
            "max_workers must be in [1, 64]",
            field="max_workers",
        )
        require(
            isinstance(self.max_pixels_per_thread, int)
            and not isinstance(self.max_pixels_per_thread, bool)
            and self.max_pixels_per_thread > 0,
            "E_PARA",
            "max_pixels_per_thread must be a positive integer",
            field="max_pixels_per_thread",
        )

    @classmethod
    def from_mapping(cls, value: dict[str, Any]) -> "WorkbenchParams":
        known = {
            "interpolation_order",
            "exposure_weights",
            "guide_radius",
            "guide_eps",
            "contrast_gain",
            "contrast_threshold",
            "gamut_intent",
            "gamut_tolerance",
            "max_gamut_iterations",
            "segment_size",
            "max_workers",
            "max_pixels_per_thread",
        }
        unknown = sorted(set(value) - known)
        if unknown:
            raise WorkbenchError("E_PARA", f"Unknown parameter(s): {', '.join(unknown)}", field=unknown[0])
        converted = dict(value)
        if "exposure_weights" in converted:
            converted["exposure_weights"] = tuple(converted["exposure_weights"])
        if "segment_size" in converted:
            converted["segment_size"] = tuple(converted["segment_size"])
        return cls(**converted)

