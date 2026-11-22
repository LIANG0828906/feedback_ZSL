"""Deterministic bidirectional coordinate conversion.

Pixel-space integer quads <-> normalized float quads, using image width/height.
The normalized representation uses a declared decimal precision and a
parameter-selectable rounding mode, so round-trips are exact within one
quantization step (and exact at the default precision for our image sizes).
"""

from __future__ import annotations

from decimal import ROUND_CEILING, ROUND_FLOOR, ROUND_HALF_EVEN, ROUND_HALF_UP, Decimal

from .errors import CoordError
from .model import Quad

DEFAULT_PRECISION = 6

_ROUNDING_MODES = {
    "half_up": ROUND_HALF_UP,
    "half_even": ROUND_HALF_EVEN,
    "floor": ROUND_FLOOR,
    "ceil": ROUND_CEILING,
}


def _mode(rounding: str) -> str:
    if rounding not in _ROUNDING_MODES:
        raise ValueError(f"unknown rounding mode: {rounding!r}")
    return _ROUNDING_MODES[rounding]


def quantization_step(width: int, height: int, precision: int) -> float:
    """Maximum absolute pixel error of a pixel->norm->pixel round trip."""
    return max(width, height) / (10**precision)


def pixel_to_normalized(
    quad: Quad,
    width: int,
    height: int,
    *,
    precision: int = DEFAULT_PRECISION,
    rounding: str = "half_up",
    line_id: int = -1,
) -> tuple:
    mode = _mode(rounding)
    quant = Decimal(1).scaleb(-precision)
    out = []
    for idx, (px, py) in enumerate(quad):
        if not (0 <= px <= width) or not (0 <= py <= height):
            raise CoordError(
                "pixel point outside image bounds",
                line_id=line_id,
                point_index=idx,
            )
        nx = (Decimal(px) / Decimal(width)).quantize(quant, rounding=mode)
        ny = (Decimal(py) / Decimal(height)).quantize(quant, rounding=mode)
        out.append((float(nx), float(ny)))
    return tuple(out)


def normalized_to_pixel(
    nquad: tuple,
    width: int,
    height: int,
    *,
    rounding: str = "half_up",
    line_id: int = -1,
) -> Quad:
    mode = _mode(rounding)
    out = []
    for idx, (nx, ny) in enumerate(nquad):
        fx, fy = float(nx), float(ny)
        if not (0.0 <= fx <= 1.0) or not (0.0 <= fy <= 1.0):
            raise CoordError(
                "normalized point outside [0, 1]",
                line_id=line_id,
                point_index=idx,
            )
        px = int((Decimal(repr(fx)) * Decimal(width)).quantize(Decimal(1), rounding=mode))
        py = int((Decimal(repr(fy)) * Decimal(height)).quantize(Decimal(1), rounding=mode))
        if not (0 <= px <= width) or not (0 <= py <= height):
            raise CoordError(
                "converted pixel point outside image bounds",
                line_id=line_id,
                point_index=idx,
            )
        out.append((px, py))
    return (out[0], out[1], out[2], out[3])


def round_trip(
    quad: Quad,
    width: int,
    height: int,
    *,
    precision: int = DEFAULT_PRECISION,
    rounding: str = "half_up",
    line_id: int = -1,
) -> Quad:
    nq = pixel_to_normalized(
        quad, width, height, precision=precision, rounding=rounding, line_id=line_id
    )
    return normalized_to_pixel(nq, width, height, rounding=rounding, line_id=line_id)
