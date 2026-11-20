from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache
from typing import Any

import numpy as np

from .constants import TONE_CURVES_WITH_COMPRESSION
from .errors import AtlasError, ErrorCode
from .hashing import array_hash, hash_value
from .model import TileErrorSummary, TileParams, TransformConfig
from .transfer import decode_transfer, encode_transfer

PRIMARIES: dict[str, tuple[tuple[float, float], ...]] = {
    "rec709": ((0.640, 0.330), (0.300, 0.600), (0.150, 0.060)),
    "display-p3": ((0.680, 0.320), (0.265, 0.690), (0.150, 0.060)),
    "rec2020": ((0.708, 0.292), (0.170, 0.797), (0.131, 0.046)),
}

WHITE_POINT_XY: dict[str, tuple[float, float]] = {
    "D50": (0.3457, 0.3585),
    "D65": (0.3127, 0.3290),
}

BRADFORD = np.array(
    [
        [0.8951, 0.2664, -0.1614],
        [-0.7502, 1.7135, 0.0367],
        [0.0389, -0.0685, 1.0296],
    ],
    dtype=np.float64,
)


def xy_to_xyz(xy: tuple[float, float]) -> np.ndarray:
    x, y = xy
    return np.array([x / y, 1.0, (1.0 - x - y) / y], dtype=np.float64)


@lru_cache(maxsize=64)
def rgb_to_xyz(space: str, white_point: str = "D65") -> np.ndarray:
    primaries = PRIMARIES[space]
    columns = np.stack([xy_to_xyz(point) for point in primaries], axis=1)
    white = xy_to_xyz(WHITE_POINT_XY[white_point])
    scale = np.linalg.solve(columns, white)
    return columns * scale[np.newaxis, :]


@lru_cache(maxsize=64)
def xyz_to_rgb(space: str, white_point: str = "D65") -> np.ndarray:
    return np.linalg.inv(rgb_to_xyz(space, white_point))


@lru_cache(maxsize=16)
def bradford_matrix(source_white: str, target_white: str) -> np.ndarray:
    if source_white == target_white:
        return np.eye(3, dtype=np.float64)
    source_xyz = xy_to_xyz(WHITE_POINT_XY[source_white])
    target_xyz = xy_to_xyz(WHITE_POINT_XY[target_white])
    source_cone = BRADFORD @ source_xyz
    target_cone = BRADFORD @ target_xyz
    scale = np.diag(target_cone / source_cone)
    return np.linalg.inv(BRADFORD) @ scale @ BRADFORD


@lru_cache(maxsize=64)
def build_rgb_to_rgb_matrix(
    source_space: str,
    target_space: str,
    source_white: str = "D65",
    target_white: str = "D65",
    adaptation: str = "bradford",
) -> np.ndarray:
    if source_space not in PRIMARIES:
        raise ValueError(f"unsupported source space {source_space}")
    if target_space not in PRIMARIES:
        raise ValueError(f"unsupported target space {target_space}")
    if source_white not in WHITE_POINT_XY or target_white not in WHITE_POINT_XY:
        raise ValueError("unsupported white point")
    if adaptation == "none":
        adaptation_matrix = np.eye(3, dtype=np.float64)
    elif adaptation == "bradford":
        adaptation_matrix = bradford_matrix(source_white, target_white)
    else:
        raise ValueError(f"unsupported adaptation {adaptation}")
    if (
        source_space == target_space
        and source_white == target_white
        and adaptation in ("bradford", "none")
    ):
        return np.eye(3, dtype=np.float64)
    matrix = (
        xyz_to_rgb(target_space, target_white)
        @ adaptation_matrix
        @ rgb_to_xyz(source_space, source_white)
    )
    # A color matrix is a shared dependency. Rounding keeps its digest stable
    # across BLAS builds while retaining more than float32 precision.
    return np.round(matrix, 15)


def _hable_curve(values: np.ndarray) -> np.ndarray:
    a, b, c, d, e, f = 0.15, 0.50, 0.10, 0.20, 0.02, 0.30
    exposed = a * values * values + c * b * values
    mapped = (exposed + d * e) / (a * values * values + b * values + d * f) - e / f
    white = (a + c * b + d * e) / (a + b + d * f) - e / f
    return mapped / white


def apply_tone_curve(values: np.ndarray, curve: str, strength: float) -> np.ndarray:
    values = np.asarray(values, dtype=np.float64)
    if curve == "none":
        return values
    if curve == "reinhard":
        mapped = values / (1.0 + np.maximum(values, 0.0))
    elif curve == "hable":
        mapped = _hable_curve(np.maximum(values, 0.0))
    else:
        raise ValueError(f"unsupported tone curve {curve}")
    if strength >= 1.0:
        return mapped
    return values * (1.0 - strength) + mapped * strength


@dataclass(frozen=True)
class TileTransformResult:
    encoded: np.ndarray
    summary: TileErrorSummary
    clipped_fraction: float
    max_unclipped: float
    min_unclipped: float


