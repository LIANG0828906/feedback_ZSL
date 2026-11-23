"""Axis normalization and response-curve mathematics."""

from __future__ import annotations

import math
from bisect import bisect_right

from .errors import ParameterError
from .models import AxisSpec, CurveSpec

EPSILON = 1e-12


def validate_curve(curve: CurveSpec, field_name: str = "curve") -> None:
    if curve.kind not in {"linear", "power", "piecewise"}:
        raise ParameterError(
            f"{field_name}.kind is invalid",
            field=f"{field_name}.kind",
        )
    if not math.isfinite(curve.exponent) or not 0.1 <= curve.exponent <= 8.0:
        raise ParameterError(
            f"{field_name}.exponent must be within [0.1, 8.0]",
            field=f"{field_name}.exponent",
        )
    if curve.kind == "piecewise":
        if len(curve.points) < 2:
            raise ParameterError(
                f"{field_name}.points requires at least two points",
                field=f"{field_name}.points",
            )
        previous_x = -math.inf
        previous_y = -math.inf
        for index, (x, y) in enumerate(curve.points):
            if not 0.0 <= x <= 1.0 or not 0.0 <= y <= 1.0:
                raise ParameterError(
                    f"{field_name}.points[{index}] must be within [0, 1]",
                    field=f"{field_name}.points[{index}]",
                )
            if x <= previous_x or y < previous_y:
                raise ParameterError(
                    f"{field_name}.points must have increasing x and nondecreasing y",
                    field=f"{field_name}.points[{index}]",
                )
            previous_x = x
            previous_y = y


def _shape_positive(value: float, curve: CurveSpec) -> float:
    magnitude = min(max(abs(value), 0.0), 1.0)
    if curve.kind == "linear":
        shaped = magnitude
    elif curve.kind == "power":
        shaped = magnitude**curve.exponent
    else:
        points = curve.points
        if magnitude <= points[0][0]:
            shaped = points[0][1]
        elif magnitude >= points[-1][0]:
            shaped = points[-1][1]
        else:
            xs = [point[0] for point in points]
            right = bisect_right(xs, magnitude)
            x0, y0 = points[right - 1]
            x1, y1 = points[right]
            ratio = (magnitude - x0) / (x1 - x0)
            shaped = y0 + (y1 - y0) * ratio
    return min(max(shaped, 0.0), 1.0)


def apply_curve(value: float, curve: CurveSpec | None, *, bipolar: bool) -> float:
    if curve is None:
        curve = CurveSpec()
    validate_curve(curve)
    if bipolar:
        shaped = _shape_positive(value, curve)
        return math.copysign(shaped, value) if abs(value) > EPSILON else 0.0
    return _shape_positive(value, curve)


def normalize_axis(axis: AxisSpec, raw_value: float, *, apply_response: bool = True) -> tuple[float, float]:
    """Return ``(oriented_value, normalized_value)``.

    The steps are fixed: declared-range normalization, trigger direction,
    deadzone removal, axial inversion, then response curve.  A deadzone boundary
    is closed: a magnitude exactly at the deadzone is removed.
    """

    if not math.isfinite(raw_value):
        raise ParameterError(
            f"axis {axis.id!r} value must be finite",
            field="value",
            context={"axis": axis.id},
        )
    if raw_value < axis.minimum - EPSILON or raw_value > axis.maximum + EPSILON:
        raise ParameterError(
            f"axis {axis.id!r} value {raw_value} is outside "
            f"[{axis.minimum}, {axis.maximum}]",
            field="value",
            context={"axis": axis.id, "minimum": axis.minimum, "maximum": axis.maximum},
        )
    if not 0.0 <= axis.deadzone < 1.0:
        raise ParameterError(
            f"axis {axis.id!r} deadzone must be within [0, 1)",
            field="deadzone",
            context={"axis": axis.id, "deadzone": axis.deadzone},
        )

    if axis.bipolar:
        scale = max(abs(axis.minimum), abs(axis.maximum))
        normalized = raw_value / scale if scale else 0.0
        normalized = min(max(normalized, -1.0), 1.0)
        if axis.kind == "trigger" and axis.trigger_direction == "decreasing":
            normalized = -normalized
        magnitude = abs(normalized)
        if magnitude <= axis.deadzone:
            normalized = 0.0
        elif axis.deadzone > 0.0:
            normalized = math.copysign(
                (magnitude - axis.deadzone) / (1.0 - axis.deadzone),
                normalized,
            )
        if axis.inverted:
            normalized = -normalized
        result = (
            apply_curve(normalized, axis.curve, bipolar=True)
            if apply_response
            else normalized
        )
        return normalized, result

    span = axis.maximum - axis.minimum
    normalized = (raw_value - axis.minimum) / span if span else 0.0
    normalized = min(max(normalized, 0.0), 1.0)
    if axis.kind == "trigger" and axis.trigger_direction == "decreasing":
        normalized = 1.0 - normalized
    if normalized <= axis.deadzone:
        normalized = 0.0
    elif axis.deadzone > 0.0:
        normalized = (normalized - axis.deadzone) / (1.0 - axis.deadzone)
    if axis.inverted:
        normalized = 1.0 - normalized
    result = (
        apply_curve(normalized, axis.curve, bipolar=False)
        if apply_response
        else normalized
    )
    return normalized, result


def apply_binding_curve(axis: AxisSpec, oriented: float, curve: CurveSpec | None) -> float:
    if curve is None:
        return apply_curve(oriented, axis.curve, bipolar=axis.bipolar)
    return apply_curve(oriented, curve, bipolar=axis.bipolar)


__all__ = [
    "EPSILON",
    "apply_binding_curve",
    "apply_curve",
    "normalize_axis",
    "validate_curve",
]
