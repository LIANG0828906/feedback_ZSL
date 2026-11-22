"""Spectral reconstruction, white-point adaptation, and gamut mapping."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import WorkbenchError, require


SRGB_WHITE_XYZ = np.array([0.95047, 1.0, 1.08883], dtype=np.float64)
SRGB_XYZ_TO_LINEAR_RGB = np.array(
    [
        [3.2404542, -1.5371385, -0.4985314],
        [-0.9692660, 1.8760108, 0.0415560],
        [0.0556434, -0.2040259, 1.0572252],
    ],
    dtype=np.float64,
)

_BRADFORD = np.array(
    [
        [0.8951, 0.2664, -0.1614],
        [-0.7502, 1.7135, 0.0367],
        [0.0389, -0.0685, 1.0296],
    ],
    dtype=np.float64,
)
_BRADFORD_INV = np.linalg.inv(_BRADFORD)


def _asymmetric_gaussian(wavelength: np.ndarray, mu: float, sigma_left: float, sigma_right: float) -> np.ndarray:
    sigma = np.where(wavelength < mu, sigma_left, sigma_right)
    return np.exp(-0.5 * ((wavelength - mu) / sigma) ** 2)


def cie_xyz_approximation(wavelength_nm: np.ndarray) -> np.ndarray:
    """Approximate CIE 1931 2-degree CMFs with Wyman et al. multi-lobe fits."""

    w = np.asarray(wavelength_nm, dtype=np.float64)
    x_bar = (
        1.056 * _asymmetric_gaussian(w, 599.8, 37.9, 31.0)
        + 0.362 * _asymmetric_gaussian(w, 442.0, 16.0, 26.7)
        - 0.065 * _asymmetric_gaussian(w, 501.1, 20.4, 26.2)
    )
    y_bar = (
        0.821 * _asymmetric_gaussian(w, 568.8, 46.9, 40.5)
        + 0.286 * _asymmetric_gaussian(w, 530.9, 16.3, 31.1)
    )
    z_bar = (
        1.217 * _asymmetric_gaussian(w, 437.0, 11.8, 36.0)
        + 0.681 * _asymmetric_gaussian(w, 459.0, 26.0, 13.8)
    )
    return np.stack([x_bar, y_bar, z_bar], axis=-1)


def gaussian_band_response(
    wavelength_nm: np.ndarray,
    center_nm: float,
    fwhm_nm: float,
) -> np.ndarray:
    sigma = float(fwhm_nm) / (2.0 * np.sqrt(2.0 * np.log(2.0)))
    values = np.exp(-0.5 * ((np.asarray(wavelength_nm, dtype=np.float64) - float(center_nm)) / sigma) ** 2)
    return values


def build_spectral_basis(
    centers_nm: list[float] | tuple[float, ...],
    fwhms_nm: list[float] | tuple[float, ...],
    wavelength_nm: np.ndarray | None = None,
) -> dict[str, Any]:
    """Build deterministic response curves and band-to-XYZ integration weights."""

    wavelengths = np.arange(400.0, 701.0, 5.0) if wavelength_nm is None else np.asarray(wavelength_nm, dtype=np.float64)
    cmf = cie_xyz_approximation(wavelengths)
    responses = np.stack(
        [gaussian_band_response(wavelengths, center, fwhm) for center, fwhm in zip(centers_nm, fwhms_nm, strict=True)],
        axis=0,
    )
    raw_weights = np.stack(
        [
            np.trapezoid(responses[index, :, None] * cmf, wavelengths, axis=0)
            / np.trapezoid(responses[index], wavelengths)
            for index in range(responses.shape[0])
        ],
        axis=0,
    )
    y_sum = float(raw_weights[:, 1].sum())
    require(y_sum > 0.0, "E_META", "Spectral basis has zero Y normalization", field="spectral.band_responses")
    weights = raw_weights / y_sum
    source_white = weights.sum(axis=0)
    return {
        "wavelengths_nm": wavelengths.tolist(),
        "cmf_xyz": cmf.tolist(),
        "band_responses": responses.tolist(),
        "xyz_weights": weights.tolist(),
        "source_white_xyz": source_white.tolist(),
    }


def bands_to_xyz(band_radiance: np.ndarray, xyz_weights: np.ndarray) -> np.ndarray:
    bands = np.asarray(band_radiance, dtype=np.float64)
    weights = np.asarray(xyz_weights, dtype=np.float64)
    require(
        bands.ndim == 3 and weights.shape == (bands.shape[0], 3),
        "E_META",
        "Band radiance and XYZ weights have incompatible shapes",
        field="spectral.xyz_weights",
    )
    return np.moveaxis(np.tensordot(weights.T, bands, axes=(1, 0)), 0, -1)


def bradford_adaptation_matrix(source_white_xyz: np.ndarray, target_white_xyz: np.ndarray) -> np.ndarray:
    source = np.asarray(source_white_xyz, dtype=np.float64)
    target = np.asarray(target_white_xyz, dtype=np.float64)
    require(
        source.shape == (3,) and target.shape == (3,),
        "E_META",
        "White points must be XYZ triples",
        field="white_point",
    )
    require(
        np.all(np.isfinite(source)) and np.all(source > 0.0) and np.all(np.isfinite(target)) and np.all(target > 0.0),
        "E_META",
        "White points must contain finite positive values",
        field="white_point",
    )
    src_lms = _BRADFORD @ source
    dst_lms = _BRADFORD @ target
    scale = dst_lms / src_lms
    return _BRADFORD_INV @ np.diag(scale) @ _BRADFORD


def xyz_to_linear_rgb(
    xyz: np.ndarray,
    source_white_xyz: np.ndarray,
    target_white_xyz: np.ndarray = SRGB_WHITE_XYZ,
    target_matrix: np.ndarray = SRGB_XYZ_TO_LINEAR_RGB,
) -> np.ndarray:
    adaptation = bradford_adaptation_matrix(source_white_xyz, target_white_xyz)
    adapted = np.asarray(xyz, dtype=np.float64) @ adaptation.T
    return adapted @ np.asarray(target_matrix, dtype=np.float64).T


def linear_rgb_to_xyz(
    rgb: np.ndarray,
    source_white_xyz: np.ndarray,
    target_white_xyz: np.ndarray = SRGB_WHITE_XYZ,
    target_matrix: np.ndarray = SRGB_XYZ_TO_LINEAR_RGB,
) -> np.ndarray:
    inverse_matrix = np.linalg.inv(np.asarray(target_matrix, dtype=np.float64))
    adapted = np.asarray(rgb, dtype=np.float64) @ inverse_matrix.T
    inverse_adaptation = bradford_adaptation_matrix(target_white_xyz, source_white_xyz)
    return adapted @ inverse_adaptation.T


@dataclass(frozen=True)
class GamutMapResult:
    mapped: np.ndarray
    out_of_gamut_mask: np.ndarray
    boundary_rgb: np.ndarray
    original_excess: np.ndarray
    out_of_gamut_count: int
    max_excess: float
    iterations: int


def _luminance(rgb: np.ndarray) -> np.ndarray:
    return rgb @ np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)


def _tone_map_luminance(luminance: np.ndarray) -> np.ndarray:
    y = np.asarray(luminance, dtype=np.float64)
    # Smoothly maps the entire real line into (0, 1) without clipping.
    return 0.5 * (1.0 + y / (1.0 + np.abs(y)))


def _project_to_cube(rgb: np.ndarray, inset: float) -> np.ndarray:
    """Desaturate each pixel toward its neutral using an exact cube intersection."""

    rgb = np.asarray(rgb, dtype=np.float64)
    flat = rgb.reshape(-1, 3)
    output = flat.copy()
    luminance = _luminance(flat)
    neutral = luminance[:, None]
    delta = flat - neutral

    for index in range(flat.shape[0]):
        n = float(neutral[index, 0])
        if 0.0 <= n <= 1.0:
            scale = 1.0
            for channel in range(3):
                component = float(delta[index, channel])
                if component > 0.0:
                    scale = min(scale, (1.0 - inset - n) / component)
                elif component < 0.0:
                    scale = min(scale, (inset - n) / component)
            scale = max(0.0, min(1.0, scale))
            output[index] = n + scale * delta[index]
        else:
            # The perceptual intent first brings luminance into range, then
            # intersects the resulting neutral-to-color ray with the cube.
            mapped_y = float(_tone_map_luminance(np.array([n]))[0])
            if abs(n) > 1.0e-15:
                output[index] = flat[index] * (mapped_y / n)
            else:
                output[index] = mapped_y
            refreshed = output[index]
            n2 = float(_luminance(refreshed[None, :])[0])
            delta2 = refreshed - n2
            scale = 1.0
            for channel in range(3):
                component = float(delta2[channel])
                if component > 0.0:
                    scale = min(scale, (1.0 - inset - n2) / component)
                elif component < 0.0:
                    scale = min(scale, (inset - n2) / component)
            scale = max(0.0, min(1.0, scale))
            output[index] = n2 + scale * delta2
    return output.reshape(rgb.shape)


def _cube_boundary(rgb: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    flat = np.asarray(rgb, dtype=np.float64).reshape(-1, 3)
    boundary = flat.copy()
    luminance = _luminance(flat)
    for index in range(flat.shape[0]):
        y = float(luminance[index])
        if 0.0 <= y <= 1.0:
            neutral = np.full(3, y, dtype=np.float64)
            delta = flat[index] - neutral
            scale = 1.0
            for channel in range(3):
                component = float(delta[channel])
                if component > 0.0:
                    scale = min(scale, (1.0 - y) / component)
                elif component < 0.0:
                    scale = min(scale, (0.0 - y) / component)
            boundary[index] = neutral + max(0.0, min(1.0, scale)) * delta
        else:
            compressed = _tone_map_luminance(np.array([y]))[0]
            scaled = flat[index] * (compressed / y) if abs(y) > 1.0e-15 else np.full(3, compressed)
            maximum = float(np.max(scaled))
            minimum = float(np.min(scaled))
            if maximum > 1.0:
                scaled = scaled / maximum
            elif minimum < 0.0:
                scaled = compressed + (scaled - compressed) * (compressed / max(compressed - minimum, 1.0e-15))
            boundary[index] = np.clip(scaled, 0.0, 1.0)
    excess = np.max(np.maximum(np.maximum(-flat, flat - 1.0), 0.0), axis=1)
    return boundary.reshape(rgb.shape), excess.reshape(rgb.shape[:-1])


def map_to_gamut(
    linear_rgb: np.ndarray,
    *,
    intent: str = "perceptual",
    tolerance: float = 1.0e-6,
    max_iterations: int = 8,
) -> GamutMapResult:
    rgb = np.asarray(linear_rgb, dtype=np.float64)
    require(rgb.ndim == 3 and rgb.shape[-1] == 3, "E_PARA", "linear_rgb must have shape (H, W, 3)", field="linear_rgb")
    require(intent in {"perceptual", "relative_colorimetric"}, "E_PARA", "Unsupported gamut intent", field="gamut_intent")
    require(np.isfinite(tolerance) and tolerance >= 0.0, "E_PARA", "Invalid gamut tolerance", field="gamut_tolerance")
    require(max_iterations >= 1, "E_PARA", "max_gamut_iterations must be positive", field="max_gamut_iterations")

    finite = np.all(np.isfinite(rgb), axis=-1)
    if not np.all(finite):
        count = int(np.size(finite) - np.count_nonzero(finite))
        raise WorkbenchError(
            "E_GAMUT",
            "Non-finite linear RGB values cannot be gamut mapped",
            field="linear_rgb",
            details={"out_of_gamut_count": count, "max_excess": None},
        )

    out_mask = np.any((rgb < -tolerance) | (rgb > 1.0 + tolerance), axis=-1)
    out_count = int(np.count_nonzero(out_mask))
    boundary, excess_map = _cube_boundary(rgb)
    max_excess = float(np.max(excess_map)) if excess_map.size else 0.0

    if intent == "relative_colorimetric":
        luminance = _luminance(rgb.reshape(-1, 3))
        invalid_luminance = (luminance < 0.0) | (luminance > 1.0)
        if np.any(invalid_luminance):
            raise WorkbenchError(
                "E_GAMUT",
                "relative_colorimetric cannot preserve luminance outside [0, 1]",
                field="gamut_intent",
                details={
                    "out_of_gamut_count": out_count,
                    "max_excess": max_excess,
                    "invalid_luminance_count": int(np.count_nonzero(invalid_luminance)),
                },
            )

    current = rgb.copy()
    iterations = 0
    for iterations in range(1, max_iterations + 1):
        current_out = np.any((current < -tolerance) | (current > 1.0 + tolerance), axis=-1)
        if not np.any(current_out):
            break
        inset = tolerance if intent == "relative_colorimetric" else max(tolerance, 1.0e-7)
        current = _project_to_cube(current, inset)
    else:
        current_out = np.any((current < -tolerance) | (current > 1.0 + tolerance), axis=-1)
        if np.any(current_out):
            remaining = np.maximum(np.maximum(-current, current - 1.0), 0.0)
            raise WorkbenchError(
                "E_GAMUT",
                "Gamut mapping did not converge within the declared intent",
                field="gamut_intent",
                details={
                    "out_of_gamut_count": int(np.count_nonzero(current_out)),
                    "max_excess": float(np.max(remaining)),
                    "iterations": iterations,
                },
            )

    return GamutMapResult(
        mapped=current,
        out_of_gamut_mask=out_mask,
        boundary_rgb=boundary,
        original_excess=excess_map,
        out_of_gamut_count=out_count,
        max_excess=max_excess,
        iterations=iterations,
    )


def quantize_linear_rgb(linear_rgb: np.ndarray, bit_depth: int) -> tuple[np.ndarray, int]:
    require(
        isinstance(bit_depth, int) and not isinstance(bit_depth, bool) and 1 <= bit_depth <= 16,
        "E_META",
        "Output bit depth must be an integer in [1, 16]",
        field="target.bit_depth",
    )
    rgb = np.asarray(linear_rgb, dtype=np.float64)
    require(
        np.all(np.isfinite(rgb)),
        "E_GAMUT",
        "Cannot quantize non-finite RGB",
        field="linear_rgb",
    )
    require(
        np.all(rgb >= -1.0e-9) and np.all(rgb <= 1.0 + 1.0e-9),
        "E_GAMUT",
        "Quantization requires in-gamut RGB after mapping",
        field="linear_rgb",
    )
    levels = (1 << bit_depth) - 1
    exact = np.clip(rgb, 0.0, 1.0)
    quantized = np.rint(exact * levels).astype(np.uint16 if bit_depth > 8 else np.uint8)
    return quantized, levels


def linear_rgb_to_preview_rgb8(linear_rgb: np.ndarray, gamma: float = 2.2) -> np.ndarray:
    rgb = np.clip(np.asarray(linear_rgb, dtype=np.float64), 0.0, 1.0)
    encoded = np.power(rgb, 1.0 / float(gamma))
    return np.rint(encoded * 255.0).astype(np.uint8)


def xyz_to_uv_chromaticity(xyz: np.ndarray) -> np.ndarray:
    values = np.asarray(xyz, dtype=np.float64)
    denominator = values[..., 0] + 15.0 * values[..., 1] + 3.0 * values[..., 2]
    safe = np.where(np.abs(denominator) < 1.0e-15, 1.0e-15, denominator)
    return np.stack([4.0 * values[..., 0] / safe, 9.0 * values[..., 1] / safe], axis=-1)