@dataclass(frozen=True)
class ColorChain:
    source_space: str
    source_transfer: str
    source_peak_nits: float
    config: TransformConfig

    @property
    def matrix(self) -> np.ndarray:
        return build_rgb_to_rgb_matrix(
            self.source_space,
            self.config.target_color_space,
            self.config.source_white_point,
            self.config.target_white_point,
            self.config.adaptation,
        )

    @property
    def base_matrix(self) -> np.ndarray:
        return build_rgb_to_rgb_matrix(
            self.source_space,
            self.config.target_color_space,
            self.config.source_white_point,
            self.config.source_white_point,
            "none",
        )

    @property
    def adaptation_matrix(self) -> np.ndarray:
        return bradford_matrix(
            self.config.source_white_point,
            self.config.target_white_point,
        )

    @property
    def digest(self) -> str:
        return hash_value(
            {
                "source_space": self.source_space,
                "source_transfer": self.source_transfer,
                "source_peak_nits": float(self.source_peak_nits),
                "config": self.config.to_dict(),
                "matrix": self.matrix.tolist(),
            }
        )

    def decode_source(self, encoded: np.ndarray) -> np.ndarray:
        return decode_transfer(
            encoded,
            self.source_transfer,
            self.source_peak_nits,
        )

    def transform_tile(
        self,
        linear_rgba: np.ndarray,
        params: TileParams,
        *,
        bit_depth: int,
        tile_index: int,
        reject_exposure_overflow: bool,
    ) -> TileTransformResult:
        rgba = np.asarray(linear_rgba, dtype=np.float64)
        rgb = rgba[..., :3]
        alpha = np.asarray(rgba[..., 3], dtype=np.float32)
        exposure_factor = float(np.exp2(params.exposure_ev))
        if not np.isfinite(exposure_factor):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "exposure_ev produces a non-finite multiplier",
                field="exposure_ev",
                tile=tile_index,
                details={"exposure_ev": params.exposure_ev},
            )

        source_linear = rgb * exposure_factor
        target_linear = np.einsum("...c,dc->...d", source_linear, self.matrix, optimize=True)
        peak_ratio = float(self.source_peak_nits) / float(self.config.target_peak_nits)
        normalized = target_linear * peak_ratio
        mapped = apply_tone_curve(normalized, params.tone_curve, params.tone_strength)
        if not np.all(np.isfinite(mapped)):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tone mapping produced non-finite values",
                field="tone_curve",
                tile=tile_index,
            )

        min_unclipped = float(np.min(mapped))
        max_unclipped = float(np.max(mapped))
        if (
            reject_exposure_overflow
            and params.exposure_ev != 0.0
            and params.tone_curve not in TONE_CURVES_WITH_COMPRESSION
            and max_unclipped > 1.0 + 1e-6
        ):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "exposure pushes encoded output beyond the representable [0, 1] range",
                field="exposure_ev",
                tile=tile_index,
                details={
                    "exposure_ev": params.exposure_ev,
                    "tone_curve": params.tone_curve,
                    "max_unclipped": max_unclipped,
                },
            )

        clipped_mask = (mapped < 0.0) | (mapped > 1.0)
        clipped_fraction = float(np.count_nonzero(clipped_mask) / clipped_mask.size)
        encoded_ideal = encode_transfer(
            np.clip(mapped, 0.0, 1.0),
            self.config.target_transfer,
            self.config.target_peak_nits,
        )
        encoded_ideal = np.clip(encoded_ideal, 0.0, 1.0)
        max_code = (1 << int(bit_depth)) - 1
        codes = np.floor(encoded_ideal * max_code + 0.5).astype(np.uint16)
        stored_rgb = (codes.astype(np.float32) / float(max_code)).astype(np.float16)
        stored = np.empty(rgba.shape, dtype=np.float16)
        stored[..., :3] = stored_rgb
        stored[..., 3] = alpha.astype(np.float16)

        stored_codes = np.floor(
            stored[..., :3].astype(np.float64) * max_code + 0.5
        )
        deviation = np.abs(stored_codes - encoded_ideal * max_code)
        max_deviation = float(np.max(deviation))
        mean_deviation = float(np.mean(deviation))
        rmse = float(np.sqrt(np.mean(np.square(deviation))))
        p99 = float(np.percentile(deviation, 99.0))
        summary_payload: dict[str, Any] = {
            "tile": tile_index,
            "pixel_count": int(rgb.shape[0] * rgb.shape[1]),
            "max_code_deviation": max_deviation,
            "mean_code_deviation": mean_deviation,
            "rmse_code_deviation": rmse,
            "p99_code_deviation": p99,
            "clipped_fraction": clipped_fraction,
        }
        summary = TileErrorSummary(
            tile=tile_index,
            max_code_deviation=max_deviation,
            mean_code_deviation=mean_deviation,
            rmse_code_deviation=rmse,
            p99_code_deviation=p99,
            clipped_fraction=clipped_fraction,
            summary_hash=hash_value(summary_payload),
        )
        return TileTransformResult(
            encoded=np.ascontiguousarray(stored),
            summary=summary,
            clipped_fraction=clipped_fraction,
            max_unclipped=max_unclipped,
            min_unclipped=min_unclipped,
        )

    def preview_srgb(self, encoded_rgba: np.ndarray) -> np.ndarray:
        rgba = np.asarray(encoded_rgba, dtype=np.float64)
        linear_target = decode_transfer(
            rgba[..., :3],
            self.config.target_transfer,
            self.config.target_peak_nits,
        )
        display_matrix = build_rgb_to_rgb_matrix(
            self.config.target_color_space,
            "rec709",
            self.config.target_white_point,
            "D65",
            "bradford",
        )
        display_linear = np.einsum("...c,dc->...d", linear_target, display_matrix, optimize=True)
        display_linear = np.clip(display_linear, 0.0, 1.0)
        srgb = encode_transfer(display_linear, "srgb", 1000.0)
        out = np.empty(rgba.shape, dtype=np.float32)
        out[..., :3] = np.clip(srgb, 0.0, 1.0)
        out[..., 3] = np.clip(rgba[..., 3], 0.0, 1.0)
        return out


def matrix_digest(matrix: np.ndarray) -> str:
    return array_hash(np.ascontiguousarray(matrix, dtype=np.float64))
