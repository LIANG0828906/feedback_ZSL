from __future__ import annotations

from datetime import date, datetime, timezone
from decimal import Decimal, ROUND_HALF_UP, localcontext


MICROS_PER_SECOND = 1_000_000
MICROS_PER_DAY = 86_400 * MICROS_PER_SECOND
SCALE = 1_000_000


def ensure_utc_naive(value: datetime) -> datetime:
    if value.tzinfo is not None:
        value = value.astimezone(timezone.utc).replace(tzinfo=None)
    return value


def parse_datetime(value: str | datetime) -> datetime:
    if isinstance(value, datetime):
        return ensure_utc_naive(value)
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    return ensure_utc_naive(parsed)


def datetime_to_us(value: datetime) -> int:
    value = ensure_utc_naive(value)
    delta = value - datetime(1970, 1, 1)
    return (
        delta.days * MICROS_PER_DAY
        + delta.seconds * MICROS_PER_SECOND
        + delta.microseconds
    )


def us_to_datetime(value: int) -> datetime:
    return datetime(1970, 1, 1) + _timedelta(microseconds=int(value))


def _timedelta(**kwargs):
    from datetime import timedelta

    return timedelta(**kwargs)


def iso_us(value: int) -> str:
    return us_to_datetime(value).isoformat(timespec="seconds") + "Z"


def iso_dt(value: datetime) -> str:
    return ensure_utc_naive(value).isoformat(timespec="seconds") + "Z"


def parse_iso_us(value: str) -> int:
    return datetime_to_us(parse_datetime(value))


def floor_us(value_us: int, interval_seconds: int) -> int:
    interval_us = int(interval_seconds) * MICROS_PER_SECOND
    return (int(value_us) // interval_us) * interval_us


def window_end_us(window_start_us: int, interval_seconds: int) -> int:
    return int(window_start_us) + int(interval_seconds) * MICROS_PER_SECOND


def date_partition(value_us: int) -> str:
    return us_to_datetime(value_us).date().isoformat()


def fixed_from_number(value: float | int | str) -> int:
    if isinstance(value, int):
        return value * SCALE
    with localcontext() as ctx:
        ctx.prec = 40
        dec = Decimal(str(value))
        return int((dec * SCALE).to_integral_value(rounding=ROUND_HALF_UP))


def number_from_fixed(value: int) -> float:
    return float(Decimal(int(value)) / Decimal(SCALE))


def decimal_string(value: int) -> str:
    return f"{Decimal(int(value)) / Decimal(SCALE):.6f}"


def energy_per_100km(mileage_u: int, energy_u: int) -> str:
    if int(mileage_u) <= 0:
        return "0.000000"
    with localcontext() as ctx:
        ctx.prec = 40
        result = Decimal(int(energy_u)) * Decimal(100) / Decimal(int(mileage_u))
        return f"{result.quantize(Decimal('0.000001'), rounding=ROUND_HALF_UP):.6f}"


def parse_date(value: date | datetime | str) -> str:
    if isinstance(value, datetime):
        return ensure_utc_naive(value).date().isoformat()
    if isinstance(value, date):
        return value.isoformat()
    return parse_datetime(value).date().isoformat()

