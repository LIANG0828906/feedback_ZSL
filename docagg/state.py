from __future__ import annotations

import math
import sys
from dataclasses import dataclass, field
from decimal import Decimal, localcontext
from typing import Any, Iterable

from .numeric import AUDIT_PRECISION, ParsedNumber, canonical_sort_key


U = sys.float_info.epsilon / 2.0
TINY = float.fromhex("0x0.0000000000001p-1022")


def _dadd(left: Decimal, right: Decimal) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = AUDIT_PRECISION
        return left + right


def _dsub(left: Decimal, right: Decimal) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = AUDIT_PRECISION
        return left - right


def _dmul(left: Decimal, right: Decimal) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = AUDIT_PRECISION
        return left * right


def _ddiv(left: Decimal, right: Decimal) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = AUDIT_PRECISION
        return left / right


def _dec(value: float | int) -> Decimal:
    if isinstance(value, int):
        return Decimal(value)
    return Decimal(str(value))


@dataclass
class MetricState:
    name: str
    op: str
    field: str
    count: int = 0
    total: float = 0.0
    correction: float = 0.0
    abs_total: float = 0.0
    mean: float = 0.0
    m2: float = 0.0
    extreme: float | None = None
    audit_sum: Decimal = Decimal(0)
    audit_abs_total: Decimal = Decimal(0)
    audit_sumsq: Decimal = Decimal(0)
    audit_extreme: Decimal | None = None
    conversion_error: Decimal = Decimal(0)
    max_conversion_error: Decimal = Decimal(0)
    merge_error: Decimal = Decimal(0)
    merge_events: int = 0
    source_types: dict[str, int] = field(default_factory=dict)
    decimal_scales: set[int] = field(default_factory=set)
    decimal_precisions: set[int] = field(default_factory=set)
    max_precision_digits: int = 0
    downgrade_count: int = 0
    precision_paths: set[str] = field(default_factory=set)

    def update_many(self, values: Iterable[ParsedNumber]) -> None:
        for number in sorted(values, key=canonical_sort_key):
            self.update(number)

    def update(self, number: ParsedNumber) -> None:
        value = number.float_value
        self.count += 1
        self.source_types[number.source_type] = (
            self.source_types.get(number.source_type, 0) + 1
        )
        if number.source_type == "decimal" and number.scale is not None:
            self.decimal_scales.add(number.scale)
            self.decimal_precisions.add(number.precision_digits)
        self.max_precision_digits = max(
            self.max_precision_digits, number.precision_digits
        )
        if number.downgraded:
            self.downgrade_count += 1
        self.precision_paths.update(number.precision_path)

        self.conversion_error = _dadd(
            self.conversion_error, number.conversion_error
        )
        self.max_conversion_error = max(
            self.max_conversion_error, number.conversion_error
        )
        self.audit_sum = _dadd(self.audit_sum, number.decimal_exact)
        self.audit_abs_total = _dadd(
            self.audit_abs_total, abs(number.decimal_exact)
        )

        if self.op in {"sum", "avg"}:
            self._compensated_add(value)
            self.abs_total += abs(value)
        elif self.op == "stdDev":
            self._welford_update(value)
            self.audit_sumsq = _dadd(
                self.audit_sumsq,
                _dmul(number.decimal_exact, number.decimal_exact),
            )
        elif self.op in {"min", "max"}:
            if self.extreme is None:
                self.extreme = value
                self.audit_extreme = number.decimal_exact
            elif self.op == "min":
                if value < self.extreme:
                    self.extreme = value
                if number.decimal_exact < self.audit_extreme:
                    self.audit_extreme = number.decimal_exact
            else:
                if value > self.extreme:
                    self.extreme = value
                if number.decimal_exact > self.audit_extreme:
                    self.audit_extreme = number.decimal_exact
        else:
            raise ValueError(f"不支持的聚合算子: {self.op}")

    def _compensated_add(self, value: float) -> None:
        new_total = self.total + value
        if abs(self.total) >= abs(value):
            self.correction += (self.total - new_total) + value
        else:
            self.correction += (value - new_total) + self.total
        self.total = new_total

    def _welford_update(self, value: float) -> None:
        new_count = self.count
        delta = value - self.mean
        self.mean += delta / new_count
        delta2 = value - self.mean
        self.m2 += delta * delta2

    def merge(self, other: "MetricState") -> None:
        if (self.name, self.op, self.field) != (
            other.name,
            other.op,
            other.field,
        ):
            raise ValueError("不能合并不同聚合定义的中间状态")
        if other.count == 0:
            return
        if self.count == 0:
            self._copy_from(other)
            return

        old_count = self.count
        new_count = self.count + other.count

        if self.op in {"sum", "avg"}:
            left_result = self.total + self.correction
            right_result = other.total + other.correction
            self._compensated_add(other.total)
            self._compensated_add(other.correction)
            self.abs_total += other.abs_total
            merge_bound = _dec(
                2.0
                * U
                * (abs(left_result) + abs(right_result))
                + 8.0 * U * U * (self.abs_total + other.abs_total)
                + 4.0 * TINY
            )
            self.merge_error = _dadd(self.merge_error, merge_bound)
        elif self.op == "stdDev":
            delta = other.mean - self.mean
            right_weight = other.count / new_count
            self.mean = self.mean + delta * right_weight
            self.m2 = (
                self.m2
                + other.m2
                + delta * delta * old_count * other.count / new_count
            )
            merge_bound = _dec(
                (abs(self.mean) + abs(other.mean) + abs(delta))
                * 8.0
                * U
                * (old_count + other.count)
            )
            self.merge_error = _dadd(self.merge_error, merge_bound)
        else:
            if self.op == "min":
                self.extreme = min(
                    value for value in (self.extreme, other.extreme) if value is not None
                )
                self.audit_extreme = min(
                    value
                    for value in (self.audit_extreme, other.audit_extreme)
                    if value is not None
                )
            else:
                self.extreme = max(
                    value for value in (self.extreme, other.extreme) if value is not None
                )
                self.audit_extreme = max(
                    value
                    for value in (self.audit_extreme, other.audit_extreme)
                    if value is not None
                )

        self.count = new_count
        self.audit_sum = _dadd(self.audit_sum, other.audit_sum)
        self.audit_abs_total = _dadd(
            self.audit_abs_total, other.audit_abs_total
        )
        self.audit_sumsq = _dadd(self.audit_sumsq, other.audit_sumsq)
        self.conversion_error = _dadd(
            self.conversion_error, other.conversion_error
        )
        self.max_conversion_error = max(
            self.max_conversion_error, other.max_conversion_error
        )
        self.merge_error = _dadd(self.merge_error, other.merge_error)
        self.merge_events += other.merge_events + 1
        self.max_precision_digits = max(
            self.max_precision_digits, other.max_precision_digits
        )
        self.downgrade_count += other.downgrade_count
        self.decimal_scales.update(other.decimal_scales)
        self.decimal_precisions.update(other.decimal_precisions)
        self.precision_paths.update(other.precision_paths)
        for source_type, count in other.source_types.items():
            self.source_types[source_type] = (
                self.source_types.get(source_type, 0) + count
            )

    def _copy_from(self, other: "MetricState") -> None:
        self.count = other.count
        self.total = other.total
        self.correction = other.correction
        self.abs_total = other.abs_total
        self.mean = other.mean
        self.m2 = other.m2
        self.extreme = other.extreme
        self.audit_sum = other.audit_sum
        self.audit_abs_total = other.audit_abs_total
        self.audit_sumsq = other.audit_sumsq
        self.audit_extreme = other.audit_extreme
        self.conversion_error = other.conversion_error
        self.max_conversion_error = other.max_conversion_error
        self.merge_error = other.merge_error
        self.merge_events = other.merge_events
        self.source_types = dict(other.source_types)
        self.decimal_scales = set(other.decimal_scales)
        self.decimal_precisions = set(other.decimal_precisions)
        self.max_precision_digits = other.max_precision_digits
        self.downgrade_count = other.downgrade_count
        self.precision_paths = set(other.precision_paths)

    def _audit_value(self) -> Decimal | None:
        if self.count == 0:
            return None
        if self.op == "sum":
            return self.audit_sum
        if self.op == "avg":
            return _ddiv(self.audit_sum, Decimal(self.count))
        if self.op in {"min", "max"}:
            return self.audit_extreme
        if self.count < 2:
            return None
        with localcontext() as ctx:
            ctx.prec = AUDIT_PRECISION
            variance = (
                self.audit_sumsq
                - _ddiv(_dmul(self.audit_sum, self.audit_sum), Decimal(self.count))
            ) / Decimal(self.count - 1)
            if variance < 0:
                variance = Decimal(0)
            return variance.sqrt()
        return None

    def _execution_value(self) -> float | None:
        if self.count == 0:
            return None
        if self.op == "sum":
            return self.total + self.correction
        if self.op == "avg":
            return (self.total + self.correction) / self.count
        if self.op == "stdDev":
            if self.count < 2:
                return None
            variance = max(0.0, self.m2 / (self.count - 1))
            return math.sqrt(variance)
        return self.extreme

    def _model_bound(self, execution_value: float, audit_value: Decimal) -> Decimal:
        n = Decimal(self.count)
        scale = max(Decimal(1), abs(audit_value), abs(self.audit_abs_total))
        baseline = scale * Decimal("5e-16")
        if self.op in {"sum", "avg"}:
            abs_total = max(
                Decimal.from_float(self.abs_total),
                self.audit_abs_total,
            )
            accumulation = (
                Decimal.from_float(2.0 * U) * abs_total
                + Decimal(8 * max(1, self.count))
                * Decimal.from_float(U * U)
                * abs_total
            )
            bound = self.conversion_error + accumulation + self.merge_error
            if self.op == "avg":
                bound = bound / n + abs(audit_value) * Decimal.from_float(U)
            return bound + baseline
        if self.op in {"min", "max"}:
            return self.max_conversion_error + self.merge_error + baseline

        mean_scale = abs(Decimal.from_float(self.mean)) + abs(audit_value)
        model = (
            mean_scale
            * Decimal.from_float(8.0 * U)
            * Decimal(max(2, self.count + 2))
            + self.conversion_error / n
            + self.merge_error
            + baseline
        )
        _ = execution_value
        return model

    def _audit_uncertainty(self) -> Decimal:
        scale = max(Decimal(1), abs(self.audit_abs_total), abs(self.audit_sumsq))
        return scale * Decimal("1e-88")

    def _serialize_float(self, value: float | None) -> float | str | None:
        if value is None:
            return None
        if not math.isfinite(value):
            return str(value)
        return value

    def result(self) -> dict[str, Any] | None:
        execution_value = self._execution_value()
        audit_value = self._audit_value()
        if (
            execution_value is None
            or audit_value is None
            or not math.isfinite(execution_value)
            or not math.isfinite(float(audit_value))
        ):
            return None

        execution_decimal = Decimal.from_float(execution_value)
        audit_gap = abs(_dsub(execution_decimal, audit_value))
        model_bound = self._model_bound(execution_value, audit_value)
        estimate = max(
            model_bound,
            _dmul(audit_gap, Decimal("1.01")) + self._audit_uncertainty(),
        )
        estimate = _dadd(
            _dmul(estimate, Decimal("1.01")),
            Decimal(4) * Decimal.from_float(TINY),
        )
        estimate_float = float(estimate)
        if not math.isfinite(estimate_float):
            return None

        path = ["source-types:" + "+".join(sorted(self.source_types))]
        if self.downgrade_count:
            path.append("binary64:inexact-downgrade")
        if self.op in {"sum", "avg"}:
            path.extend(["abs-sorted", "neumaier-compensated-sum"])
        elif self.op == "stdDev":
            path.extend(["abs-sorted", "welford-m2"])
        else:
            path.extend(["binary64-compare", "high-precision-audit-compare"])
        if self.merge_events:
            path.append("spill-merge:error-bound-transfer")
        path.append("high-precision-audit:decimal-100")
        path.append("declare-error-bound")

        return {
            "value": execution_value,
            "error_estimate": estimate_float,
            "error_components": {
                "conversion": float(self.conversion_error),
                "accumulation_and_merge": float(
                    max(
                        Decimal(0),
                        model_bound - self.conversion_error,
                    )
                ),
                "merge_bound": float(self.merge_error),
                "audit_gap": float(audit_gap),
                "safety_padding": float(estimate - model_bound),
            },
            "error_path": path,
            "precision": {
                "audit_precision_digits": AUDIT_PRECISION,
                "source_types": dict(sorted(self.source_types.items())),
                "decimal_scales": sorted(self.decimal_scales),
                "decimal_precision_digits": sorted(self.decimal_precisions),
                "scale_preserved": bool(self.decimal_scales),
                "max_precision_digits": self.max_precision_digits,
                "type_downgraded": self.downgrade_count > 0,
                "downgrade_count": self.downgrade_count,
                "source_precision_paths": sorted(self.precision_paths),
            },
            "merge_events": self.merge_events,
            "count": self.count,
            "_audit_decimal": audit_value,
        }

    def audit_text(self, value: Decimal) -> str:
        if self.op == "stdDev":
            return format(value, ".50f")
        return format(value, "f")

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "op": self.op,
            "field": self.field,
            "count": self.count,
            "total": self._serialize_float(self.total),
            "correction": self._serialize_float(self.correction),
            "abs_total": self._serialize_float(self.abs_total),
            "mean": self._serialize_float(self.mean),
            "m2": self._serialize_float(self.m2),
            "extreme": self._serialize_float(self.extreme),
            "audit_sum": str(self.audit_sum),
            "audit_abs_total": str(self.audit_abs_total),
            "audit_sumsq": str(self.audit_sumsq),
            "audit_extreme": (
                None if self.audit_extreme is None else str(self.audit_extreme)
            ),
            "conversion_error": str(self.conversion_error),
            "max_conversion_error": str(self.max_conversion_error),
            "merge_error": str(self.merge_error),
            "merge_events": self.merge_events,
            "source_types": dict(sorted(self.source_types.items())),
            "decimal_scales": sorted(self.decimal_scales),
            "decimal_precision_digits": sorted(self.decimal_precisions),
            "max_precision_digits": self.max_precision_digits,
            "downgrade_count": self.downgrade_count,
            "precision_paths": sorted(self.precision_paths),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "MetricState":
        try:
            return cls(
                name=str(payload["name"]),
                op=str(payload["op"]),
                field=str(payload["field"]),
                count=int(payload["count"]),
                total=float(payload["total"]),
                correction=float(payload["correction"]),
                abs_total=float(payload["abs_total"]),
                mean=float(payload["mean"]),
                m2=float(payload["m2"]),
                extreme=(
                    None
                    if payload.get("extreme") is None
                    else float(payload["extreme"])
                ),
                audit_sum=Decimal(payload["audit_sum"]),
                audit_abs_total=Decimal(payload["audit_abs_total"]),
                audit_sumsq=Decimal(payload["audit_sumsq"]),
                audit_extreme=(
                    None
                    if payload.get("audit_extreme") is None
                    else Decimal(payload["audit_extreme"])
                ),
                conversion_error=Decimal(payload["conversion_error"]),
                max_conversion_error=Decimal(payload["max_conversion_error"]),
                merge_error=Decimal(payload["merge_error"]),
                merge_events=int(payload.get("merge_events", 0)),
                source_types={
                    str(key): int(value)
                    for key, value in payload.get("source_types", {}).items()
                },
                decimal_scales={
                    int(value) for value in payload.get("decimal_scales", [])
                },
                decimal_precisions={
                    int(value)
                    for value in payload.get("decimal_precision_digits", [])
                },
                max_precision_digits=int(payload.get("max_precision_digits", 0)),
                downgrade_count=int(payload.get("downgrade_count", 0)),
                precision_paths=set(payload.get("precision_paths", [])),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError(f"部分聚合状态损坏: {exc}") from exc
