"""Deterministic fixed-point and binary64 numeric backends."""

from __future__ import annotations

import math
from decimal import Decimal, ROUND_HALF_EVEN, localcontext
from typing import Any

from .model import decimal_string


PI = Decimal(
    "3.14159265358979323846264338327950288419716939937510"
    "58209749445923078164062862089986280348253421170679"
)
TWO_PI = PI * 2


def half_ulp(places: int) -> Decimal:
    """Half of one unit in the last configured decimal place."""

    return Decimal(5) * (Decimal(10) ** (-(places + 1)))


def quantize(value: Decimal, places: int) -> tuple[Decimal, str]:
    step = Decimal(1).scaleb(-places)
    quantized = value.quantize(step, rounding=ROUND_HALF_EVEN)
    if quantized > value:
        direction = "up"
    elif quantized < value:
        direction = "down"
    else:
        direction = "none"
    return quantized, direction


def normalize_angle(value: Decimal) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = 80
        reduced = value % TWO_PI
        if reduced > PI:
            reduced -= TWO_PI
        if reduced <= -PI:
            reduced += TWO_PI
        return +reduced


def _sin_cos_decimal(value: Decimal, places: int) -> tuple[Decimal, Decimal, Decimal]:
    """Taylor sin/cos with a conservative truncation bound."""

    precision = max(50, places + 28)
    with localcontext() as ctx:
        ctx.prec = precision
        x = normalize_angle(value)
        x2 = x * x

        sin_term = x
        sin_total = x
        n = 1
        threshold = Decimal(10) ** (-(places + 14))
        while abs(sin_term) > threshold and n < 200:
            sin_term = -sin_term * x2 / Decimal((2 * n) * (2 * n + 1))
            sin_total += sin_term
            n += 1

        cos_term = Decimal(1)
        cos_total = Decimal(1)
        n = 1
        while abs(cos_term) > threshold and n < 200:
            cos_term = -cos_term * x2 / Decimal((2 * n - 1) * (2 * n))
            cos_total += cos_term
            n += 1

        trig_error = Decimal(10) ** (-(places + 10))
        return +sin_total, +cos_total, trig_error


class VelocityCache:
    """Deterministic operation-parameter cache used by both numeric paths."""

    def __init__(self) -> None:
        self._fixed: dict[tuple[str, str, int], tuple[Decimal, Decimal, Decimal, Decimal, Decimal]] = {}
        self._float: dict[tuple[float, float], tuple[float, float]] = {}
        self.hits = 0
        self.misses = 0

    def fixed(
        self,
        speed: Decimal,
        heading: Decimal,
        places: int,
    ) -> tuple[Decimal, Decimal, Decimal, Decimal, Decimal, bool]:
        key = (decimal_string(speed), decimal_string(heading), places)
        cached = self._fixed.get(key)
        if cached is not None:
            self.hits += 1
            sin_value, cos_value, vx, vy, trig_error = cached
            return sin_value, cos_value, vx, vy, trig_error, True
        self.misses += 1
        sin_value, cos_value, trig_error = _sin_cos_decimal(heading, places)
        vx = speed * cos_value
        vy = speed * sin_value
        self._fixed[key] = (sin_value, cos_value, vx, vy, trig_error)
        return sin_value, cos_value, vx, vy, trig_error, False

    def binary64(self, speed: float, heading: float) -> tuple[float, float, bool]:
        key = (speed, heading)
        cached = self._float.get(key)
        if cached is not None:
            self.hits += 1
            return cached[0], cached[1], True
        self.misses += 1
        vx = speed * math.cos(heading)
        vy = speed * math.sin(heading)
        self._float[key] = (vx, vy)
        return vx, vy, False

    def stats(self) -> dict[str, int]:
        return {"hits": self.hits, "misses": self.misses}


def rounding_record(
    original: Decimal,
    quantized: Decimal,
    direction: str,
) -> dict[str, Any]:
    return {
        "original": decimal_string(original),
        "quantized": decimal_string(quantized),
        "direction": direction,
        "delta": decimal_string(quantized - original),
    }
