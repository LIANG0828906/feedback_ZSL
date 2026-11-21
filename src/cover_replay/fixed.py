"""Decimal-backed fixed-point helpers."""

from __future__ import annotations

from decimal import (
    ROUND_HALF_EVEN,
    ROUND_HALF_UP,
    Decimal,
    InvalidOperation,
    localcontext,
)
from typing import Any

from .errors import E_REPLAY_INVALID, ReplayError

_ROUNDING = {
    "half_even": ROUND_HALF_EVEN,
    "half_up": ROUND_HALF_UP,
}


def as_decimal(value: Any, *, field_path: str, record_index: int | None = None) -> Decimal:
    if isinstance(value, bool) or value is None:
        raise ReplayError(
            E_REPLAY_INVALID,
            "expected a finite decimal number",
            record_index,
            field_path,
        )
    if isinstance(value, Decimal):
        result = value
    elif isinstance(value, (int, float, str)):
        try:
            result = Decimal(str(value).strip())
        except (InvalidOperation, ValueError):
            raise ReplayError(
                E_REPLAY_INVALID,
                "expected a decimal number",
                record_index,
                field_path,
            ) from None
    else:
        raise ReplayError(
            E_REPLAY_INVALID,
            "expected a decimal number",
            record_index,
            field_path,
        )
    if not result.is_finite():
        raise ReplayError(
            E_REPLAY_INVALID,
            "number must be finite",
            record_index,
            field_path,
        )
    return result


def quantum_for(decimals: int) -> Decimal:
    return Decimal(1).scaleb(-decimals)


def quantize_units(
    value: Decimal,
    decimals: int,
    mode: str = "half_even",
) -> int:
    try:
        rounding = _ROUNDING[mode]
    except KeyError:
        raise ValueError(f"unsupported quantization mode: {mode}") from None
    quantum = quantum_for(decimals)
    with localcontext() as context:
        context.prec = max(50, decimals + 40)
        scaled = (value / quantum).to_integral_value(rounding=rounding)
    return int(scaled)


def units_decimal(units: int, decimals: int) -> Decimal:
    return Decimal(units) * quantum_for(decimals)


def format_units(units: int, decimals: int) -> str:
    if decimals == 0:
        return str(units)
    sign = "-" if units < 0 else ""
    absolute = abs(units)
    scale = 10**decimals
    whole, fraction = divmod(absolute, scale)
    return f"{sign}{whole}.{fraction:0{decimals}d}"


def format_decimal(value: Decimal, decimals: int) -> str:
    quantum = quantum_for(decimals)
    with localcontext() as context:
        context.prec = max(50, decimals + 40)
        rounded = value.quantize(quantum, rounding=ROUND_HALF_EVEN)
    return f"{rounded:.{decimals}f}"


def quantization_error_bound(operations: int, decimals: int) -> str:
    bound = Decimal(operations) * quantum_for(decimals) / Decimal(2)
    if decimals == 0 and bound == bound.to_integral_value():
        return str(int(bound))
    exponent = max(decimals + 1, 1)
    with localcontext() as context:
        context.prec = max(50, exponent + 20)
        bound = bound.quantize(Decimal(1).scaleb(-exponent), rounding=ROUND_HALF_EVEN)
    return f"{bound:.{exponent}f}"

