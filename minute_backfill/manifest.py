"""Declarative input schema loading and validation.

The schema manifest is the single source of truth for field names and types.
Any structural mismatch raises E_INPUT with the shard/batch subject supplied by
the caller.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pyarrow as pa

from .errors import EngineError, input_error
from .timeutil import parse_amount_minor, parse_time_us
from .util import load_json_strict


_DECIMAL_RE = re.compile(r"^decimal\((\d+),(\d+)\)$")
_SUPPORTED_SCALARS = {"string", "int64", "timestamp_us_utc"}


@dataclass(frozen=True)
class FieldSpec:
    name: str
    type_name: str
    nullable: bool = False
    allowed_values: tuple[str, ...] | None = None

    def convert(self, value: Any, *, subject: str, row: int) -> Any:
        if value is None:
            if self.nullable:
                return None
            raise input_error(
                f"{subject}: required field {self.name!r} is null",
                subject,
                row=row,
                field_name=self.name,
            )
        try:
            if self.type_name == "string":
                if not isinstance(value, str):
                    raise ValueError("expected string")
                result: Any = value
            elif self.type_name == "int64":
                if isinstance(value, bool) or not isinstance(value, int):
                    raise ValueError("expected int64")
                if not -(2**63) <= value < 2**63:
                    raise ValueError("int64 out of range")
                result = value
            elif self.type_name == "timestamp_us_utc":
                result = parse_time_us(value, field_name=self.name)
            elif _DECIMAL_RE.match(self.type_name):
                result = parse_amount_minor(value, field_name=self.name)
            else:
                raise ValueError(f"unsupported manifest type {self.type_name!r}")
        except ValueError as exc:
            raise input_error(
                f"{subject}: field {self.name!r} is invalid: {exc}",
                subject,
                row=row,
                field_name=self.name,
            ) from exc
        if self.allowed_values is not None and result not in self.allowed_values:
            raise input_error(
                f"{subject}: field {self.name!r} value {result!r} is outside "
                f"{list(self.allowed_values)!r}",
                subject,
                row=row,
                field_name=self.name,
            )
        return result

    def arrow_type(self) -> pa.DataType:
        if self.type_name == "string":
            return pa.string()
        if self.type_name == "int64":
            return pa.int64()
        if self.type_name == "timestamp_us_utc":
            return pa.timestamp("us", tz="UTC")
        match = _DECIMAL_RE.match(self.type_name)
        if match:
            precision, scale = int(match.group(1)), int(match.group(2))
            return pa.decimal128(precision, scale)
        raise ValueError(f"unsupported type {self.type_name!r}")

    def decimal_precision_scale(self) -> tuple[int, int] | None:
        match = _DECIMAL_RE.match(self.type_name)
        if not match:
            return None
        return int(match.group(1)), int(match.group(2))


@dataclass(frozen=True)
class TableSpec:
    name: str
    fields: tuple[FieldSpec, ...]
    shard_manifest: str
    format: str

    @property
    def field_names(self) -> tuple[str, ...]:
        return tuple(field.name for field in self.fields)

    def by_name(self) -> dict[str, FieldSpec]:
        return {field.name: field for field in self.fields}


@dataclass(frozen=True)
class SchemaManifest:
    path: Path
    version: int
    payments: TableSpec
    refunds: TableSpec
    raw: dict[str, Any]


def _load_table(raw: Any, table_name: str) -> TableSpec:
    if not isinstance(raw, dict):
        raise ValueError(f"manifest.{table_name} must be an object")
    fmt = raw.get("format")
    if fmt not in {"parquet", "jsonl"}:
        raise ValueError(f"manifest.{table_name}.format must be parquet or jsonl")
    fields_raw = raw.get("fields")
    if not isinstance(fields_raw, dict) or not fields_raw:
        raise ValueError(f"manifest.{table_name}.fields must be a non-empty object")
    fields: list[FieldSpec] = []
    for name, field_raw in fields_raw.items():
        if not isinstance(name, str) or not name:
            raise ValueError(f"manifest.{table_name} contains an invalid field name")
        if not isinstance(field_raw, dict):
            raise ValueError(f"manifest.{table_name}.{name} must be an object")
        type_name = field_raw.get("type")
        if not isinstance(type_name, str):
            raise ValueError(f"manifest.{table_name}.{name}.type must be a string")
        if type_name not in _SUPPORTED_SCALARS and not _DECIMAL_RE.match(type_name):
            raise ValueError(f"manifest.{table_name}.{name}.type is unsupported: {type_name}")
        nullable = field_raw.get("nullable", False)
        if not isinstance(nullable, bool):
            raise ValueError(f"manifest.{table_name}.{name}.nullable must be boolean")
        allowed = field_raw.get("allowed_values")
        parsed_allowed: tuple[str, ...] | None = None
        if allowed is not None:
            if not isinstance(allowed, list) or not all(isinstance(item, str) for item in allowed):
                raise ValueError(f"manifest.{table_name}.{name}.allowed_values must be strings")
            parsed_allowed = tuple(allowed)
        fields.append(FieldSpec(name, type_name, nullable, parsed_allowed))
    shard_manifest = raw.get("manifest")
    if not isinstance(shard_manifest, str) or not shard_manifest:
        raise ValueError(f"manifest.{table_name}.manifest must be a path string")
    return TableSpec(table_name, tuple(fields), shard_manifest, fmt)


def load_manifest(path: Path) -> SchemaManifest:
    try:
        raw = load_json_strict(path)
    except FileNotFoundError as exc:
        raise input_error(f"schema manifest not found: {path}", path.name) from exc
    except Exception as exc:
        raise input_error(f"schema manifest cannot be parsed: {exc}", path.name) from exc
    if not isinstance(raw, dict):
        raise input_error("schema manifest root must be an object", path.name)
    try:
        version = raw.get("manifest_version")
        if version != 1:
            raise ValueError("manifest_version must be 1")
        payments = _load_table(raw.get("payments"), "payments")
        refunds = _load_table(raw.get("refunds"), "refunds")
        if payments.format != "parquet":
            raise ValueError("manifest.payments.format must be parquet")
        if refunds.format != "jsonl":
            raise ValueError("manifest.refunds.format must be jsonl")
    except ValueError as exc:
        raise input_error(f"schema manifest is invalid: {exc}", path.name) from exc
    required_common = {"payment_id", "merchant_id", "event_time", "amount", "currency", "event_type"}
    if not required_common.issubset(set(payments.field_names)):
        missing = sorted(required_common - set(payments.field_names))
        raise input_error(
            f"payments schema is missing required fields: {missing}", path.name
        )
    if not required_common.issubset(set(refunds.field_names)):
        missing = sorted(required_common - set(refunds.field_names))
        raise input_error(
            f"refunds schema is missing required fields: {missing}", path.name
        )
    return SchemaManifest(path, version, payments, refunds, raw)


def validate_arrow_schema(
    schema: pa.Schema,
    table: TableSpec,
    *,
    subject: str,
) -> None:
    actual = {field.name: field for field in schema}
    expected = table.by_name()
    missing = sorted(set(expected) - set(actual))
    extra = sorted(set(actual) - set(expected))
    if missing or extra:
        raise input_error(
            f"{subject}: parquet schema mismatch (missing={missing}, extra={extra})",
            subject,
            details={"missing": missing, "extra": extra},
        )
    for name, spec in expected.items():
        arrow_field = actual[name]
        wanted = spec.arrow_type()
        got = arrow_field.type
        if spec.type_name == "timestamp_us_utc":
            good = (
                pa.types.is_timestamp(got)
                and got.unit == "us"
                and got.tz in {"UTC", "+00:00", None}
            )
        elif spec.decimal_precision_scale():
            precision, scale = spec.decimal_precision_scale() or (0, 0)
            good = (
                (pa.types.is_decimal128(got) or pa.types.is_decimal256(got))
                and got.precision == precision
                and got.scale == scale
            )
        else:
            good = got == wanted
        if not good:
            raise input_error(
                f"{subject}: field {name!r} type mismatch, "
                f"manifest={spec.type_name} data={got}",
                subject,
                field_name=name,
            )
        if arrow_field.nullable and not spec.nullable:
            # Arrow's nullable flag is advisory; null values are checked row by row.
            continue


def convert_row(
    row: dict[str, Any],
    table: TableSpec,
    *,
    subject: str,
    row_number: int,
) -> dict[str, Any]:
    expected = set(table.field_names)
    actual = set(row)
    if actual != expected:
        missing = sorted(expected - actual)
        extra = sorted(actual - expected)
        raise input_error(
            f"{subject}: row field set mismatch (missing={missing}, extra={extra})",
            subject,
            row=row_number,
            details={"missing": missing, "extra": extra},
        )
    result: dict[str, Any] = {}
    for field in table.fields:
        result[field.name] = field.convert(row[field.name], subject=subject, row=row_number)
    return result


def ensure_same_schema(left: SchemaManifest, right: SchemaManifest) -> None:
    if left.raw != right.raw:
        raise EngineError("E_INPUT", "schema manifest changed while job was running")
