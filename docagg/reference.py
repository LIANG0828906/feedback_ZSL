from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from decimal import Decimal, localcontext
from fractions import Fraction
from pathlib import Path
from typing import Any

from .config import PipelineConfig
from .errors import BusinessIssue, ErrorCode, NumericParseError
from .io_utils import ColumnarBatch, count_jsonl_records, sha256_file
from .numeric import AUDIT_PRECISION, ParsedNumber, decimal_from_fraction, parse_numeric


@dataclass
class ReferenceAggregate:
    name: str
    op: str
    field: str
    count: int
    method: str
    value: Decimal
    value_float: float | None
    value_fraction: Fraction | None
    source_types: dict[str, int] = field(default_factory=dict)
    decimal_scales: list[int] = field(default_factory=list)
    decimal_precision_digits: list[int] = field(default_factory=list)
    downgraded: bool = False

    def to_dict(self) -> dict[str, Any]:
        text = (
            format(self.value, ".50f")
            if self.op == "stdDev"
            else format(self.value, "f")
        )
        payload: dict[str, Any] = {
            "status": "ok",
            "name": self.name,
            "op": self.op,
            "field": self.field,
            "count": self.count,
            "method": self.method,
            "value": self.value_float,
            "value_text": text,
            "source_types": dict(sorted(self.source_types.items())),
            "decimal_scales": self.decimal_scales,
            "decimal_precision_digits": self.decimal_precision_digits,
            "type_downgraded": self.downgraded,
        }
        if self.value_fraction is not None:
            payload["value_fraction"] = (
                f"{self.value_fraction.numerator}/{self.value_fraction.denominator}"
            )
        return payload


@dataclass
class _ReferenceGroup:
    key: dict[str, Any]
    key_id: str
    values: dict[str, list[ParsedNumber]] = field(default_factory=dict)


def _key_id(key: dict[str, Any]) -> str:
    return json.dumps(
        key,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


class ReferenceEngine:
    def __init__(self, pipeline: PipelineConfig) -> None:
        self.pipeline = pipeline
        self.groups: dict[str, _ReferenceGroup] = {}
        self.issues: list[BusinessIssue] = []
        self.invalid: set[tuple[str, str]] = set()
        self.document_count = 0

    def consume_batch(self, batch: ColumnarBatch) -> None:
        valid_rows: list[tuple[int, dict[str, Any], dict[str, Any], str]] = []
        for row_index, row in batch.iter_rows():
            self.document_count += 1
            key: dict[str, Any] = {}
            missing = False
            for field_name in self.pipeline.group_by:
                if field_name not in row or row[field_name] is None:
                    missing = True
                    issue = BusinessIssue(
                        ErrorCode.FIELD_MISSING,
                        "分组键字段缺失",
                        group_key=key or None,
                        field=field_name,
                        details={"row_index": row_index},
                    )
                    self.issues.append(issue)
                    break
                if isinstance(row[field_name], (dict, list)) or (
                    isinstance(row[field_name], float)
                    and not math.isfinite(row[field_name])
                ):
                    self.issues.append(
                        BusinessIssue(
                            ErrorCode.GROUP_KEY_INVALID,
                            "分组键必须是有限标量值",
                            group_key=key or None,
                            field=field_name,
                            details={"row_index": row_index},
                        )
                    )
                    missing = True
                    break
                key[field_name] = row[field_name]
            if missing:
                continue
            key_id = _key_id(key)
            if key_id not in self.groups:
                self.groups[key_id] = _ReferenceGroup(key=key, key_id=key_id)
            valid_rows.append((row_index, row, key, key_id))

        for aggregate in self.pipeline.aggregates:
            for row_index, row, key, key_id in valid_rows:
                if (key_id, aggregate.name) in self.invalid:
                    continue
                if aggregate.field not in row or row[aggregate.field] is None:
                    self.invalid.add((key_id, aggregate.name))
                    self.issues.append(
                        BusinessIssue(
                            ErrorCode.FIELD_MISSING,
                            "数值字段缺失",
                            group_key=key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"row_index": row_index},
                        )
                    )
                    continue
                try:
                    number = parse_numeric(row[aggregate.field])
                except NumericParseError as exc:
                    self.invalid.add((key_id, aggregate.name))
                    self.issues.append(
                        BusinessIssue(
                            exc.code,
                            exc.reason,
                            group_key=key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"row_index": row_index, **exc.details},
                        )
                    )
                    continue
                group = self.groups[key_id]
                group.values.setdefault(aggregate.name, []).append(number)

        for aggregate in self.pipeline.aggregates:
            for key_id, group in self.groups.items():
                if (key_id, aggregate.name) in self.invalid:
                    continue
                scales = {
                    number.scale
                    for number in group.values.get(aggregate.name, [])
                    if number.source_type == "decimal" and number.scale is not None
                }
                if len(scales) > 1:
                    self.invalid.add((key_id, aggregate.name))
                    self.issues.append(
                        BusinessIssue(
                            ErrorCode.SCALE_MISMATCH,
                            "同一分组内定点小数的标度不一致",
                            group_key=group.key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"scales": sorted(scales)},
                        )
                    )

    def finalize(self) -> dict[str, Any]:
        groups_payload: list[dict[str, Any]] = []
        for key_id in sorted(self.groups):
            group = self.groups[key_id]
            aggregates_payload: dict[str, Any] = {}
            for aggregate in self.pipeline.aggregates:
                if (key_id, aggregate.name) in self.invalid:
                    aggregates_payload[aggregate.name] = {
                        "status": "error",
                        "error_codes": sorted(
                            {
                                issue.code.value
                                for issue in self.issues
                                if issue.group_key == group.key
                                and issue.aggregate == aggregate.name
                            }
                        ),
                    }
                    continue
                numbers = group.values.get(aggregate.name, [])
                if not numbers:
                    aggregates_payload[aggregate.name] = {
                        "status": "error",
                        "error_codes": [ErrorCode.FIELD_MISSING.value],
                    }
                    continue
                if aggregate.op == "stdDev" and len(numbers) < 2:
                    self.issues.append(
                        BusinessIssue(
                            ErrorCode.STDDEV_INSUFFICIENT_SAMPLES,
                            "stdDev 至少需要两个样本",
                            group_key=group.key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"sample_count": len(numbers)},
                        )
                    )
                    aggregates_payload[aggregate.name] = {
                        "status": "error",
                        "error_codes": [
                            ErrorCode.STDDEV_INSUFFICIENT_SAMPLES.value
                        ],
                    }
                    continue
                reference = _aggregate_reference(aggregate.name, aggregate.op, aggregate.field, numbers)
                aggregates_payload[aggregate.name] = reference.to_dict()
            groups_payload.append(
                {"key": group.key, "aggregates": aggregates_payload}
            )
        return {
            "schema_version": "1.0",
            "pipeline": self.pipeline.to_dict(),
            "groups": groups_payload,
            "errors": [issue.to_dict() for issue in self.issues],
        }


