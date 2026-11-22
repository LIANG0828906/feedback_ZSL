"""Color science: response integration, white adaptation, gamut, and matrices."""

from __future__ import annotations

from functools import lru_cache
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .models import SceneMetadata

WAVELENGTHS_400_700_10 = np.arange(400.0, 701.0, 10.0, dtype=np.float64)

CIE_1931_2DEG_400_700_10 = np.array(
    [
        [0.01431, 0.000396, 0.06785],
        [0.04351, 0.001210, 0.20740],
        [0.13438, 0.004000, 0.64560],
        [0.28390, 0.011600, 1.38560],
        [0.34828, 0.023000, 1.74706],
        [0.33620, 0.038000, 1.77211],
        [0.29080, 0.060000, 1.66920],
        [0.19536, 0.091000, 1.28764],
        [0.09564, 0.139000, 0.81300],
        [0.03201, 0.208000, 0.46518],
        [0.00490, 0.323000, 0.27200],
        [0.00930, 0.503000, 0.15820],
        [0.06327, 0.710000, 0.07825],
        [0.16550, 0.862000, 0.04216],
        [0.29040, 0.954000, 0.02030],
        [0.43345, 0.994950, 0.00875],
        [0.59450, 0.995000, 0.00390],
        [0.76210, 0.978600, 0.00210],
        [0.91630, 0.952000, 0.00165],
        [1.02630, 0.870000, 0.00110],
        [1.06220, 0.757000, 0.00080],
        [1.00260, 0.631000, 0.00034],
        [0.85445, 0.503000, 0.00019],
        [0.64240, 0.381000, 0.00005],
        [0.44790, 0.265000, 0.00000],
        [0.28350, 0.175000, 0.00000],
        [0.16490, 0.107000, 0.00000],
        [0.08740, 0.061000, 0.00000],
        [0.04677, 0.032000, 0.00000],
        [0.02270, 0.017000, 0.00000],
        [0.01136, 0.008210, 0.00000],
    ],
    dtype=np.float64,
)

D65_XYZ = np.array([0.95047, 1.0, 1.08883], dtype=np.float64)
D50_XYZ = np.array([0.96422, 1.0, 0.82521], dtype=np.float64)

GAMUT_PRIMARIES_XY = {
    "sRGB": {
        "red": (0.6400, 0.3300),
        "green": (0.3000, 0.6000),
        "blue": (0.1500, 0.0600),
        "white": (0.3127, 0.3290),
    },
    "DisplayP3": {
        "red": (0.6800, 0.3200),
        "green": (0.2650, 0.6900),
        "blue": (0.1500, 0.0600),
        "white": (0.3127, 0.3290),
    },
    "Rec2020": {
        "red": (0.7080, 0.2920),
        "green": (0.1700, 0.7970),
        "blue": (0.1310, 0.0460),
        "white": (0.3127, 0.3290),
    },
}

_BRADFORD = np.array(
    [
        [0.8951, 0.2664, -0.1614],
        [-0.7502, 1.7135, 0.0367],
        [0.0389, -0.0685, 1.0296],
    ],
    dtype=np.float64,
)
_BRADFORD_INV = np.linalg.inv(_BRADFORD)


def xy_to_xyz(xy: tuple[float, float], luminance: float = 1.0) -> np.ndarray:
    x, y = xy
    return np.array([x * luminance / y, luminance, (1.0 - x - y) * luminance / y], dtype=np.float64)


@lru_cache(maxsize=8)
def rgb_to_xyz_matrix(gamut: str) -> np.ndarray:
    if gamut not in GAMUT_PRIMARIES_XY:
        raise WorkbenchError("E_PARA", "Unknown target gamut", {"field": "target_gamut", "value": gamut})
    entry = GAMUT_PRIMARIES_XY[gamut]
    columns = np.stack(
        [xy_to_xyz(entry[name]) for name in ("red", "green", "blue")],
        axis=1,
    )
    white = xy_to_xyz(entry["white"])
    scale = np.linalg.solve(columns, white)
    return columns * scale[np.newaxis, :]


@lru_cache(maxsize=8)
def xyz_to_rgb_matrix(gamut: str) -> np.ndarray:
    return np.linalg.inv(rgb_to_xyz_matrix(gamut))


def target_white_xyz(gamut: str) -> np.ndarray:
    if gamut not in GAMUT_PRIMARIES_XY:
        raise WorkbenchError("E_PARA", "Unknown target gamut", {"field": "target_gamut", "value": gamut})
    return xy_to_xyz(GAMUT_PRIMARIES_XY[gamut]["white"])


