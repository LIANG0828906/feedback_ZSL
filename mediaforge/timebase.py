from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from math import gcd
from typing import Any

from .errors import E_PARAM, E_PREC, fail


@dataclass(frozen=True, order=True)
class Timebase:
    num: int
    den: int

    def __post_init__(self) -> None:
        if isinstance(self.num, bool) or isinstance(self.den, bool):
            fail(E_PARAM, "$.timebase", "timebase values must be integers")
        if not isinstance(self.num, int) or not isinstance(self.den, int):
            fail(E_PARAM, "$.timebase", "timebase values must be integers")
        if self.num <= 0 or self.den <= 0:
            fail(E_PARAM, "$.timebase", "timebase numerator and denominator must be positive")
        divisor = gcd(self.num, self.den)
        object.__setattr__(self, "num", self.num // divisor)
        object.__setattr__(self, "den", self.den // divisor)

    @property
    def ticks_per_second(self) -> Fraction:
        return Fraction(self.den, self.num)

    def to_dict(self) -> dict[str, int]:
        return {"num": self.num, "den": self.den}

    @classmethod
    def from_dict(cls, value: Any, path: str = "$.timebase") -> "Timebase":
        if not isinstance(value, dict):
            fail(E_PARAM, path, "timebase must be an object")
        num = value.get("num")
        den = value.get("den")
        if not isinstance(num, int) or isinstance(num, bool) or not isinstance(den, int) or isinstance(den, bool):
            fail(E_PARAM, path, "timebase.num and timebase.den must be integers")
        return cls(num=num, den=den)

    def seconds(self, ticks: int) -> Fraction:
        return Fraction(ticks * self.num, self.den)

    def ticks_from_seconds(self, seconds: Fraction, path: str) -> int:
        value = Fraction(seconds) * self.den / self.num
        if value.denominator != 1:
            fail(
                E_PREC,
                path,
                "value cannot be represented exactly in the declared timebase",
                measured={"numerator": value.numerator, "denominator": value.denominator},
                declared=self.to_dict(),
            )
        return value.numerator

    def convert_ticks(self, ticks: int, target: "Timebase", path: str) -> int:
        seconds = self.seconds(ticks)
        return target.ticks_from_seconds(seconds, path)

    def to_units(self, ticks: int, units_per_second: int, path: str) -> int:
        value = self.seconds(ticks) * units_per_second
        if value.denominator != 1:
            fail(
                E_PREC,
                path,
                "time cannot be represented exactly in the target unit",
                measured={"numerator": value.numerator, "denominator": value.denominator},
                declared={"units_per_second": units_per_second},
            )
        return value.numerator

    def ticks_from_units(self, value: int, units_per_second: int, path: str) -> int:
        ticks = self.ticks_from_seconds(Fraction(value, units_per_second), path)
        return ticks


def fractional_seconds_to_ticks(value: Fraction, target: Timebase, path: str) -> int:
    return target.ticks_from_seconds(value, path)


def ticks_to_fraction(ticks: int, timebase: Timebase) -> Fraction:
    return timebase.seconds(ticks)
