"""Explicit fixed-point and IEEE-754 binary64 evaluation modes."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, ROUND_HALF_EVEN, ROUND_HALF_UP
from typing import Any

import numpy as np

from .canonical import canonical_decimal
from .errors import E_SCENARIO_INVALID, KernelError


ROUNDING_MODES = {
    "half_even": ROUND_HALF_EVEN,
    "half_up": ROUND_HALF_UP,
}

FLOAT64_EPS = float(np.finfo(np.float64).eps)
FLOAT64_UNIT_ROUNDOFF = FLOAT64_EPS / 2.0


@dataclass(frozen=True)
class Quantized:
    value: Decimal
    direction: str
    error: Decimal

    def to_dict(self) -> dict[str, Any]:
        return {
            "value": canonical_decimal(self.value),
            "direction": self.direction,
            "error": canonical_decimal(self.error),
        }


@dataclass(frozen=True)
class Precision:
    mode: str
    decimal_places: int = 6
    rounding: str = "half_even"

    def __post_init__(self) -> None:
        if self.mode not in {"fixed", "float"}:
            raise KernelError(
                E_SCENARIO_INVALID,
                "precision mode must be 'fixed' or 'float'",
                {"mode": self.mode},
            )
        if not isinstance(self.decimal_places, int) or not 0 <= self.decimal_places <= 18:
            raise KernelError(
                E_SCENARIO_INVALID,
                "decimal_places must be an integer in [0, 18]",
                {"decimal_places": self.decimal_places},
            )
        if self.rounding not in ROUNDING_MODES:
            raise KernelError(
                E_SCENARIO_INVALID,
                "unsupported rounding mode",
                {"rounding": self.rounding},
            )

    def to_dict(self) -> dict[str, Any]:
        if self.mode == "fixed":
            return {
                "mode": "fixed",
                "decimal_places": self.decimal_places,
                "rounding": self.rounding,
                "quantum": canonical_decimal(Decimal(1).scaleb(-self.decimal_places)),
            }
        return {
            "mode": "float",
            "binary_format": "IEEE-754 binary64",
            "rounding": "nearest_even",
            "epsilon": FLOAT64_EPS.hex(),
            "unit_roundoff": FLOAT64_UNIT_ROUNDOFF.hex(),
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Precision":
        if not isinstance(value, dict):
            raise KernelError(E_SCENARIO_INVALID, "precision must be an object", {})
        mode = value.get("mode")
        if mode == "fixed":
            return cls(
                mode="fixed",
                decimal_places=value.get("decimal_places", 6),
                rounding=value.get("rounding", "half_even"),
            )
        if mode == "float":
            return cls(mode="float")
        raise KernelError(E_SCENARIO_INVALID, "missing precision mode", {})

    def quantize_decimal(self, value: Decimal) -> Quantized:
        if self.mode != "fixed":
            raise RuntimeError("quantize_decimal is only valid in fixed mode")
        quantum = Decimal(1).scaleb(-self.decimal_places)
        rounded = value.quantize(quantum, rounding=ROUNDING_MODES[self.rounding])
        if rounded > value:
            direction = "up"
        elif rounded < value:
            direction = "down"
        else:
            direction = "none"
        return Quantized(rounded, direction, abs(rounded - value))


def float64(value: Decimal | float | int | np.float64) -> np.float64:
    if isinstance(value, Decimal):
        result = np.float64(str(value))
    else:
        result = np.float64(value)
    if not np.isfinite(result):
        raise KernelError(
            E_SCENARIO_INVALID,
            "non-finite float64 coefficient",
            {"value": str(value)},
        )
    return result


def float64_local_error(result: np.float64, operand_scale: float = 1.0) -> np.float64:
    magnitude = max(abs(float(result)), abs(operand_scale), 1.0)
    return np.float64(FLOAT64_UNIT_ROUNDOFF * magnitude)
