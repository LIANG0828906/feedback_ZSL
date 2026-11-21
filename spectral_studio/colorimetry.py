"""Colorimetric foundations: observers, illuminants, white points,
chromatic adaptation and target color spaces.

Observer color-matching functions are analytic multi-Gaussian fits
(Wyman/Sloan/Shirley-style lobe fits for the 1931 2-degree observer; a
documented analytic approximation for the 1964 10-degree observer).
Illuminants are deterministic analytic spectra (Planckian radiator at the
documented correlated color temperature, or equal-energy).  Everything is
computed on the unified wavelength grid so results are reproducible and
hashable without external data files.
"""

from __future__ import annotations

import numpy as np

from .canon import content_hash
from .errors import ErrorCode, SpectralStudioError
from .spectrum import unified_grid

OBSERVER_1931_2 = "1931_2"
OBSERVER_1964_10 = "1964_10"
KNOWN_OBSERVERS = (OBSERVER_1931_2, OBSERVER_1964_10)

ADAPT_BRADFORD = "bradford"
ADAPT_VON_KRIES = "von_kries"
ADAPT_NONE = "none"
KNOWN_ADAPTATIONS = (ADAPT_BRADFORD, ADAPT_VON_KRIES, ADAPT_NONE)

SPACE_SRGB = "srgb"
SPACE_DISPLAY_P3 = "display-p3"
KNOWN_SPACES = (SPACE_SRGB, SPACE_DISPLAY_P3)

# Cone response matrices for chromatic adaptation.
BRADFORD_M = np.array([
    [0.8951, 0.2664, -0.1614],
    [-0.7502, 1.7135, 0.0367],
    [0.0389, -0.0685, 1.0296],
], dtype=np.float64)

VON_KRIES_M = np.array([
    [0.40024, 0.70760, -0.08081],
    [-0.22630, 1.16532, 0.04570],
    [0.00000, 0.00000, 0.91822],
], dtype=np.float64)

# XYZ -> linear RGB matrices (D65 white).
XYZ_TO_RGB = {
    SPACE_SRGB: np.array([
        [3.2404542, -1.5371385, -0.4985314],
        [-0.9692660, 1.8760108, 0.0415560],
        [0.0556434, -0.2040259, 1.0572252],
    ], dtype=np.float64),
    SPACE_DISPLAY_P3: np.array([
        [2.4934969, -0.9313836, -0.4027107],
        [-0.8294890, 1.7626641, 0.0236247],
        [0.0358458, -0.0761724, 0.9568845],
    ], dtype=np.float64),
}

_ILLUMINANT_CCT = {"D65": 6504.0, "D50": 5003.0, "A": 2856.0}
KNOWN_ILLUMINANTS = tuple(_ILLUMINANT_CCT) + ("E",)


def _gaussian(x, mu, sigma_left, sigma_right):
    sigma = np.where(x < mu, sigma_left, sigma_right)
    return np.exp(-0.5 * ((x - mu) / sigma) ** 2)


def color_matching_functions(observer=OBSERVER_1931_2) -> np.ndarray:
    """Observer CMFs on the unified grid, shape (N, 3) for (x, y, z)."""
    grid = unified_grid()
    if observer == OBSERVER_1931_2:
        xbar = (1.056 * _gaussian(grid, 599.8, 37.9, 31.0)
                + 0.362 * _gaussian(grid, 442.0, 16.0, 26.7)
                - 0.065 * _gaussian(grid, 501.1, 20.4, 26.2))
        ybar = (0.821 * _gaussian(grid, 568.8, 46.9, 40.5)
                + 0.286 * _gaussian(grid, 530.9, 16.3, 31.1))
        zbar = (1.217 * _gaussian(grid, 437.0, 11.8, 36.0)
                + 0.681 * _gaussian(grid, 459.0, 26.0, 13.8))
    elif observer == OBSERVER_1964_10:
        # Analytic approximation of the 1964 10-degree observer: broader
        # lobes, slightly shifted peaks, suppressed short-wave x lobe.
        xbar = (1.130 * _gaussian(grid, 595.0, 40.0, 33.0)
                + 0.340 * _gaussian(grid, 446.0, 17.0, 28.0)
                - 0.058 * _gaussian(grid, 503.0, 21.0, 27.0))
        ybar = (0.905 * _gaussian(grid, 559.0, 44.0, 38.0)
                + 0.158 * _gaussian(grid, 525.0, 17.0, 33.0))
        zbar = (1.290 * _gaussian(grid, 441.0, 13.0, 37.0)
                + 0.620 * _gaussian(grid, 465.0, 27.0, 15.0))
    else:
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "unknown observer", {"observer": observer})
    cmf = np.stack([xbar, ybar, zbar], axis=1)
    return np.clip(cmf, 0.0, None)


def cmf_hash(observer) -> str:
    cmf = color_matching_functions(observer)
    return content_hash({"observer": observer, "cmf": cmf.tolist()})


def planckian_spd(cct_kelvin) -> np.ndarray:
    """Planck's law spectral radiance, normalized to unit peak."""
    grid_m = unified_grid() * 1e-9
    c2 = 1.438776877e-2  # h c / k_B, in m*K
    spd = grid_m ** -5 / np.expm1(c2 / (grid_m * cct_kelvin))
    return spd / np.max(spd)


