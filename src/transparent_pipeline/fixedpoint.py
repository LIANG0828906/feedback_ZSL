"""Fixed-point accumulation for path length and optical depth."""

from __future__ import annotations

from dataclasses import dataclass

from .errors import parameter_error

LENGTH_SCALE = 1_000_000_000
OPTICAL_SCALE = 1_000_000_000_000


def quantize_nonnegative(value: float, scale: int) -> int:
    if value < 0.0:
        raise parameter_error("fixedpoint.value", "fixed-point accumulation only accepts non-negative values")
    # floor(x + 0.5) is deterministic and does not depend on platform rounding mode.
    return int(value * scale + 0.5)


@dataclass
class FixedAccumulator:
    length_scale: int = LENGTH_SCALE
    optical_scale: int = OPTICAL_SCALE
    path_units: int = 0
    optical_units: int = 0
    raw_path_length: float = 0.0
    raw_optical_depth: float = 0.0

    def add_segment(self, length: float, extinction_coefficient: float) -> None:
        if length < 0.0 or extinction_coefficient < 0.0:
            raise parameter_error("fixedpoint.segment", "path length and extinction coefficient must be non-negative")
        self.raw_path_length += float(length)
        self.raw_optical_depth += float(length) * float(extinction_coefficient)
        self.path_units += quantize_nonnegative(self.raw_path_length, self.length_scale) - quantize_nonnegative(
            self.raw_path_length - float(length), self.length_scale
        )
        self.optical_units += quantize_nonnegative(self.raw_optical_depth, self.optical_scale) - quantize_nonnegative(
            self.raw_optical_depth - float(length) * float(extinction_coefficient), self.optical_scale
        )

    @property
    def path_length(self) -> float:
        return self.path_units / self.length_scale

    @property
    def optical_depth(self) -> float:
        return self.optical_units / self.optical_scale

    @property
    def path_quantization_error(self) -> float:
        return abs(self.path_units / self.length_scale - self.raw_path_length)

    @property
    def optical_quantization_error(self) -> float:
        return abs(self.optical_units / self.optical_scale - self.raw_optical_depth)


__all__ = [
    "FixedAccumulator",
    "LENGTH_SCALE",
    "OPTICAL_SCALE",
    "quantize_nonnegative",
]

