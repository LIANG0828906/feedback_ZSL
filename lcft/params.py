"""Integer/rational layout parameters shared by every track type."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any

from .canonical import sha256_value
from .errors import param_error

ESTIMATOR_VERSION = "text-units-v1"


def fraction_text(value: Fraction) -> str:
    if value.denominator == 1:
        return str(value.numerator)
    denominator = value.denominator
    reduced = denominator
    while reduced % 2 == 0:
        reduced //= 2
    while reduced % 5 == 0:
        reduced //= 5
    if reduced != 1:
        return f"{value.numerator}/{value.denominator}"
    sign = "-" if value < 0 else ""
    numerator = abs(value.numerator)
    whole, remainder = divmod(numerator, denominator)
    digits: list[str] = []
    while remainder:
        remainder *= 10
        digit, remainder = divmod(remainder, denominator)
        digits.append(str(digit))
    text = f"{sign}{whole}"
    if digits:
        text += "." + "".join(digits).rstrip("0")
    return text


def _fraction(raw: Any, field: str, *, default: str | None = None) -> Fraction:
    if raw is None and default is not None:
        raw = default
    if isinstance(raw, bool) or isinstance(raw, (dict, list)):
        raise param_error(field, "parameter must be a rational number", value=repr(raw))
    try:
        if isinstance(raw, float):
            raw = str(raw)
        value = Fraction(raw)
    except (ValueError, ZeroDivisionError) as exc:
        raise param_error(field, "invalid rational parameter", value=repr(raw)) from exc
    if value < 0:
        raise param_error(field, "parameter must not be negative", value=fraction_text(value))
    return value


def _integer(raw: Any, field: str, default: int) -> int:
    if raw is None:
        return default
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise param_error(field, "parameter must be an integer", value=repr(raw))
    return raw


@dataclass(frozen=True)
class LayoutParams:
    min_display_seconds: Fraction = Fraction(6, 5)
    min_gap_seconds: Fraction = Fraction(2, 25)
    default_max_read_speed: Fraction = Fraction(14)
    speech_priority: int = 3
    description_min_seconds: Fraction = Fraction(1, 2)
    description_read_speed: Fraction = Fraction(14)
    description_priority: int = 2
    max_stretch_ratio: Fraction = Fraction(4)

    @classmethod
    def from_dict(cls, raw: dict[str, Any] | None) -> "LayoutParams":
        if raw is None:
            return cls()
        if not isinstance(raw, dict):
            raise param_error("params", "layout parameters must be an object")
        return cls(
            min_display_seconds=_fraction(
                raw.get("min_display_seconds"),
                "params.min_display_seconds",
                default="1.2",
            ),
            min_gap_seconds=_fraction(
                raw.get("min_gap_seconds"),
                "params.min_gap_seconds",
                default="0.08",
            ),
            default_max_read_speed=_fraction(
                raw.get("default_max_read_speed"),
                "params.default_max_read_speed",
                default="14",
            ),
            speech_priority=_integer(
                raw.get("speech_priority"), "params.speech_priority", 3
            ),
            description_min_seconds=_fraction(
                raw.get("description_min_seconds"),
                "params.description_min_seconds",
                default="0.5",
            ),
            description_read_speed=_fraction(
                raw.get("description_read_speed"),
                "params.description_read_speed",
                default="14",
            ),
            description_priority=_integer(
                raw.get("description_priority"), "params.description_priority", 2
            ),
            max_stretch_ratio=_fraction(
                raw.get("max_stretch_ratio"),
                "params.max_stretch_ratio",
                default="4",
            ),
        )

    def validate(self) -> None:
        if self.default_max_read_speed <= 0:
            raise param_error(
                "params.default_max_read_speed", "read speed must be positive"
            )
        if self.description_read_speed <= 0:
            raise param_error(
                "params.description_read_speed", "description read speed must be positive"
            )
        if self.max_stretch_ratio < 1:
            raise param_error(
                "params.max_stretch_ratio", "stretch ratio must be at least 1"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "min_display_seconds": fraction_text(self.min_display_seconds),
            "min_gap_seconds": fraction_text(self.min_gap_seconds),
            "default_max_read_speed": fraction_text(self.default_max_read_speed),
            "speech_priority": self.speech_priority,
            "description_min_seconds": fraction_text(self.description_min_seconds),
            "description_read_speed": fraction_text(self.description_read_speed),
            "description_priority": self.description_priority,
            "max_stretch_ratio": fraction_text(self.max_stretch_ratio),
            "estimator_version": ESTIMATOR_VERSION,
        }

    @property
    def hash(self) -> str:
        return sha256_value(self.to_dict())

    def seconds_to_ticks(self, seconds: Fraction, tps: int, *, mode: str = "round") -> int:
        value = seconds * tps
        if mode == "ceil":
            return -((-value.numerator) // value.denominator)
        if mode == "floor":
            return value.numerator // value.denominator
        return (value.numerator * 2 + value.denominator) // (value.denominator * 2)

