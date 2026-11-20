from __future__ import annotations

from decimal import Decimal, ROUND_CEILING, ROUND_DOWN, ROUND_HALF_EVEN, ROUND_UP, localcontext
from typing import Any

from .config import RoundingSpec


_DECIMAL_ROUNDING = {
    "half_even": ROUND_HALF_EVEN,
    "toward_zero": ROUND_DOWN,
    "up": ROUND_UP,
    "ceiling": ROUND_CEILING,
}


def round_significant(value: Decimal, spec: RoundingSpec) -> Decimal:
    if not value.is_finite():
        raise ValueError("不能对非有限值舍入")
    if value.is_zero():
        return Decimal(0)
    adjusted = value.adjusted()
    quantum = Decimal(1).scaleb(adjusted - spec.significant_digits + 1)
    digits = len(value.as_tuple().digits)
    with localcontext() as ctx:
        ctx.prec = max(200, digits + abs(int(value.as_tuple().exponent)) + 20)
        ctx.Emax = 999999
        ctx.Emin = -999999
        return value.quantize(
            quantum,
            rounding=_DECIMAL_ROUNDING[spec.mode],
        )


def apply_rounding(
    value: float,
    spec: RoundingSpec | None,
) -> dict[str, Any]:
    unrounded_decimal = Decimal.from_float(value)
    if spec is None:
        return {
            "applied": False,
            "rounding_uncertainty": 0.0,
            "value_before_round": value,
            "value_before_round_text": format(value, ".17g"),
            "rounded_value": value,
            "rounded_value_text": format(value, ".17g"),
        }

    rounded = round_significant(unrounded_decimal, spec)
    rounded_float = float(rounded)
    if rounded_float == 0:
        rounded_float = 0.0
    quantum = Decimal(1).scaleb(
        unrounded_decimal.adjusted() - spec.significant_digits + 1
    )
    uncertainty = float(quantum)
    if spec.mode == "half_even":
        uncertainty /= 2.0
    return {
        "mode": spec.mode,
        "significant_digits": spec.significant_digits,
        "applied": True,
        "rounding_uncertainty": uncertainty,
        "value_before_round": value,
        "value_before_round_text": format(value, ".17g"),
        "rounded_value": rounded_float,
        "rounded_value_text": format(rounded, "f"),
        "rounding_stage": "after_error_estimation",
    }
