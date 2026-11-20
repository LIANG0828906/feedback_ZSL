from __future__ import annotations

from functools import cache, lru_cache
from typing import Any

import numpy as np
from numpy.typing import NDArray

from .formats import HDRHeader
from .model import ColorSpace, ConversionParams, ToneMap, TransferFunction
from .util import canonical_bytes, sha256_hex

INTERMEDIATE_DTYPE = np.float64
TOLERANCE_SCALE = 1e-7


@cache
def _primaries(color_space: ColorSpace) -> tuple[tuple[float, float], ...]:
    if color_space is ColorSpace.REC709:
        return ((0.640, 0.330), (0.300, 0.600), (0.150, 0.060))
    if color_space is ColorSpace.REC2020:
        return ((0.708, 0.292), (0.170, 0.797), (0.131, 0.046))
    if color_space is ColorSpace.DISPLAY_P3:
        return ((0.680, 0.320), (0.265, 0.690), (0.150, 0.060))
    raise ValueError(f"unsupported color space: {color_space}")


@cache
def _white_point(_: ColorSpace) -> tuple[float, float]:
    return (0.3127, 0.3290)


def _xy_to_xyz(xy: tuple[float, float]) -> NDArray[np.float64]:
    x, y = xy
    return np.array([x / y, 1.0, (1.0 - x - y) / y], dtype=INTERMEDIATE_DTYPE)


def _rgb_to_xyz_matrix(
    primaries: tuple[tuple[float, float], ...],
    white: tuple[float, float],
) -> NDArray[np.float64]:
    columns = []
    for x, y in primaries:
        columns.append([x / y, 1.0, (1.0 - x - y) / y])
    primary_matrix = np.asarray(columns, dtype=INTERMEDIATE_DTYPE).T
    white_xyz = _xy_to_xyz(white)
    scale = np.linalg.solve(primary_matrix, white_xyz)
    return primary_matrix @ np.diag(scale)


@lru_cache(maxsize=1)
def _bradford_matrix() -> NDArray[np.float64]:
    return np.array(
        [
            [0.8951, 0.2664, -0.1614],
            [-0.7502, 1.7135, 0.0367],
            [0.0389, -0.0685, 1.0296],
        ],
        dtype=INTERMEDIATE_DTYPE,
    )


def white_adaptation_matrix(
    source_white: tuple[float, float],
    target_white: tuple[float, float],
) -> NDArray[np.float64]:
    source_xyz = _xy_to_xyz(source_white)
    target_xyz = _xy_to_xyz(target_white)
    transform = _bradford_matrix()
    source_cone = transform @ source_xyz
    target_cone = transform @ target_xyz
    scale = target_cone / source_cone
    return np.linalg.inv(transform) @ np.diag(scale) @ transform


@cache
def conversion_matrix(
    source_color_space: ColorSpace, target_color_space: ColorSpace
) -> NDArray[np.float64]:
    source_to_xyz = _rgb_to_xyz_matrix(
        _primaries(source_color_space), _white_point(source_color_space)
    )
    target_to_xyz = _rgb_to_xyz_matrix(
        _primaries(target_color_space), _white_point(target_color_space)
    )
    adaptation = white_adaptation_matrix(
        _white_point(source_color_space), _white_point(target_color_space)
    )
    matrix = np.linalg.solve(target_to_xyz, adaptation @ source_to_xyz)
    return np.asarray(matrix, dtype=INTERMEDIATE_DTYPE)


def pq_eotf(encoded: NDArray[np.float64]) -> NDArray[np.float64]:
    """ST 2084 EOTF, returned as normalized 0..1 display luminance."""

    m1 = 2610.0 / 16384.0
    m2 = 2523.0 / 4096.0 * 128.0
    c1 = 3424.0 / 4096.0
    c2 = 2413.0 / 4096.0 * 32.0
    c3 = 2392.0 / 4096.0 * 32.0
    values = np.clip(encoded.astype(INTERMEDIATE_DTYPE, copy=False), 0.0, 1.0)
    power = np.power(values, 1.0 / m2, dtype=INTERMEDIATE_DTYPE)
    numerator = np.maximum(power - c1, 0.0)
    denominator = np.maximum(c2 - c3 * power, np.finfo(np.float64).tiny)
    return np.power(numerator / denominator, 1.0 / m1, dtype=INTERMEDIATE_DTYPE)


