"""Piecewise-linear transfer function with float and fixed LUT paths."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Mapping, Sequence

import numpy as np

from .errors import E_MODE_INVALID, E_TF_INVALID, VolRenderError


PRECISION_MODES = ("float", "fixed")


@dataclass(frozen=True)
class ControlPoint:
    x: float
    opacity: float
    r: float
    g: float
    b: float

    @classmethod
    def coerce(cls, value: "ControlPoint | Mapping[str, float] | Sequence[float]") -> "ControlPoint":
        if isinstance(value, cls):
            return value
        try:
            if isinstance(value, Mapping):
                return cls(
                    float(value["x"]),
                    float(value["opacity"]),
                    float(value["r"]),
                    float(value["g"]),
                    float(value["b"]),
                )
            if len(value) != 5:
                raise ValueError("wrong control point arity")
            return cls(*(float(v) for v in value))
        except VolRenderError:
            raise
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise VolRenderError(
                E_TF_INVALID,
                "control points must contain numeric x, opacity, r, g, b",
            ) from exc

    def as_dict(self) -> dict[str, float]:
        return {
            "x": self.x,
            "opacity": self.opacity,
            "r": self.r,
            "g": self.g,
            "b": self.b,
        }


@dataclass(frozen=True)
class IntervalOpacityBound:
    index: int
    x0: float
    x1: float
    slope: float
    lut_domain_error: float
    value_quantization_error: float
    upper_bound: float

    def as_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "x0": self.x0,
            "x1": self.x1,
            "slope": self.slope,
            "lut_domain_error": self.lut_domain_error,
            "value_quantization_error": self.value_quantization_error,
            "upper_bound": self.upper_bound,
        }


@dataclass(frozen=True)
class LUTBundle:
    mode: str
    resolution: int
    opacity: np.ndarray
    color: np.ndarray
    opacity_quant_step: float
    color_quant_step: float
    domain_step: float
    interval_bounds: tuple[IntervalOpacityBound, ...]

    @property
    def interval_upper_bounds(self) -> np.ndarray:
        return np.asarray(
            [entry.upper_bound for entry in self.interval_bounds],
            dtype=np.float64,
        )


def _validate_unit(value: float, label: str) -> float:
    if not np.isfinite(value) or value < 0.0 or value > 1.0:
        raise VolRenderError(
            E_TF_INVALID,
            f"{label} must be a finite value in [0, 1]",
            details={"value": value},
        )
    return float(value)


class TransferFunction:
    """Validated piecewise-linear opacity/color transfer function."""

    def __init__(self, points: Iterable[ControlPoint | Mapping[str, float] | Sequence[float]]):
        coerced = tuple(ControlPoint.coerce(point) for point in points)
        self._validate(coerced)
        self._points = coerced

    @staticmethod
    def _validate(points: tuple[ControlPoint, ...]) -> None:
        if len(points) < 2:
            raise VolRenderError(
                E_TF_INVALID,
                "transfer function requires at least two control points",
            )
        previous = -np.inf
        for index, point in enumerate(points):
            if not np.isfinite(point.x) or point.x < 0.0 or point.x > 1.0:
                raise VolRenderError(
                    E_TF_INVALID,
                    "control point x must be finite and in [0, 1]",
                    details={"index": index, "x": point.x},
                )
            if point.x <= previous:
                raise VolRenderError(
                    E_TF_INVALID,
                    "control point x values must be strictly increasing",
                    details={"index": index, "x": point.x, "previous_x": previous},
                )
            previous = point.x
            _validate_unit(point.opacity, "opacity")
            _validate_unit(point.r, "red")
            _validate_unit(point.g, "green")
            _validate_unit(point.b, "blue")

    @property
    def points(self) -> tuple[ControlPoint, ...]:
        return self._points

    def as_list(self) -> list[dict[str, float]]:
        return [point.as_dict() for point in self._points]

    def with_points(
        self,
        points: Iterable[ControlPoint | Mapping[str, float] | Sequence[float]],
    ) -> "TransferFunction":
        """Validate and construct a replacement without mutating this object."""

        return TransferFunction(points)

    def _interpolated(self, x: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        xs = np.asarray([point.x for point in self._points], dtype=np.float64)
        opacity = np.asarray([point.opacity for point in self._points], dtype=np.float64)
        colors = np.asarray(
            [[point.r, point.g, point.b] for point in self._points],
            dtype=np.float64,
        )
        op_values = np.interp(x, xs, opacity)
        color_values = np.stack(
            [np.interp(x, xs, colors[:, channel]) for channel in range(3)],
            axis=-1,
        )
        return op_values, color_values

    def interval_opacity_bounds(
        self,
        *,
        resolution: int,
        opacity_quantization_error: float,
        domain_step: float,
    ) -> tuple[IntervalOpacityBound, ...]:
        bounds: list[IntervalOpacityBound] = []
        for index, (left, right) in enumerate(zip(self._points, self._points[1:])):
            width = right.x - left.x
            slope = abs(right.opacity - left.opacity) / width
            domain_error = slope * (0.5 * domain_step)
            upper = min(1.0, domain_error + opacity_quantization_error)
            bounds.append(
                IntervalOpacityBound(
                    index=index,
                    x0=left.x,
                    x1=right.x,
                    slope=float(slope),
                    lut_domain_error=float(domain_error),
                    value_quantization_error=float(opacity_quantization_error),
                    upper_bound=float(upper),
                )
            )
        return tuple(bounds)

    def build_lut(
        self,
        mode: str,
        *,
        resolution: int = 65536,
        fixed_bits: int = 12,
        fixed_color_bits: int = 12,
    ) -> LUTBundle:
        """Build one calibrated LUT for the requested precision mode."""

        if mode not in PRECISION_MODES:
            raise VolRenderError(
                E_MODE_INVALID,
                "precision mode must be 'float' or 'fixed'",
                details={"mode": mode},
            )
        if not isinstance(resolution, int) or resolution < 16:
            raise VolRenderError(E_MODE_INVALID, "LUT resolution must be at least 16")
        if not isinstance(fixed_bits, int) or not 1 <= fixed_bits <= 24:
            raise VolRenderError(E_MODE_INVALID, "fixed_bits must be in [1, 24]")
        if not isinstance(fixed_color_bits, int) or not 1 <= fixed_color_bits <= 24:
            raise VolRenderError(
                E_MODE_INVALID,
                "fixed_color_bits must be in [1, 24]",
            )

        x = np.linspace(0.0, 1.0, resolution, dtype=np.float64)
        opacity, color = self._interpolated(x)
        opacity = np.clip(opacity, 0.0, 1.0)
        color = np.clip(color, 0.0, 1.0)
        domain_step = 1.0 / float(resolution - 1)

        if mode == "fixed":
            opacity_levels = (1 << fixed_bits) - 1
            color_levels = (1 << fixed_color_bits) - 1
            opacity = np.round(opacity * opacity_levels) / opacity_levels
            color = np.round(color * color_levels) / color_levels
            opacity_step = 1.0 / opacity_levels
            color_step = 1.0 / color_levels
            opacity_error = 0.5 * opacity_step
        else:
            opacity = opacity.astype(np.float64, copy=True)
            color = color.astype(np.float64, copy=True)
            opacity_step = float(np.finfo(np.float64).eps)
            color_step = float(np.finfo(np.float64).eps)
            opacity_error = 0.5 * opacity_step

        interval_bounds = self.interval_opacity_bounds(
            resolution=resolution,
            opacity_quantization_error=opacity_error,
            domain_step=domain_step,
        )
        return LUTBundle(
            mode=mode,
            resolution=resolution,
            opacity=np.ascontiguousarray(opacity, dtype=np.float64),
            color=np.ascontiguousarray(color, dtype=np.float64),
            opacity_quant_step=float(opacity_step),
            color_quant_step=float(color_step),
            domain_step=float(domain_step),
            interval_bounds=interval_bounds,
        )


def sample_lut(bundle: LUTBundle, values: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Linearly interpolate a LUT at scalar values in [0, 1]."""

    x = np.clip(np.asarray(values, dtype=np.float64), 0.0, 1.0)
    index = x * (bundle.resolution - 1)
    lower = np.floor(index).astype(np.intp)
    upper = np.minimum(lower + 1, bundle.resolution - 1)
    fraction = index - lower
    opacity = bundle.opacity[lower] * (1.0 - fraction) + bundle.opacity[upper] * fraction
    color = (
        bundle.color[lower] * (1.0 - fraction[..., None])
        + bundle.color[upper] * fraction[..., None]
    )
    return opacity, color


def opacity_error_at(tf: TransferFunction, values: np.ndarray, bundle: LUTBundle) -> np.ndarray:
    """Select the interval-wise opacity error upper bound for each sample."""

    x = np.clip(np.asarray(values, dtype=np.float64), 0.0, 1.0)
    knots = np.asarray([point.x for point in tf.points], dtype=np.float64)
    bounds = bundle.interval_upper_bounds
    indices = np.searchsorted(knots, x, side="right") - 1
    indices = np.clip(indices, 0, len(bounds) - 1)
    return bounds[indices]