def bradford_adaptation(src_white_xyz: np.ndarray, dst_white_xyz: np.ndarray) -> np.ndarray:
    src = np.asarray(src_white_xyz, dtype=np.float64)
    dst = np.asarray(dst_white_xyz, dtype=np.float64)
    src_cone = _BRADFORD @ src
    dst_cone = _BRADFORD @ dst
    scale = np.diag(dst_cone / np.maximum(src_cone, 1e-12))
    return _BRADFORD_INV @ scale @ _BRADFORD


def compute_xyz_basis(
    responses: np.ndarray,
    cmf: np.ndarray = CIE_1931_2DEG_400_700_10,
    wavelength_step_nm: float = 10.0,
) -> np.ndarray:
    """Return a deterministic band-radiance -> XYZ quadrature basis.

    ``responses`` is (wavelength, band). The basis is the least-squares
    spectral reconstruction followed by CIE color matching integration.
    """
    response = np.asarray(responses, dtype=np.float64)
    if response.ndim != 2 or cmf.shape[0] != response.shape[0]:
        raise WorkbenchError("E_META", "Response matrix shape is invalid", {"field": "responses"})
    design = response.T * float(wavelength_step_nm)
    inverse = np.linalg.pinv(design)
    basis = inverse.T @ np.asarray(cmf, dtype=np.float64) * float(wavelength_step_nm)
    return basis.astype(np.float64)


def band_radiance_to_xyz(band_radiance: np.ndarray, xyz_basis: np.ndarray) -> np.ndarray:
    values = np.asarray(band_radiance, dtype=np.float64)
    basis = np.asarray(xyz_basis, dtype=np.float64)
    if values.ndim != 3 or values.shape[0] != basis.shape[0]:
        raise WorkbenchError(
            "E_DIM",
            "Band cube and XYZ basis dimensions do not match",
            {"band_shape": list(values.shape), "basis_shape": list(basis.shape)},
        )
    return np.einsum("bhw,bc->hwc", values, basis, optimize=True)


def adapt_xyz(xyz: np.ndarray, scene_white_xyz: np.ndarray, target_white: np.ndarray) -> np.ndarray:
    matrix = bradford_adaptation(scene_white_xyz, target_white)
    return np.asarray(xyz, dtype=np.float64) @ matrix.T


def xyz_to_target_rgb(xyz: np.ndarray, gamut: str) -> np.ndarray:
    return np.asarray(xyz, dtype=np.float64) @ xyz_to_rgb_matrix(gamut).T


def source_xyz_to_target_rgb(
    xyz: np.ndarray,
    scene_white_xyz: np.ndarray,
    gamut: str,
) -> np.ndarray:
    adapted = adapt_xyz(xyz, scene_white_xyz, target_white_xyz(gamut))
    return xyz_to_target_rgb(adapted, gamut)


def normalize_chroma(rgb: np.ndarray) -> np.ndarray:
    values = np.asarray(rgb, dtype=np.float64)
    luminance = (
        0.2126 * values[..., 0]
        + 0.7152 * values[..., 1]
        + 0.0722 * values[..., 2]
    )
    positive = luminance > 1e-12
    safe = np.where(positive, luminance, 1.0)
    result = values / safe[..., np.newaxis]
    if np.any(~positive):
        result = np.where(positive[..., np.newaxis], result, 1.0)
    return result


def normalize_chromaticity(rgb: np.ndarray) -> np.ndarray:
    """Scale each pixel so its largest channel is one.

    Gamut boundaries are exposure-independent. HDR values above one are not,
    by themselves, out of gamut; negative or chromaticity-exceeding channels
    are. This normalization makes upper-bound checks meaningful while retaining
    the full exposure range for later tone mapping.
    """
    values = np.asarray(rgb, dtype=np.float64)
    maximum = values.max(axis=2)
    positive = maximum > 1e-12
    safe = np.where(positive, maximum, 1.0)
    return np.where(positive[..., np.newaxis], values / safe[..., np.newaxis], 0.0)


def out_of_gamut_mask(rgb: np.ndarray, tolerance: float = 0.0) -> np.ndarray:
    values = np.asarray(rgb, dtype=np.float64)
    return (values < -tolerance) | (values > 1.0 + tolerance)


