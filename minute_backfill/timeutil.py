"""UTC timestamp and minute-window helpers.

All event times are represented as integer microseconds since the Unix epoch.
Using integers avoids float rounding in watermarks and window boundaries.
"""

from __future__ import annotations

import calendar
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP

MICROS_PER_SECOND = 1_000_000
MICROS_PER_MINUTE = 60 * MICROS_PER_SECOND
CENT = Decimal("0.01")


def parse_time_us(value: object, *, field_name: str = "event_time") -> int:
    if isinstance(value, datetime):
        dt = value
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        else:
            dt = dt.astimezone(timezone.utc)
        return calendar.timegm(dt.utctimetuple()) * MICROS_PER_SECOND + dt.microsecond
    if not isinstance(value, str):
        raise ValueError(f"{field_name} must be an ISO-8601 string or datetime")
    text = value.strip()
    if not text:
        raise ValueError(f"{field_name} is empty")
    if text.endswith(("Z", "z")):
        text = text[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(text)
    except ValueError as exc:
        raise ValueError(f"{field_name} is not ISO-8601: {value!r}") from exc
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    else:
        dt = dt.astimezone(timezone.utc)
    return calendar.timegm(dt.utctimetuple()) * MICROS_PER_SECOND + dt.microsecond


def format_time_us(value: int) -> str:
    seconds, micros = divmod(int(value), MICROS_PER_SECOND)
    dt = datetime.fromtimestamp(seconds, tz=timezone.utc).replace(microsecond=micros)
    return dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond:06d}Z"


def minute_start_us(value: int) -> int:
    return value - (value % MICROS_PER_MINUTE)


def minute_end_us(window_start: int) -> int:
    return window_start + MICROS_PER_MINUTE


def parse_amount_minor(value: object, *, field_name: str = "amount") -> int:
    """Parse a decimal(18,2) amount into integer cents without float math."""

    if isinstance(value, bool):
        raise ValueError(f"{field_name} must be a decimal value, not bool")
    if isinstance(value, Decimal):
        dec = value
    elif isinstance(value, int):
        dec = Decimal(value)
    elif isinstance(value, str):
        text = value.strip()
        if not text:
            raise ValueError(f"{field_name} is empty")
        try:
            dec = Decimal(text)
        except InvalidOperation as exc:
            raise ValueError(f"{field_name} is not a decimal: {value!r}") from exc
    else:
        raise ValueError(f"{field_name} must be a decimal string, got {type(value).__name__}")

    if not dec.is_finite():
        raise ValueError(f"{field_name} must be finite")
    # A value with more than two fractional digits is a manifest/data mismatch.
    if dec.as_tuple().exponent < -2:
        raise ValueError(f"{field_name} has more than two fractional digits: {value!r}")
    scaled = dec * 100
    if scaled != scaled.to_integral_value():
        raise ValueError(f"{field_name} cannot be represented in cents: {value!r}")
    minor = int(scaled)
    # decimal(18,2): absolute value < 10^18 yuan.
    if abs(minor) >= 10**20:
        raise ValueError(f"{field_name} exceeds decimal(18,2) range")
    return minor


def format_amount_minor(value: int) -> str:
    sign = "-" if value < 0 else ""
    value = abs(value)
    return f"{sign}{value // 100}.{value % 100:02d}"


def format_amount_decimal(value: int) -> str:
    return format_amount_minor(value)


def quantize_amount(value: Decimal) -> Decimal:
    try:
        return value.quantize(CENT, rounding=ROUND_HALF_UP)
    except InvalidOperation as exc:
        raise ValueError(f"invalid amount: {value!r}") from exc
