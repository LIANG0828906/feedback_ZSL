"""Deterministic color conversion chain with no execution-order state."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Sequence

from .binary import compute_block_hash, pack_samples, unpack_samples
from .config import JobConfig
from .errors import E_CONFIG_INVALID, error
from .model import Primaries, TileBlock, TileSequence, TransferCode


WhitePoint = tuple[float, float]

WHITE_POINTS: dict[str, WhitePoint] = {
    "d65": (0.3127, 0.3290),
    "d60": (0.32168, 0.33767),
    "d50": (0.3457, 0.3585),
}

PRIMARIES_XY: dict[Primaries, tuple[WhitePoint, WhitePoint, WhitePoint]] = {
    Primaries.REC709: ((0.64, 0.33), (0.30, 0.60), (0.15, 0.06)),
    Primaries.DISPLAY_P3: ((0.680, 0.320), (0.265, 0.690), (0.150, 0.060)),
    Primaries.REC2020: ((0.708, 0.292), (0.170, 0.797), (0.131, 0.046)),
}

_BRADFORD = (
    (0.8951, 0.2664, -0.1614),
    (-0.7502, 1.7135, 0.0367),
    (0.0389, -0.0685, 1.0296),
)
_BRADFORD_INV = (
    (0.9869929, -0.1470543, 0.1599627),
    (0.4323053, 0.5183603, 0.0492912),
    (-0.0085287, 0.0400428, 0.9684867),
)


def _mat_mul(a: Sequence[Sequence[float]], b: Sequence[Sequence[float]]) -> tuple[tuple[float, ...], ...]:
    return tuple(
        tuple(sum(a[row][k] * b[k][col] for k in range(3)) for col in range(3))
        for row in range(3)
    )


def _mat_vec(matrix: Sequence[Sequence[float]], vector: Sequence[float]) -> tuple[float, float, float]:
    return (
        matrix[0][0] * vector[0] + matrix[0][1] * vector[1] + matrix[0][2] * vector[2],
        matrix[1][0] * vector[0] + matrix[1][1] * vector[1] + matrix[1][2] * vector[2],
        matrix[2][0] * vector[0] + matrix[2][1] * vector[1] + matrix[2][2] * vector[2],
    )


def _mat_inv(matrix: Sequence[Sequence[float]]) -> tuple[tuple[float, ...], ...]:
    a, b, c = matrix[0]
    d, e, f = matrix[1]
    g, h, i = matrix[2]
    det = a * (e * i - f * h) - b * (d * i - f * g) + c * (d * h - e * g)
    if abs(det) < 1e-15:
        raise error(E_CONFIG_INVALID, "singular color matrix", field="primaries")
    inv_det = 1.0 / det
    return (
        ((e * i - f * h) * inv_det, (c * h - b * i) * inv_det, (b * f - c * e) * inv_det),
        ((f * g - d * i) * inv_det, (a * i - c * g) * inv_det, (c * d - a * f) * inv_det),
        ((d * h - e * g) * inv_det, (b * g - a * h) * inv_det, (a * e - b * d) * inv_det),
    )


def _rgb_to_xyz_matrix(primaries: Primaries, white_name: str) -> tuple[tuple[float, ...], ...]:
    red, green, blue = PRIMARIES_XY[primaries]
    white_x, white_y = WHITE_POINTS[white_name]
    columns = []
    for x, y in (red, green, blue):
        columns.append((x / y, 1.0, (1.0 - x - y) / y))
    # Solve for primary scale factors from the white chromaticity.
    base = (
        (columns[0][0], columns[1][0], columns[2][0]),
        (columns[0][1], columns[1][1], columns[2][1]),
        (columns[0][2], columns[1][2], columns[2][2]),
    )
    white_xyz = (white_x / white_y, 1.0, (1.0 - white_x - white_y) / white_y)
    scales = _mat_vec(_mat_inv(base), white_xyz)
    return tuple(
        tuple(base[row][col] * scales[col] for col in range(3))
        for row in range(3)
    )


def _bradford_adaptation(source_white: str, target_white: str) -> tuple[tuple[float, ...], ...] | None:
    if source_white == target_white:
        return None
    src_x, src_y = WHITE_POINTS[source_white]
    dst_x, dst_y = WHITE_POINTS[target_white]
    src_xyz = (src_x / src_y, 1.0, (1.0 - src_x - src_y) / src_y)
    dst_xyz = (dst_x / dst_y, 1.0, (1.0 - dst_x - dst_y) / dst_y)
    src_cone = _mat_vec(_BRADFORD, src_xyz)
    dst_cone = _mat_vec(_BRADFORD, dst_xyz)
    diagonal = (
        (dst_cone[0] / src_cone[0], 0.0, 0.0),
        (0.0, dst_cone[1] / src_cone[1], 0.0),
        (0.0, 0.0, dst_cone[2] / src_cone[2]),
    )
    return _mat_mul(_BRADFORD_INV, _mat_mul(diagonal, _BRADFORD))


def _sdr_eotf(value: float) -> float:
    if value < 0.0:
        return 0.0
    if value < 0.081:
        return value / 4.5
    return ((value + 0.099) / 1.099) ** (1.0 / 0.45)


def _sdr_oetf(value: float) -> float:
    value = min(max(value, 0.0), 1.0)
    if value < 0.018:
        return 4.5 * value
    return 1.099 * (value ** 0.45) - 0.099


_PQ_M1 = 2610.0 / 16384.0
_PQ_M2 = 2523.0 / 32.0
_PQ_C1 = 3424.0 / 4096.0
_PQ_C2 = 2413.0 / 128.0
_PQ_C3 = 2392.0 / 128.0


def _pq_eotf(value: float) -> float:
    value = min(max(value, 0.0), 1.0)
    p = value ** (1.0 / _PQ_M2)
    numerator = max(p - _PQ_C1, 0.0)
    denominator = _PQ_C2 - _PQ_C3 * p
    if denominator <= 0.0:
        return 10000.0
    return (numerator / denominator) ** (1.0 / _PQ_M1) * 10000.0


def _pq_oetf(nits: float) -> float:
    luminance = min(max(nits / 10000.0, 0.0), 1.0)
    powered = luminance ** _PQ_M1
    return ((_PQ_C1 + _PQ_C2 * powered) / (1.0 + _PQ_C3 * powered)) ** _PQ_M2


_HLG_A = 0.17883277
_HLG_B = 1.0 - 4.0 * _HLG_A
_HLG_C = 0.5 - _HLG_A * math.log(4.0 * _HLG_A)


def _hlg_eotf(value: float) -> float:
    value = min(max(value, 0.0), 1.0)
    if value <= 0.5:
        return value * value / 3.0
    return (math.exp((value - _HLG_C) / _HLG_A) + _HLG_B) / 12.0


def _hlg_oetf(value: float) -> float:
    value = min(max(value, 0.0), 1.0)
    if value <= 1.0 / 12.0:
        return math.sqrt(3.0 * value)
    return _HLG_A * math.log(12.0 * value - _HLG_B) + _HLG_C


def decode_transfer(code: int, transfer: TransferCode, peak_nits: float) -> float:
    """Decode a normalized code to absolute display nits."""

    if transfer == TransferCode.PQ:
        return _pq_eotf(code)
    if transfer == TransferCode.HLG:
        return _hlg_eotf(code) * peak_nits
    return _sdr_eotf(code) * peak_nits


def encode_transfer(nits: float, transfer: TransferCode, peak_nits: float) -> float:
    """Encode absolute display nits to a normalized target code value."""

    if transfer == TransferCode.PQ:
        return _pq_oetf(nits)
    relative = nits / peak_nits
    if transfer == TransferCode.HLG:
        return _hlg_oetf(relative)
    return _sdr_oetf(relative)


def apply_tone_map(rgb_nits: tuple[float, float, float], peak_nits: float, curve: str) -> tuple[float, float, float]:
    red = min(max(rgb_nits[0], 0.0), peak_nits)
    green = min(max(rgb_nits[1], 0.0), peak_nits)
    blue = min(max(rgb_nits[2], 0.0), peak_nits)
    if curve == "none":
        return red, green, blue
    luminance = 0.2126 * red + 0.7152 * green + 0.0722 * blue
    if luminance <= 1e-15:
        return 0.0, 0.0, 0.0
    x = luminance / peak_nits
    if curve == "reinhard":
        mapped = x / (1.0 + x)
    elif curve == "aces":
        mapped = (x * (2.51 * x + 0.03)) / (x * (2.43 * x + 0.59) + 0.14)
        mapped = min(max(mapped, 0.0), 1.0)
    else:
        raise error(E_CONFIG_INVALID, "unknown tone map curve", field="tone_map_curve", actual=curve)
    scale = (mapped * peak_nits) / luminance
    return (
        min(max(red * scale, 0.0), peak_nits),
        min(max(green * scale, 0.0), peak_nits),
        min(max(blue * scale, 0.0), peak_nits),
    )


@dataclass(frozen=True)
class ColorPipeline:
    source_primaries: Primaries
    target_primaries: Primaries
    source_transfer: TransferCode
    target_transfer: TransferCode
    source_peak_nits: float
    target_peak_nits: float
    source_to_xyz: tuple[tuple[float, ...], ...]
    xyz_to_target: tuple[tuple[float, ...], ...]
    adaptation: tuple[tuple[float, ...], ...] | None

    @classmethod
    def from_config(cls, config: JobConfig) -> "ColorPipeline":
        source_matrix = _rgb_to_xyz_matrix(config.source.primaries, config.source.white_point)
        target_matrix = _rgb_to_xyz_matrix(config.target.primaries, config.target.white_point)
        return cls(
            source_primaries=config.source.primaries,
            target_primaries=config.target.primaries,
            source_transfer=config.source.transfer,
            target_transfer=config.target.transfer,
            source_peak_nits=config.source.peak_nits,
            target_peak_nits=config.target.peak_nits,
            source_to_xyz=source_matrix,
            xyz_to_target=_mat_inv(target_matrix),
            adaptation=_bradford_adaptation(config.source.white_point, config.target.white_point),
        )

    def convert_pixel(self, rgb_code: tuple[int, int, int], max_code: int, exposure_stops: float, curve: str) -> tuple[float, float, float]:
        encoded = (rgb_code[0] / max_code, rgb_code[1] / max_code, rgb_code[2] / max_code)
        linear_nits = (
            decode_transfer(encoded[0], self.source_transfer, self.source_peak_nits),
            decode_transfer(encoded[1], self.source_transfer, self.source_peak_nits),
            decode_transfer(encoded[2], self.source_transfer, self.source_peak_nits),
        )
        xyz = _mat_vec(self.source_to_xyz, linear_nits)
        if self.adaptation is not None:
            xyz = _mat_vec(self.adaptation, xyz)
        target_nits = _mat_vec(self.xyz_to_target, xyz)
        multiplier = 2.0 ** exposure_stops
        exposed = (
            max(target_nits[0], 0.0) * multiplier,
            max(target_nits[1], 0.0) * multiplier,
            max(target_nits[2], 0.0) * multiplier,
        )
        mapped = apply_tone_map(exposed, self.target_peak_nits, curve)
        return (
            encode_transfer(mapped[0], self.target_transfer, self.target_peak_nits),
            encode_transfer(mapped[1], self.target_transfer, self.target_peak_nits),
            encode_transfer(mapped[2], self.target_transfer, self.target_peak_nits),
        )


@dataclass(frozen=True)
class TileResult:
    tile_id: int
    data: bytes
    exposure: float
    max_code_delta: int
    content_hash: bytes
    tone_map_curve: str
    source_hash: bytes
    profile: str = "full"


def convert_tile(
    sequence: TileSequence,
    block: TileBlock,
    config: JobConfig,
    pipeline: ColorPipeline | None = None,
    profile: str = "full",
) -> TileResult:
    """Convert one tile. The result depends only on arguments, never on wall time."""

    header = sequence.header
    if header.channels != 3:
        raise error(
            E_CONFIG_INVALID,
            "color conversion currently requires exactly three channels",
            field="channels",
            actual=header.channels,
        )
    if profile not in ("full", "banded", "streaming"):
        raise error(E_CONFIG_INVALID, "unknown conversion memory profile", field="profile", actual=profile)
    pipeline = pipeline or ColorPipeline.from_config(config)
    exposure = config.effective_exposure(block.exposure, block.tile_id)
    curve = config.effective_curve(block.tile_id)
    max_code = (1 << header.bit_depth) - 1
    source_values = unpack_samples(block.data, header.bit_depth)
    output_values: list[int] = []
    max_delta = 0
    pixel_count = block.width * block.height
    for pixel in range(pixel_count):
        offset = pixel * 3
        encoded = pipeline.convert_pixel(
            (source_values[offset], source_values[offset + 1], source_values[offset + 2]),
            max_code,
            exposure,
            curve,
        )
        for value in encoded:
            scaled = min(max(value, 0.0), 1.0) * max_code
            quantized = int(math.floor(scaled + 0.5))
            if quantized > max_code:
                quantized = max_code
            delta = abs(scaled - quantized)
            if delta > max_delta:
                max_delta = delta
            output_values.append(quantized)
    payload = pack_samples(output_values, header.bit_depth)
    content_hash = compute_block_hash(
        block.tile_id,
        block.x,
        block.y,
        block.width,
        block.height,
        exposure,
        block.sample_count,
        payload,
    )
    return TileResult(
        tile_id=block.tile_id,
        data=payload,
        exposure=exposure,
        max_code_delta=max_delta,
        content_hash=content_hash,
        tone_map_curve=curve,
        source_hash=block.content_hash,
        profile=profile,
    )


def tile_fingerprint(sequence: TileSequence, block: TileBlock, config: JobConfig) -> str:
    """Stable cache key for one tile under one complete parameter submission."""

    header = sequence.header
    payload = {
        "source_hash": block.content_hash.hex(),
        "tile_id": block.tile_id,
        "geometry": [block.x, block.y, block.width, block.height],
        "file_exposure": block.exposure,
        "effective_exposure": config.effective_exposure(block.exposure, block.tile_id),
        "curve": config.effective_curve(block.tile_id),
        "bit_depth": header.bit_depth,
        "channels": header.channels,
        "source": config.source.to_dict(),
        "target": config.target.to_dict(),
        "observation": config.observation.to_dict(),
    }
    import json

    return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