def illuminant_spd(illuminant) -> np.ndarray:
    """Illuminant spectral power distribution on the unified grid.

    *illuminant* is a name ("D65", "D50", "A", "E") or a custom SPD given
    as {"custom": [values on the unified grid]}.
    """
    if isinstance(illuminant, dict) and "custom" in illuminant:
        values = np.asarray(illuminant["custom"], dtype=np.float64)
        if values.shape != unified_grid().shape or not np.all(np.isfinite(values)) \
                or np.any(values < 0.0) or np.max(values) <= 0.0:
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "invalid custom illuminant SPD")
        return values / np.max(values)
    if illuminant == "E":
        return np.ones_like(unified_grid())
    if illuminant in _ILLUMINANT_CCT:
        return planckian_spd(_ILLUMINANT_CCT[illuminant])
    raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                              "unknown illuminant", {"illuminant": illuminant})


def illuminant_hash(illuminant) -> str:
    return content_hash({"illuminant": illuminant})


def integrate_xyz(spd_or_reflectance, cmf, illuminant=None) -> np.ndarray:
    """Trapezoidal integration to tristimulus values.

    If *illuminant* is given, the first argument is a reflectance and the
    result is normalized so the perfect diffuser yields Y = 1.
    """
    grid = unified_grid()
    if illuminant is not None:
        radiance = spd_or_reflectance * illuminant
    else:
        radiance = spd_or_reflectance
    xyz = np.trapezoid(radiance[:, None] * cmf, grid, axis=0)
    if illuminant is not None:
        norm = np.trapezoid(illuminant * cmf[:, 1], grid)
        if norm <= 0.0 or not np.isfinite(norm):
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "illuminant normalization failed")
        xyz = xyz / norm
    return xyz


def white_point_xyz(white_point, observer=OBSERVER_1931_2) -> np.ndarray:
    """White point as XYZ with Y = 1.

    Accepts an illuminant name (white = that illuminant's white), an xy
    chromaticity {"xy": [x, y]}, or explicit XYZ {"xyz": [X, Y, Z]}.
    """
    if isinstance(white_point, str):
        spd = illuminant_spd(white_point)
        cmf = color_matching_functions(observer)
        return integrate_xyz(spd, cmf)
    if isinstance(white_point, dict) and "xy" in white_point:
        x, y = (float(v) for v in white_point["xy"])
        if y <= 0.0:
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "white point y must be positive")
        return np.array([x / y, 1.0, (1.0 - x - y) / y], dtype=np.float64)
    if isinstance(white_point, dict) and "xyz" in white_point:
        xyz = np.asarray(white_point["xyz"], dtype=np.float64)
        if xyz.shape != (3,) or xyz[1] <= 0.0 or not np.all(np.isfinite(xyz)):
            raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                      "invalid white point XYZ")
        return xyz / xyz[1]
    raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                              "unknown white point", {"white_point": white_point})


def white_point_hash(white_point) -> str:
    return content_hash({"white_point": white_point})


def illuminant_white_xyz(illuminant, observer=OBSERVER_1931_2) -> np.ndarray:
    """Tristimulus white (Y = 1) of an illuminant SPD."""
    spd = illuminant_spd(illuminant)
    cmf = color_matching_functions(observer)
    return integrate_xyz(spd, cmf)


def adaptation_matrix(strategy, src_white_xyz, dst_white_xyz) -> np.ndarray:
    """Chromatic adaptation matrix mapping XYZ under src white to dst white."""
    if strategy == ADAPT_NONE:
        return np.eye(3)
    if strategy == ADAPT_BRADFORD:
        cone = BRADFORD_M
    elif strategy == ADAPT_VON_KRIES:
        cone = VON_KRIES_M
    else:
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "unknown adaptation strategy",
                                  {"adaptation": strategy})
    src = cone @ np.asarray(src_white_xyz, dtype=np.float64)
    dst = cone @ np.asarray(dst_white_xyz, dtype=np.float64)
    if np.any(src == 0.0):
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "degenerate source white point")
    scale = np.diag(dst / src)
    return np.linalg.inv(cone) @ scale @ cone


def adaptation_matrix_hash(strategy, src_white_xyz, dst_white_xyz) -> str:
    return content_hash({
        "adaptation": strategy,
        "src_white": [float(v) for v in src_white_xyz],
        "dst_white": [float(v) for v in dst_white_xyz],
    })


def rgb_to_xyz_matrix(space) -> np.ndarray:
    _check_space(space)
    return np.linalg.inv(XYZ_TO_RGB[space])


def xyz_to_rgb_matrix(space) -> np.ndarray:
    _check_space(space)
    return XYZ_TO_RGB[space]


def _check_space(space):
    if space not in KNOWN_SPACES:
        raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                  "unknown target color space", {"space": space})


def encode_transfer(linear, space) -> np.ndarray:
    """Encode linear RGB with the target space transfer function.

    Both sRGB and Display-P3 use the sRGB piecewise transfer function.
    """
    _check_space(space)
    linear = np.asarray(linear, dtype=np.float64)
    encoded = np.where(
        linear <= 0.0031308,
        12.92 * linear,
        1.055 * np.power(np.clip(linear, 0.0, None), 1.0 / 2.4) - 0.055,
    )
    return encoded


def decode_transfer(encoded, space) -> np.ndarray:
    _check_space(space)
    encoded = np.asarray(encoded, dtype=np.float64)
    return np.where(
        encoded <= 0.04045,
        encoded / 12.92,
        np.power(np.clip((encoded + 0.055) / 1.055, 0.0, None), 2.4),
    )
