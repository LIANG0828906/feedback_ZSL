"""Typed scene, segmentation, parameter, and result models."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np


@dataclass(frozen=True)
class BandInfo:
    index: int
    name: str
    file: str
    center_nm: float
    sigma_nm: float
    bit_depth: int
    radiometric_scale: float
    sha256: str


@dataclass(frozen=True)
class ExposureInfo:
    index: int
    name: str
    file: str
    exposure_time: float
    bit_depth: int
    sha256: str


@dataclass
class SceneMetadata:
    root: Path
    raw: dict[str, Any]
    scene_id: str
    title: str
    seed: int
    width: int
    height: int
    pixel_pitch_um: float
    metadata_version: str
    metadata_hash: str
    bands: list[BandInfo]
    exposures: list[ExposureInfo]
    wave_grid_nm: np.ndarray
    responses: np.ndarray
    xyz_basis: np.ndarray
    scene_white_xyz: np.ndarray
    target_white_xyz: np.ndarray
    camera_response: dict[str, float]
    registration_offsets: np.ndarray
    truth_file: str
    default_parameters: dict[str, Any] = field(default_factory=dict)


@dataclass
class Scene:
    metadata: SceneMetadata
    band_raw: np.ndarray
    exposure_raw: np.ndarray
    truth: dict[str, Any] | None = None

    @property
    def band_count(self) -> int:
        return int(self.band_raw.shape[0])

    @property
    def exposure_count(self) -> int:
        return int(self.exposure_raw.shape[0])

    @property
    def width(self) -> int:
        return self.metadata.width

    @property
    def height(self) -> int:
        return self.metadata.height


@dataclass(frozen=True)
class SegmentSpec:
    index: int
    mode: str
    x0: int
    y0: int
    x1: int
    y1: int
    band_start: int | None = None
    band_end: int | None = None
    label: str = ""

    @property
    def width(self) -> int:
        return self.x1 - self.x0

    @property
    def height(self) -> int:
        return self.y1 - self.y0

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "mode": self.mode,
            "x0": self.x0,
            "y0": self.y0,
            "x1": self.x1,
            "y1": self.y1,
            "band_start": self.band_start,
            "band_end": self.band_end,
            "label": self.label,
        }


@dataclass
class SegmentResult:
    spec: SegmentSpec
    radiance: np.ndarray
    absolute_rgb: np.ndarray
    rgb_chroma: np.ndarray
    linear_rgb: np.ndarray
    display: np.ndarray
    gamut_channel_mask: np.ndarray
    params: dict[str, Any]
    param_version: str
    stats: dict[str, Any]
    alignment_residual: float


@dataclass
class CompositeOutput:
    radiance: np.ndarray
    rgb_chroma: np.ndarray
    linear_rgb: np.ndarray
    display: np.ndarray
    gamut_channel_mask: np.ndarray
    segment_index: np.ndarray
    peak_radiance: float
    peak_location: tuple[int, int]
    gamut_count: int
    gamut_coordinates: list[dict[str, Any]]
    segment_stats: dict[int, dict[str, Any]]
    complete: bool
    param_versions: list[str]
    metadata_version: str
    metadata_hash: str
    algorithm_version: str

    def summary(self) -> dict[str, Any]:
        return {
            "complete": self.complete,
            "size": [int(self.radiance.shape[1]), int(self.radiance.shape[0])],
            "peak_radiance": float(self.peak_radiance),
            "peak_location": list(self.peak_location),
            "gamut_count": int(self.gamut_count),
            "gamut_coordinates": self.gamut_coordinates[:32],
            "segment_stats": {str(k): v for k, v in self.segment_stats.items()},
            "param_versions": self.param_versions,
            "metadata_version": self.metadata_version,
            "metadata_hash": self.metadata_hash,
            "algorithm_version": self.algorithm_version,
        }

    def compact_summary(self) -> dict[str, Any]:
        return {
            "complete": self.complete,
            "size": [int(self.radiance.shape[1]), int(self.radiance.shape[0])],
            "peak_radiance": float(self.peak_radiance),
            "peak_location": list(self.peak_location),
            "gamut_count": int(self.gamut_count),
            "param_versions": self.param_versions,
            "metadata_version": self.metadata_version,
            "metadata_hash": self.metadata_hash,
            "algorithm_version": self.algorithm_version,
        }


@dataclass
class ComparisonReport:
    equal: bool
    first_mismatch: dict[str, Any] | None = None
    peak_equal: bool = True
    gamut_equal: bool = True
    stages_checked: list[str] = field(default_factory=list)
    path_summaries: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.equal,
            "equal": self.equal,
            "first_mismatch": self.first_mismatch,
            "peak_equal": self.peak_equal,
            "gamut_equal": self.gamut_equal,
            "stages_checked": self.stages_checked,
            "paths": self.path_summaries,
        }