def _aggregate_reference(
    name: str,
    op: str,
    field_name: str,
    numbers: list[ParsedNumber],
) -> ReferenceAggregate:
    has_float = any(number.source_type == "float" for number in numbers)
    source_types: dict[str, int] = {}
    downgraded = False
    scales: set[int] = set()
    decimal_precisions: set[int] = set()
    for number in numbers:
        source_types[number.source_type] = source_types.get(number.source_type, 0) + 1
        downgraded = downgraded or number.downgraded
        if number.source_type == "decimal" and number.scale is not None:
            scales.add(number.scale)
            decimal_precisions.add(number.precision_digits)

    if has_float:
        value, value_fraction = _decimal_reference(op, numbers)
        method = "high_precision_decimal_simulation"
    else:
        value_fraction, value = _rational_reference(op, numbers)
        method = "exact_rational"

    try:
        value_float = float(value)
        if not math.isfinite(value_float):
            value_float = None
    except (OverflowError, ValueError):
        value_float = None
    return ReferenceAggregate(
        name=name,
        op=op,
        field=field_name,
        count=len(numbers),
        method=method,
        value=value,
        value_float=value_float,
        value_fraction=value_fraction,
        source_types=source_types,
        decimal_scales=sorted(scales),
        decimal_precision_digits=sorted(decimal_precisions),
        downgraded=downgraded,
    )


def _rational_reference(
    op: str,
    numbers: list[ParsedNumber],
) -> tuple[Fraction | None, Decimal]:
    values = [number.exact for number in numbers]
    if op == "sum":
        result = sum(values, Fraction(0))
        return result, decimal_from_fraction(result, AUDIT_PRECISION)
    if op == "avg":
        result = sum(values, Fraction(0)) / len(values)
        return result, decimal_from_fraction(result, AUDIT_PRECISION)
    if op == "min":
        result = min(values)
        return result, decimal_from_fraction(result, AUDIT_PRECISION)
    if op == "max":
        result = max(values)
        return result, decimal_from_fraction(result, AUDIT_PRECISION)
    if op == "stdDev":
        mean = sum(values, Fraction(0)) / len(values)
        variance = sum(
            ((value - mean) * (value - mean) for value in values),
            Fraction(0),
        ) / (len(values) - 1)
        with localcontext() as ctx:
            ctx.prec = AUDIT_PRECISION
            decimal_variance = decimal_from_fraction(variance, AUDIT_PRECISION + 10)
            return None, decimal_variance.sqrt()
    raise ValueError(f"不支持的算子: {op}")


def _decimal_reference(
    op: str,
    numbers: list[ParsedNumber],
) -> tuple[Decimal, Fraction | None]:
    values = [number.decimal_exact for number in numbers]
    with localcontext() as ctx:
        ctx.prec = AUDIT_PRECISION
        total = sum(values, Decimal(0))
        if op == "sum":
            return total, None
        if op == "avg":
            return total / Decimal(len(values)), None
        if op == "min":
            return min(values), None
        if op == "max":
            return max(values), None
        if op == "stdDev":
            sumsq = sum((value * value for value in values), Decimal(0))
            variance = (
                sumsq - (total * total / Decimal(len(values)))
            ) / Decimal(len(values) - 1)
            if variance < 0:
                variance = Decimal(0)
            return variance.sqrt(), None
    raise ValueError(f"不支持的算子: {op}")


def compute_reference(
    path: str | Path,
    pipeline: PipelineConfig,
    batch_size: int = 64,
) -> dict[str, Any]:
    from .io_utils import iter_jsonl_batches

    engine = ReferenceEngine(pipeline)
    for batch in iter_jsonl_batches(path, batch_size):
        engine.consume_batch(batch)
    payload = engine.finalize()
    payload["input"] = {
        "path": str(path),
        "sha256": sha256_file(path),
        "documents": count_jsonl_records(path),
    }
    return payload