def pq_oetf(normalized_nits: NDArray[np.float64]) -> NDArray[np.float64]:
    """ST 2084 inverse EOTF. Inputs above 1 remain representable as >1 codes."""

    m1 = 2610.0 / 16384.0
    m2 = 2523.0 / 4096.0 * 128.0
    c1 = 3424.0 / 4096.0
    c2 = 2413.0 / 4096.0 * 32.0
    c3 = 2392.0 / 4096.0 * 32.0
    values = np.maximum(normalized_nits.astype(INTERMEDIATE_DTYPE, copy=False), 0.0)
    power = np.power(values, m1, dtype=INTERMEDIATE_DTYPE)
    return np.power((c1 + c2 * power) / (1.0 + c3 * power), m2, dtype=INTERMEDIATE_DTYPE)


def hlg_inverse_oetf(encoded: NDArray[np.float64]) -> NDArray[np.float64]:
    a = 0.17883277
    b = 0.28466892
    c = 0.55991073
    values = np.clip(encoded.astype(INTERMEDIATE_DTYPE, copy=False), 0.0, 1.0)
    low = np.square(values) / 3.0
    high = (np.exp((values - c) / a) + b) / 12.0
    return np.where(values <= 0.5, low, high)


def hlg_oetf(linear: NDArray[np.float64]) -> NDArray[np.float64]:
    a = 0.17883277
    b = 0.28466892
    c = 0.55991073
    values = np.maximum(linear.astype(INTERMEDIATE_DTYPE, copy=False), 0.0)
    low = np.sqrt(3.0 * values)
    low_mask = values <= 1.0 / 12.0
    high = np.empty_like(values)
    high_values = np.maximum(values[~low_mask], 1.0 / 12.0)
    high[~low_mask] = a * np.log(12.0 * high_values - b) + c
    return np.where(low_mask, low, high)


def decode_transfer(
    encoded: NDArray[np.float64],
    transfer: TransferFunction,
    source_peak_nits: float,
) -> NDArray[np.float64]:
    if transfer is TransferFunction.SDR_GAMMA:
        return np.power(np.clip(encoded, 0.0, 1.0), 2.4) * source_peak_nits
    if transfer is TransferFunction.PQ:
        return pq_eotf(encoded) * 10000.0
    if transfer is TransferFunction.HLG:
        return hlg_inverse_oetf(encoded) * source_peak_nits
    raise ValueError(f"unsupported transfer function: {transfer}")


def encode_transfer(
    nits: NDArray[np.float64],
    transfer: TransferFunction,
    target_peak_nits: float,
) -> NDArray[np.float64]:
    safe_nits = np.maximum(nits, 0.0)
    if transfer is TransferFunction.SDR_GAMMA:
        return np.power(safe_nits / target_peak_nits, 1.0 / 2.4)
    if transfer is TransferFunction.PQ:
        return pq_oetf(safe_nits / 10000.0)
    if transfer is TransferFunction.HLG:
        return hlg_oetf(safe_nits / target_peak_nits)
    raise ValueError(f"unsupported transfer function: {transfer}")


def _hable_curve(value: NDArray[np.float64]) -> NDArray[np.float64]:
    a = 0.15
    b = 0.50
    c = 0.10
    d = 0.20
    e = 0.02
    f = 0.30
    numerator = value * (a * value + c * b) + d * e
    denominator = value * (a * value + b) + d * f
    return numerator / denominator - e / f


_HABLE_WHITE = float(_hable_curve(np.array([1.0], dtype=INTERMEDIATE_DTYPE))[0])


