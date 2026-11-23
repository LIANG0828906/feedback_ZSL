"""Configuration objects and strict parameter validation."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass, field
from typing import Any

from .errors import ErrorCode, WorkbenchError, parameter_error


ALGORITHM_VERSION = "pw-1.0.0"


def _number(
    name: str,
    value: float,
    low: float,
    high: float,
    *,
    integer: bool = False,
    low_inclusive: bool = True,
) -> None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise parameter_error(name, f"{name} must be numeric")
    if not math.isfinite(float(value)):
        raise parameter_error(name, f"{name} must be finite")
    if integer and int(value) != value:
        raise parameter_error(name, f"{name} must be an integer")
    below = value < low if low_inclusive else value <= low
    if below or value > high:
        suffix = "[" if low_inclusive else "("
        raise parameter_error(
            name,
            f"{name} must be in {suffix}{low}, {high}]",
            value=value,
            minimum=low,
            maximum=high,
        )


@dataclass(frozen=True)
class FeatureConfig:
    max_features: int = 5000
    octave_layers: int = 4
    contrast_threshold: float = 0.012
    edge_threshold: float = 12.0
    ratio_test: float = 0.76
    min_matches: int = 12

    def validate(self) -> "FeatureConfig":
        _number("features.max_features", self.max_features, 32, 50000, integer=True)
        _number("features.octave_layers", self.octave_layers, 1, 8, integer=True)
        _number(
            "features.contrast_threshold",
            self.contrast_threshold,
            1e-5,
            0.5,
            low_inclusive=False,
        )
        _number("features.edge_threshold", self.edge_threshold, 1.0, 100.0)
        _number(
            "features.ratio_test",
            self.ratio_test,
            0.40,
            0.99,
            low_inclusive=False,
        )
        _number("features.min_matches", self.min_matches, 4, 5000, integer=True)
        return self

    def with_budget(self, budget: int) -> "FeatureConfig":
        budget = min(int(budget), self.max_features)
        budget = max(32, budget)
        return FeatureConfig(
            max_features=budget,
            octave_layers=self.octave_layers,
            contrast_threshold=self.contrast_threshold,
            edge_threshold=self.edge_threshold,
            ratio_test=self.ratio_test,
            min_matches=self.min_matches,
        ).validate()


@dataclass(frozen=True)
class HomographyConfig:
    reproj_threshold: float = 3.0
    consensus_threshold: float = 5.0
    max_iterations: int = 2400
    min_inliers: int = 10
    confidence: float = 0.995
    truth_corner_tolerance: float = 2.5
    covariance_regularization: float = 1e-9

    def validate(self) -> "HomographyConfig":
        _number(
            "homography.reproj_threshold",
            self.reproj_threshold,
            0.05,
            50.0,
            low_inclusive=False,
        )
        _number(
            "homography.consensus_threshold",
            self.consensus_threshold,
            0.05,
            50.0,
            low_inclusive=False,
        )
        _number(
            "homography.max_iterations",
            self.max_iterations,
            50,
            200000,
            integer=True,
        )
        _number(
            "homography.min_inliers",
            self.min_inliers,
            4,
            100000,
            integer=True,
        )
        _number(
            "homography.confidence",
            self.confidence,
            0.5,
            0.999999,
            low_inclusive=False,
        )
        _number(
            "homography.truth_corner_tolerance",
            self.truth_corner_tolerance,
            0.01,
            100.0,
            low_inclusive=False,
        )
        _number(
            "homography.covariance_regularization",
            self.covariance_regularization,
            0.0,
            1.0,
        )
        return self


@dataclass(frozen=True)
class StrategyConfig:
    mode: str = "auto"
    small_max_images: int = 8
    work_unit_threshold_mp: float = 4.5
    high_feature_budget: int = 6000
    low_feature_budget: int = 900
    restricted_neighbor_links: int = 2
    max_restricted_pairs: int = 96

    def validate(self) -> "StrategyConfig":
        if self.mode not in {"auto", "full", "restricted"}:
            raise parameter_error(
                "strategy.mode", "mode must be auto, full, or restricted"
            )
        _number(
            "strategy.small_max_images",
            self.small_max_images,
            1,
            1000,
            integer=True,
        )
        _number(
            "strategy.work_unit_threshold_mp",
            self.work_unit_threshold_mp,
            0.1,
            10000.0,
        )
        _number(
            "strategy.high_feature_budget",
            self.high_feature_budget,
            32,
            50000,
            integer=True,
        )
        _number(
            "strategy.low_feature_budget",
            self.low_feature_budget,
            32,
            50000,
            integer=True,
        )
        if self.low_feature_budget > self.high_feature_budget:
            raise parameter_error(
                "strategy.low_feature_budget",
                "low_feature_budget cannot exceed high_feature_budget",
            )
        _number(
            "strategy.restricted_neighbor_links",
            self.restricted_neighbor_links,
            0,
            64,
            integer=True,
        )
        _number(
            "strategy.max_restricted_pairs",
            self.max_restricted_pairs,
            1,
            100000,
            integer=True,
        )
        return self


@dataclass(frozen=True)
class ExposureConfig:
    gain_min: float = 0.25
    gain_max: float = 4.0
    offset_min: float = -80.0
    offset_max: float = 80.0
    sample_stride: int = 7
    overflow_tolerance: float = 1.0

    def validate(self) -> "ExposureConfig":
        _number("exposure.gain_min", self.gain_min, 0.01, 10.0, low_inclusive=False)
        _number("exposure.gain_max", self.gain_max, 0.02, 20.0)
        if self.gain_min >= self.gain_max:
            raise parameter_error(
                "exposure.gain_min", "gain_min must be smaller than gain_max"
            )
        _number("exposure.offset_min", self.offset_min, -255.0, 255.0)
        _number("exposure.offset_max", self.offset_max, -255.0, 255.0)
        if self.offset_min >= self.offset_max:
            raise parameter_error(
                "exposure.offset_min", "offset_min must be smaller than offset_max"
            )
        _number(
            "exposure.sample_stride",
            self.sample_stride,
            1,
            64,
            integer=True,
        )
        _number(
            "exposure.overflow_tolerance",
            self.overflow_tolerance,
            0.0,
            32.0,
        )
        return self


@dataclass(frozen=True)
class BlendConfig:
    pyramid_levels: int = 4
    seam_color_weight: float = 0.72
    seam_gradient_weight: float = 0.28
    seam_gradient_limit: float = 55.0
    canvas_reconcile_px: int = 3

    def validate(self) -> "BlendConfig":
        _number("blend.pyramid_levels", self.pyramid_levels, 1, 8, integer=True)
        _number("blend.seam_color_weight", self.seam_color_weight, 0.0, 1.0)
        _number("blend.seam_gradient_weight", self.seam_gradient_weight, 0.0, 1.0)
        if abs(
            self.seam_color_weight + self.seam_gradient_weight - 1.0
        ) > 1e-9:
            raise parameter_error(
                "blend.seam_color_weight",
                "seam_color_weight + seam_gradient_weight must equal 1",
            )
        _number(
            "blend.seam_gradient_limit",
            self.seam_gradient_limit,
            0.1,
            255.0,
            low_inclusive=False,
        )
        _number(
            "blend.canvas_reconcile_px",
            self.canvas_reconcile_px,
            0,
            16,
            integer=True,
        )
        return self


@dataclass(frozen=True)
class RuntimeConfig:
    max_workers: int = 4
    max_cached_images: int = 8
    max_buffered_pairs: int = 12
    deterministic_cv_threads: int = 1

    def validate(self) -> "RuntimeConfig":
        _number("runtime.max_workers", self.max_workers, 1, 32, integer=True)
        _number(
            "runtime.max_cached_images",
            self.max_cached_images,
            1,
            128,
            integer=True,
        )
        _number(
            "runtime.max_buffered_pairs",
            self.max_buffered_pairs,
            1,
            256,
            integer=True,
        )
        _number(
            "runtime.deterministic_cv_threads",
            self.deterministic_cv_threads,
            1,
            16,
            integer=True,
        )
        return self


@dataclass(frozen=True)
class PipelineConfig:
    features: FeatureConfig = field(default_factory=FeatureConfig)
    homography: HomographyConfig = field(default_factory=HomographyConfig)
    strategy: StrategyConfig = field(default_factory=StrategyConfig)
    exposure: ExposureConfig = field(default_factory=ExposureConfig)
    blend: BlendConfig = field(default_factory=BlendConfig)
    runtime: RuntimeConfig = field(default_factory=RuntimeConfig)

    def validate(self) -> "PipelineConfig":
        self.features.validate()
        self.homography.validate()
        self.strategy.validate()
        self.exposure.validate()
        self.blend.validate()
        self.runtime.validate()
        return self

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def canonical_json(self) -> str:
        return json.dumps(
            self.to_dict(), sort_keys=True, separators=(",", ":"), allow_nan=False
        )

    def parameter_version(self, plan: Any | None = None) -> str:
        payload = {"config": self.to_dict(), "algorithm": ALGORITHM_VERSION}
        if plan is not None:
            payload["plan"] = plan
        raw = json.dumps(
            payload, sort_keys=True, separators=(",", ":"), allow_nan=False
        ).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()


def ensure_choice(name: str, value: str, choices: set[str]) -> None:
    if value not in choices:
        raise WorkbenchError(
            ErrorCode.PARA,
            f"{name} must be one of {sorted(choices)}",
            field=name,
        )
