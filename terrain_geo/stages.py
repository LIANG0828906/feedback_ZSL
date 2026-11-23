"""Transform stages with explicit parameter ranges and cache-key derivation."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable

import numpy as np

from .errors import GeoError, ErrorCode
from .geometry import (
    affine_inverse,
    affine_forward,
    apply_homography,
    distort_points,
    invert_homography,
    undistort_points,
)
from .hashing import canonical_json, content_hash
from .resample import ResampleConfig, cast_like_source, resample_full, resample_tiled

# Parameter ranges (documented and enforced).
HOMOGRAPHY_ABS_MAX = 1e6
SIZE_MAX = 16384
K_ABS_MAX = 1.0
RADIUS_MIN, RADIUS_MAX = 1.0, 1e5
SCALE_MIN, SCALE_MAX = 1e-3, 1e3
TRANSLATE_ABS_MAX = 1e5


def _check_range(value, low, high, stage: str, field_name: str):
    if not np.isfinite(value):
        raise GeoError(ErrorCode.E_PARA, f"{field_name} must be finite",
                       stage=stage, field=field_name)
    if value < low or value > high:
        raise GeoError(
            ErrorCode.E_PARA,
            f"{field_name}={value} out of range [{low}, {high}]",
            stage=stage, field=field_name)


def _check_size(size, stage: str):
    if (not isinstance(size, (tuple, list)) or len(size) != 2
            or not all(isinstance(v, int) for v in size)):
        raise GeoError(ErrorCode.E_PARA,
                       "output_size must be a (height, width) int pair",
                       stage=stage, field="output_size")
    for idx, value in enumerate(size):
        _check_range(value, 1, SIZE_MAX, stage,
                     f"output_size[{idx}]")


@dataclass
class StageConfig:
    """Common envelope for every stage."""

    stage_id: str
    params: dict
    param_version: str
    resample: ResampleConfig = field(default_factory=ResampleConfig)

    def validate(self) -> None:
        raise NotImplementedError

    def inverse_map(self) -> Callable[[np.ndarray], np.ndarray]:
        raise NotImplementedError

    def forward_points(self, points: np.ndarray) -> np.ndarray:
        raise NotImplementedError

    @property
    def output_size(self):
        return tuple(int(v) for v in self.params["output_size"])

    def param_fingerprint(self) -> str:
        return content_hash({
            "stage": self.stage_id,
            "version": self.param_version,
            "params": jsonable(self.params),
            "resample": {
                "tile_size": self.resample.tile_size,
                "order": self.resample.order,
                "max_overlap": self.resample.max_overlap,
            },
        })


def jsonable(obj):
    if isinstance(obj, np.ndarray):
        return obj.tolist()
    if isinstance(obj, (tuple, list)):
        return [jsonable(v) for v in obj]
    if isinstance(obj, dict):
        return {k: jsonable(v) for k, v in sorted(obj.items())}
    if isinstance(obj, (np.floating, np.integer)):
        return obj.item()
    return obj


class HomographyStage(StageConfig):
    def validate(self) -> None:
        self.resample.validate(self.stage_id)
        _check_size(self.params.get("output_size"), self.stage_id)
        H = np.asarray(self.params.get("H"), dtype=np.float64)
        if H.shape != (3, 3):
            raise GeoError(ErrorCode.E_PARA, "H must be a 3x3 matrix",
                           stage=self.stage_id, field="H")
        if not np.all(np.isfinite(H)) or np.max(np.abs(H)) > HOMOGRAPHY_ABS_MAX:
            raise GeoError(ErrorCode.E_PARA,
                           f"H entries must be finite and <= {HOMOGRAPHY_ABS_MAX}",
                           stage=self.stage_id, field="H")
        if abs(H[2, 2]) < 1e-12 or np.linalg.matrix_rank(H) < 3:
            raise GeoError(ErrorCode.E_PARA, "H must be full rank",
                           stage=self.stage_id, field="H")

    def inverse_map(self):
        H = np.asarray(self.params["H"], dtype=np.float64)
        Hinv = invert_homography(H)
        return lambda pts: apply_homography(Hinv, pts)

    def forward_points(self, points):
        return apply_homography(np.asarray(self.params["H"], dtype=np.float64),
                                points)


class RadialCorrectionStage(StageConfig):
    def validate(self) -> None:
        self.resample.validate(self.stage_id)
        _check_size(self.params.get("output_size"), self.stage_id)
        p = self.params
        center = p.get("center")
        if (not isinstance(center, (tuple, list)) or len(center) != 2
                or not all(np.isfinite(v) for v in center)):
            raise GeoError(ErrorCode.E_PARA,
                           "center must be a finite (cx, cy) pair",
                           stage=self.stage_id, field="center")
        _check_range(p["radius"], RADIUS_MIN, RADIUS_MAX,
                     self.stage_id, "radius")
        _check_range(p["k1"], -K_ABS_MAX, K_ABS_MAX, self.stage_id, "k1")
        _check_range(p["k2"], -K_ABS_MAX, K_ABS_MAX, self.stage_id, "k2")
        h, w = self.output_size
        for idx, value in enumerate(center):
            bound = (w, h)[idx]
            _check_range(value, -bound, 2 * bound, self.stage_id,
                         f"center[{idx}]")

    def inverse_map(self):
        p = self.params
        return lambda pts: distort_points(
            pts, p["center"], p["radius"], p["k1"], p["k2"])

    def forward_points(self, points):
        p = self.params
        return undistort_points(
            points, p["center"], p["radius"], p["k1"], p["k2"])


class AffineStage(StageConfig):
    def validate(self) -> None:
        self.resample.validate(self.stage_id)
        _check_size(self.params.get("output_size"), self.stage_id)
        p = self.params
        scale = p.get("scale")
        translate = p.get("translate")
        if not isinstance(scale, (tuple, list)) or len(scale) != 2:
            raise GeoError(ErrorCode.E_PARA, "scale must be (sx, sy)",
                           stage=self.stage_id, field="scale")
        if not isinstance(translate, (tuple, list)) or len(translate) != 2:
            raise GeoError(ErrorCode.E_PARA, "translate must be (tx, ty)",
                           stage=self.stage_id, field="translate")
        for idx, value in enumerate(scale):
            _check_range(value, SCALE_MIN, SCALE_MAX,
                         self.stage_id, f"scale[{idx}]")
        for idx, value in enumerate(translate):
            _check_range(value, -TRANSLATE_ABS_MAX, TRANSLATE_ABS_MAX,
                         self.stage_id, f"translate[{idx}]")

    def inverse_map(self):
        p = self.params
        return lambda pts: affine_inverse(pts, p["scale"], p["translate"])

    def forward_points(self, points):
        p = self.params
        return affine_forward(points, p["scale"], p["translate"])


def execute_stage(stage: StageConfig, image: np.ndarray,
                  executor=None) -> np.ndarray:
    """Validate, then resample one stage; channels stay independent."""
    stage.validate()
    if image.ndim == 2:
        image = image[..., None]
    if image.ndim != 3:
        raise GeoError(ErrorCode.E_PARA,
                       "image must be 2-D (single channel) or 3-D (HxWxC)",
                       stage=stage.stage_id, field="image")
    out_h, out_w = stage.output_size
    inverse = stage.inverse_map()
    cfg = stage.resample
    if cfg.tile_size == 0:
        result = resample_full(image, inverse, out_h, out_w, cfg.order)
    else:
        result = resample_tiled(image, inverse, out_h, out_w, cfg,
                                stage.stage_id, executor)
    result = cast_like_source(result, image.dtype)
    if image.shape[2] == 1:
        return result[..., 0]
    return result


def stage_cache_key(stage: StageConfig, input_hash: str,
                    upstream_key: str | None) -> str:
    """Cache key = input content hash + stage id + params + upstream key."""
    payload = canonical_json({
        "input_hash": input_hash,
        "upstream_key": upstream_key,
        "stage_fingerprint": stage.param_fingerprint(),
    })
    return content_hash(payload)