def gamut_report(mask: np.ndarray, values: np.ndarray, *, offset: tuple[int, int] = (0, 0)) -> dict[str, Any]:
    channel_mask = np.asarray(mask, dtype=bool)
    rgb = np.asarray(values, dtype=np.float64)
    locations = np.argwhere(channel_mask)
    pixel_locations = np.argwhere(channel_mask.any(axis=2))
    coordinates: list[dict[str, Any]] = []
    for y, x, channel in locations[:64]:
        coordinates.append(
            {
                "x": int(x + offset[0]),
                "y": int(y + offset[1]),
                "channel": "RGB"[int(channel)],
                "value": float(rgb[y, x, channel]),
            }
        )
    return {
        "pixel_count": int(pixel_locations.shape[0]),
        "channel_count": int(locations.shape[0]),
        "coordinates": coordinates,
        "first": coordinates[0] if coordinates else None,
    }


def compress_gamut(rgb: np.ndarray) -> np.ndarray:
    """Desaturate toward the luminance axis until RGB is inside the gamut."""
    values = np.asarray(rgb, dtype=np.float64)
    luma = (
        0.2126 * values[..., 0]
        + 0.7152 * values[..., 1]
        + 0.0722 * values[..., 2]
    )
    maximum = values.max(axis=2)
    minimum = values.min(axis=2)
    positive_room = np.maximum(maximum - luma, 1e-12)
    negative_room = np.maximum(luma - minimum, 1e-12)
    scale_high = np.where(maximum > 1.0, np.maximum(1.0 - luma, 0.0) / positive_room, 1.0)
    scale_low = np.where(minimum < 0.0, np.maximum(luma, 0.0) / negative_room, 1.0)
    scale = np.minimum(1.0, np.minimum(scale_high, scale_low))
    mapped = luma[..., np.newaxis] + (values - luma[..., np.newaxis]) * scale[..., np.newaxis]
    return np.clip(mapped, 0.0, 1.0)


def map_gamut(
    rgb: np.ndarray,
    intent: str,
    *,
    offset: tuple[int, int] = (0, 0),
    raise_on_out: bool = False,
) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
    allowed = {"clip", "compress", "strict"}
    if intent not in allowed:
        raise WorkbenchError(
            "E_PARA",
            "Invalid gamut mapping intent",
            {"field": "gamut_intent", "value": intent, "allowed": sorted(allowed)},
        )
    values = np.asarray(rgb, dtype=np.float64)
    mask = out_of_gamut_mask(values)
    report = gamut_report(mask, values, offset=offset)
    if intent == "strict" or raise_on_out:
        if report["channel_count"]:
            first = report["first"]
            raise WorkbenchError(
                "E_GAMUT",
                "Linear RGB contains out-of-gamut pixels",
                {
                    "x": first["x"],
                    "y": first["y"],
                    "channel": first["channel"],
                    "value": first["value"],
                    **report,
                },
            )
        mapped = np.clip(values, 0.0, 1.0)
    elif intent == "clip":
        mapped = np.clip(values, 0.0, 1.0)
    else:
        mapped = compress_gamut(values)
    channel_mask = mask.astype(np.uint8)
    return mapped, channel_mask, report


def reconstruct_linear_rgb(
    band_radiance: np.ndarray,
    metadata: SceneMetadata,
    target_gamut: str,
    *,
    intent: str = "compress",
    offset: tuple[int, int] = (0, 0),
) -> tuple[np.ndarray, np.ndarray, np.ndarray, dict[str, Any]]:
    """Reconstruct absolute linear RGB and Y-normalized, gamut-mapped chroma."""
    xyz = band_radiance_to_xyz(band_radiance, metadata.xyz_basis)
    absolute_rgb = source_xyz_to_target_rgb(xyz, metadata.scene_white_xyz, target_gamut)
    chromaticity = normalize_chromaticity(absolute_rgb)
    mapped, channel_mask, report = map_gamut(chromaticity, intent, offset=offset)
    mapped_luma = (
        0.2126 * mapped[..., 0]
        + 0.7152 * mapped[..., 1]
        + 0.0722 * mapped[..., 2]
    )
    positive = mapped_luma > 1e-12
    safe = np.where(positive, mapped_luma, 1.0)
    chroma = mapped / safe[..., np.newaxis]
    if np.any(~positive):
        chroma = np.where(positive[..., np.newaxis], chroma, 1.0)
    return absolute_rgb, chroma, channel_mask, report


def gamut_boundary_xy(gamut: str) -> np.ndarray:
    entry = GAMUT_PRIMARIES_XY[gamut]
    points = np.array(
        [entry["red"], entry["green"], entry["blue"], entry["red"]],
        dtype=np.float64,
    )
    return points
