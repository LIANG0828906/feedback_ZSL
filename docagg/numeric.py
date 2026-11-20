from __future__ import annotations

import math
import re
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation, localcontext
from fractions import Fraction
from typing import Any

from .errors import ErrorCode, NumericParseError


AUDIT_PRECISION = 100
_DECIMAL_RE = re.compile(
    r"^[+-]?(?:(?:\d+(?:\.\d*)?)|(?:\.\d+))(?:[eE][+-]?\d+)?$"
)


@dataclass(frozen=True)
class ParsedNumber:
    raw: Any
    source_type: str
    exact: Fraction
    decimal_exact: Decimal
    float_value: float
    scale: int | None
    precision_digits: int
    conversion_error: Decimal
    downgraded: bool
    precision_path: tuple[str, ...]


def decimal_from_fraction(value: Fraction, precision: int = AUDIT_PRECISION) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = precision + 10
        return Decimal(value.numerator) / Decimal(value.denominator)


def decimal_from_float_exact(value: float) -> Decimal:
    return Decimal.from_float(value)


def _validate_float(raw: Any) -> float:
    if not math.isfinite(raw):
        raise NumericParseError(
            ErrorCode.TYPE_INCOMPARABLE,
            "非有限浮点值不可参与聚合",
            raw=raw,
        )
    return float(raw)


def _to_finite_float(value: Decimal, raw: Any) -> float:
    try:
        converted = float(value)
    except (OverflowError, ValueError) as exc:
        raise NumericParseError(
            ErrorCode.NUMERIC_OVERFLOW,
            "数值超出 IEEE 双精度可表示范围，拒绝静默截断",
            raw=raw,
            details={"source_type": "decimal"},
        ) from exc
    if not math.isfinite(converted):
        raise NumericParseError(
            ErrorCode.NUMERIC_OVERFLOW,
            "数值超出 IEEE 双精度可表示范围，拒绝静默截断",
            raw=raw,
            details={"source_type": "decimal"},
        )
    return converted


def parse_numeric(value: Any) -> ParsedNumber:
    """Parse JSON-compatible numeric values without losing source precision.

    JSON booleans are rejected explicitly even though bool is an int subclass.
    Decimal strings retain their declared scale and significant digit count.
    """

    if isinstance(value, bool):
        raise NumericParseError(
            ErrorCode.TYPE_INCOMPARABLE,
            "布尔值不能作为数值字段参与聚合",
            raw=value,
        )

    if isinstance(value, int):
        exact = Fraction(value)
        decimal_exact = Decimal(value)
        float_value = _to_finite_float(decimal_exact, value)
        conversion_error = abs(decimal_exact - Decimal.from_float(float_value))
        downgraded = conversion_error != 0
        path = ["integer->binary64"]
        if downgraded:
            path.append("integer-precision-loss")
        return ParsedNumber(
            raw=value,
            source_type="integer",
            exact=exact,
            decimal_exact=decimal_exact,
            float_value=float_value,
            scale=0,
            precision_digits=len(str(abs(value))),
            conversion_error=conversion_error,
            downgraded=downgraded,
            precision_path=tuple(path),
        )

    if isinstance(value, float):
        float_value = _validate_float(value)
        decimal_exact = Decimal.from_float(float_value)
        return ParsedNumber(
            raw=value,
            source_type="float",
            exact=Fraction.from_float(float_value),
            decimal_exact=decimal_exact,
            float_value=float_value,
            scale=None,
            precision_digits=17,
            conversion_error=Decimal(0),
            downgraded=False,
            precision_path=("float->binary64",),
        )

    if isinstance(value, Decimal):
        decimal_exact = value
    elif isinstance(value, str):
        text = value.strip()
        if not _DECIMAL_RE.fullmatch(text):
            raise NumericParseError(
                ErrorCode.TYPE_INCOMPARABLE,
                "字符串不是定点小数或整数格式",
                raw=value,
            )
        try:
            decimal_exact = Decimal(text)
        except InvalidOperation as exc:
            raise NumericParseError(
                ErrorCode.TYPE_INCOMPARABLE,
                "无法解析定点小数字符串",
                raw=value,
            ) from exc
    else:
        raise NumericParseError(
            ErrorCode.TYPE_INCOMPARABLE,
            f"不支持的数值类型: {type(value).__name__}",
            raw=value,
            details={"python_type": type(value).__name__},
        )

    if not decimal_exact.is_finite():
        raise NumericParseError(
            ErrorCode.TYPE_INCOMPARABLE,
            "非有限定点值不可参与聚合",
            raw=value,
        )

    float_value = _to_finite_float(decimal_exact, value)
    tuple_value = decimal_exact.as_tuple()
    exponent = int(tuple_value.exponent)
    scale = max(0, -exponent)
    digit_count = max(1, len(tuple_value.digits))
    conversion_error = abs(decimal_exact - Decimal.from_float(float_value))
    downgraded = conversion_error != 0
    path = [f"decimal(scale={scale})->binary64"]
    if downgraded:
        path.append("decimal-precision-loss")
    return ParsedNumber(
        raw=value,
        source_type="decimal",
        exact=Fraction(decimal_exact),
        decimal_exact=decimal_exact,
        float_value=float_value,
        scale=scale,
        precision_digits=digit_count,
        conversion_error=conversion_error,
        downgraded=downgraded,
        precision_path=tuple(path),
    )


def canonical_sort_key(number: ParsedNumber) -> tuple[float, str, str]:
    return (
        abs(number.float_value),
        number.source_type,
        str(number.raw),
    )