def tone_map_rgb(
    linear_nits: NDArray[np.float64],
    source_peak_nits: float,
    target_peak_nits: float,
    curve: ToneMap,
) -> NDArray[np.float64]:
    if curve is ToneMap.NONE:
        return linear_nits
    normalized = np.maximum(linear_nits, 0.0) / source_peak_nits
    if curve is ToneMap.REINHARD:
        mapped = (2.0 * normalized) / (1.0 + normalized)
    elif curve is ToneMap.HABLE:
        mapped = _hable_curve(normalized) / _HABLE_WHITE
    else:
        raise ValueError(f"unsupported tone map: {curve}")
    return mapped * target_peak_nits


def build_intermediate_summary(
    header: HDRHeader, params: ConversionParams
) -> dict[str, Any]:
    matrix = conversion_matrix(header.color_space, params.target_color_space)
    matrix_values = np.round(matrix, 15).tolist()
    matrix_hash = sha256_hex(canonical_bytes(matrix_values))
    return {
        "intermediate_precision": "float64",
        "source": {
            "color_space": header.color_space.value,
            "transfer": header.transfer.value,
            "peak_luminance": header.peak_luminance,
            "white_point": list(_white_point(header.color_space)),
        },
        "target": {
            "color_space": params.target_color_space.value,
            "transfer": params.target_transfer.value,
            "peak_luminance": params.peak_luminance,
            "white_point": list(_white_point(params.target_color_space)),
        },
        "white_adaptation": "Bradford",
        "tone_map": params.tone_map.value,
        "color_matrix": matrix_values,
        "color_matrix_hash": matrix_hash,
        "transfer_notes": {
            "sdr_gamma": "pure 2.4 display gamma",
            "pq": "SMPTE ST 2084, 10000-nit normalization",
            "hlg": "BT.2100 inverse OETF/OETF with metadata peak",
        },
    }


def convert_frame(
    frame: NDArray[np.integer],
    header: HDRHeader,
    params: ConversionParams,
    frame_index: int,
) -> tuple[NDArray[np.uint8] | NDArray[np.uint16], dict[str, Any]]:
    """Convert one frame using float64 intermediates and return raw output codes."""

    max_code = header.max_code
    encoded = frame.astype(INTERMEDIATE_DTYPE) / float(max_code)
    source_linear_nits = decode_transfer(encoded, header.transfer, header.peak_luminance)
    matrix = conversion_matrix(header.color_space, params.target_color_space)
    target_linear_nits = np.einsum(
        "ij,hwj->hwi", matrix, source_linear_nits, optimize=True
    )
    tone_mapped = tone_map_rgb(
        target_linear_nits,
        header.peak_luminance,
        params.peak_luminance,
        params.tone_map,
    )
    gain = float(2.0 ** params.ev_for_frame(frame_index))
    exposed = tone_mapped * gain
    raw_codes = encode_transfer(exposed, params.target_transfer, params.peak_luminance)
    raw_codes = raw_codes * float(max_code)

    tolerance = max(1.0, float(max_code)) * TOLERANCE_SCALE
    clipped = np.clip(raw_codes, 0.0, float(max_code))
    rounded = np.rint(clipped)
    output_dtype = np.uint8 if header.bit_depth <= 8 else np.uint16
    output = rounded.astype(output_dtype)

    raw_min = float(raw_codes.min()) if raw_codes.size else 0.0
    raw_max = float(raw_codes.max()) if raw_codes.size else 0.0
    max_deviation = float(np.max(np.abs(raw_codes - rounded))) if raw_codes.size else 0.0
    max_clip_deviation = max(0.0, -raw_min, raw_max - float(max_code))
    clipped_samples = int(
        np.count_nonzero((raw_codes < -tolerance) | (raw_codes > float(max_code) + tolerance))
    )
    summary = {
        "frame": frame_index,
        "exposure_ev": params.ev_for_frame(frame_index),
        "raw_min_code": raw_min,
        "raw_max_code": raw_max,
        "max_code_deviation": max_deviation,
        "max_clip_deviation": max_clip_deviation,
        "clipped_samples": clipped_samples,
        "out_of_range": bool(
            raw_min < -tolerance or raw_max > float(max_code) + tolerance
        ),
        "overrange": bool(raw_max > float(max_code) + tolerance),
    }
    return output, summary

