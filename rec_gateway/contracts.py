from __future__ import annotations

import json
import math
import numbers
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from rec_gateway.errors import BusinessError, ModelLoadError


SUPPORTED_DTYPES = {"float32"}
SUPPORTED_RESPONSE_TYPES = {"bool", "float", "string"}


@dataclass(frozen=True)
class FeatureColumn:
    index: int
    name: str
    dtype: str


@dataclass(frozen=True)
class TensorSpec:
    name: str
    shape: tuple[int | str, ...]
    dtype: str


@dataclass(frozen=True)
class SloSpec:
    latency_p99_ms: float
    error_rate: float
    window_size: int
    min_samples: int
    request_timeout_ms: float


@dataclass(frozen=True)
class Manifest:
    schema_version: int
    protocol_version: str
    model_file: str
    model_sha256: str | None
    feature_columns: tuple[FeatureColumn, ...]
    feature_dimension: int
    input_spec: TensorSpec
    output_specs: tuple[TensorSpec, ...]
    response_kind: str
    score_field: str
    score_type: str | None
    object_fields: tuple[tuple[str, str], ...]
    calibration_version: str | None
    required_fields: tuple[str, ...]
    slo: SloSpec


def _manifest_error(code: str, message: str, details: dict[str, Any] | None = None) -> None:
    raise ModelLoadError(code, message, details)


def _require_mapping(value: Any, name: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        _manifest_error("MANIFEST_INVALID", f"{name} must be an object")
    return value


def _require_string(value: Any, name: str) -> str:
    if not isinstance(value, str) or not value:
        _manifest_error("MANIFEST_INVALID", f"{name} must be a non-empty string")
    return value


def _require_number(value: Any, name: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, numbers.Real):
        _manifest_error("MANIFEST_INVALID", f"{name} must be numeric")
    number = float(value)
    if not math.isfinite(number):
        _manifest_error("MANIFEST_INVALID", f"{name} must be finite")
    if minimum is not None and number < minimum:
        _manifest_error("MANIFEST_INVALID", f"{name} is below its minimum")
    return number


def _parse_tensor_spec(value: Any, name: str) -> TensorSpec:
    raw = _require_mapping(value, name)
    tensor_name = _require_string(raw.get("name"), f"{name}.name")
    dtype = _require_string(raw.get("dtype"), f"{name}.dtype")
    if dtype not in SUPPORTED_DTYPES:
        _manifest_error(
            "MANIFEST_INVALID",
            f"{name}.dtype is unsupported",
            {"dtype": dtype},
        )
    shape_raw = raw.get("shape")
    if not isinstance(shape_raw, list) or not shape_raw:
        _manifest_error("MANIFEST_INVALID", f"{name}.shape must be a non-empty array")
    shape: list[int | str] = []
    for index, dimension in enumerate(shape_raw):
        if isinstance(dimension, str):
            if not dimension:
                _manifest_error("MANIFEST_INVALID", f"{name}.shape[{index}] is empty")
            shape.append(dimension)
        elif isinstance(dimension, int) and not isinstance(dimension, bool):
            if dimension < 0:
                _manifest_error(
                    "MANIFEST_INVALID", f"{name}.shape[{index}] is negative"
                )
            shape.append(dimension)
        else:
            _manifest_error(
                "MANIFEST_INVALID", f"{name}.shape[{index}] must be int or string"
            )
    return TensorSpec(tensor_name, tuple(shape), dtype)


def load_manifest(version_dir: Path, expected_protocol: str | None = None) -> Manifest:
    manifest_path = version_dir / "manifest.json"
    if not manifest_path.is_file():
        _manifest_error("MANIFEST_MISSING", "manifest.json is missing")
    try:
        raw = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        _manifest_error("MANIFEST_INVALID", "manifest.json cannot be parsed")
    if not isinstance(raw, dict):
        _manifest_error("MANIFEST_INVALID", "manifest root must be an object")
    schema_version = raw.get("schema_version")
    if schema_version != 1:
        _manifest_error(
            "MANIFEST_INVALID",
            "unsupported manifest schema_version",
            {"schema_version": schema_version},
        )
    protocol_version = _require_string(raw.get("protocol_version"), "protocol_version")
    if expected_protocol is not None and protocol_version != expected_protocol:
        _manifest_error(
            "MANIFEST_PROTOCOL_MISMATCH",
            "manifest protocol does not match requested version",
            {
                "expected_protocol_version": expected_protocol,
                "declared_protocol_version": protocol_version,
            },
        )
    model_file = _require_string(raw.get("model_file"), "model_file")
    if Path(model_file).is_absolute() or ".." in Path(model_file).parts:
        _manifest_error("MANIFEST_INVALID", "model_file must be a local relative path")

    feature_protocol = _require_mapping(raw.get("feature_protocol"), "feature_protocol")
    dimension = feature_protocol.get("dimension")
    if not isinstance(dimension, int) or isinstance(dimension, bool) or dimension < 1:
        _manifest_error("MANIFEST_INVALID", "feature_protocol.dimension is invalid")
    columns_raw = feature_protocol.get("columns")
    if not isinstance(columns_raw, list):
        _manifest_error("MANIFEST_INVALID", "feature_protocol.columns must be an array")
    if len(columns_raw) != dimension:
        _manifest_error(
            "MANIFEST_DIMENSION_MISMATCH",
            "feature column count does not match declared dimension",
            {"declared_dimension": dimension, "actual_columns": len(columns_raw)},
        )
    columns: list[FeatureColumn] = []
    seen_names: set[str] = set()
    for position, item in enumerate(columns_raw):
        column = _require_mapping(item, f"feature_protocol.columns[{position}]")
        index = column.get("index")
        if index != position:
            _manifest_error(
                "MANIFEST_COLUMN_INDEX_INVALID",
                "feature columns must have contiguous zero-based indexes",
                {"position": position, "index": index},
            )
        name = _require_string(column.get("name"), f"feature_protocol.columns[{position}].name")
        dtype = _require_string(
            column.get("type"), f"feature_protocol.columns[{position}].type"
        )
        if dtype not in SUPPORTED_DTYPES:
            _manifest_error(
                "MANIFEST_INVALID",
                "feature column type is unsupported",
                {"column_index": position, "type": dtype},
            )
        if name in seen_names:
            _manifest_error(
                "MANIFEST_INVALID",
                "feature column names must be unique",
                {"column_index": position, "column": name},
            )
        seen_names.add(name)
        columns.append(FeatureColumn(index, name, dtype))

    input_spec = _parse_tensor_spec(raw.get("input"), "input")
    if len(input_spec.shape) != 2 or input_spec.shape[1] != dimension:
        _manifest_error(
            "MANIFEST_INPUT_DIMENSION_MISMATCH",
            "manifest input shape does not match feature dimension",
            {
                "expected_dimension": dimension,
                "declared_input_shape": list(input_spec.shape),
            },
        )

    outputs_raw = raw.get("outputs")
    if not isinstance(outputs_raw, list) or not outputs_raw:
        _manifest_error("MANIFEST_INVALID", "outputs must be a non-empty array")
    output_specs = tuple(
        _parse_tensor_spec(value, f"outputs[{index}]")
        for index, value in enumerate(outputs_raw)
    )
    if len({spec.name for spec in output_specs}) != len(output_specs):
        _manifest_error("MANIFEST_INVALID", "output names must be unique")
    for output_spec in output_specs:
        if len(output_spec.shape) != 2 or output_spec.shape[1] != 1:
            _manifest_error(
                "MANIFEST_OUTPUT_SHAPE_INVALID",
                "declared outputs must have shape [batch, 1]",
                {"output": output_spec.name, "shape": list(output_spec.shape)},
            )

    response = _require_mapping(raw.get("response"), "response")
    response_kind = _require_string(response.get("kind"), "response.kind")
    score_field = _require_string(response.get("score_field"), "response.score_field")
    required_fields_raw = response.get("required_fields")
    if not isinstance(required_fields_raw, list) or not all(
        isinstance(value, str) and value for value in required_fields_raw
    ):
        _manifest_error(
            "MANIFEST_RESPONSE_INVALID", "response.required_fields must be string array"
        )
    required_fields = tuple(required_fields_raw)
    score_type: str | None = None
    object_fields: tuple[tuple[str, str], ...] = ()
    calibration_version: str | None = None
    if response_kind == "scalar_score":
        score_type = _require_string(response.get("score_type"), "response.score_type")
        if score_type != "float":
            _manifest_error(
                "MANIFEST_RESPONSE_INVALID", "scalar_score score_type must be float"
            )
        if len(output_specs) != 1:
            _manifest_error(
                "MANIFEST_RESPONSE_MISMATCH",
                "scalar_score response requires exactly one model output",
                {"model_outputs": len(output_specs)},
            )
    elif response_kind == "calibrated_object":
        object_fields_raw = _require_mapping(
            response.get("object_fields"), "response.object_fields"
        )
        parsed_fields: list[tuple[str, str]] = []
        for field_name, field_type in object_fields_raw.items():
            if not isinstance(field_name, str) or not field_name:
                _manifest_error(
                    "MANIFEST_RESPONSE_INVALID", "object field names must be non-empty"
                )
            if field_type not in SUPPORTED_RESPONSE_TYPES:
                _manifest_error(
                    "MANIFEST_RESPONSE_INVALID",
                    "object field type is unsupported",
                    {"field": field_name, "type": field_type},
                )
            parsed_fields.append((field_name, field_type))
        object_fields = tuple(sorted(parsed_fields))
        calibration_version = _require_string(
            response.get("calibration_version"), "response.calibration_version"
        )
        output_names = {spec.name for spec in output_specs}
        if not {"score", "calibration_offset"}.issubset(output_names):
            _manifest_error(
                "MANIFEST_RESPONSE_MISMATCH",
                "calibrated_object requires score and calibration_offset outputs",
                {"model_outputs": sorted(output_names)},
            )
    else:
        _manifest_error(
            "MANIFEST_RESPONSE_INVALID",
            "response.kind is unsupported",
            {"kind": response_kind},
        )

    slo_raw = _require_mapping(raw.get("slo"), "slo")
    slo = SloSpec(
        latency_p99_ms=_require_number(
            slo_raw.get("latency_p99_ms"), "slo.latency_p99_ms", 0.0
        ),
        error_rate=_require_number(slo_raw.get("error_rate"), "slo.error_rate", 0.0),
        window_size=int(_require_number(slo_raw.get("window_size"), "slo.window_size", 1)),
        min_samples=int(
            _require_number(slo_raw.get("min_samples"), "slo.min_samples", 1)
        ),
        request_timeout_ms=_require_number(
            slo_raw.get("request_timeout_ms"), "slo.request_timeout_ms", 0.0
        ),
    )
    if slo.error_rate > 1.0:
        _manifest_error("MANIFEST_INVALID", "slo.error_rate must be within [0, 1]")
    if slo.min_samples > slo.window_size:
        _manifest_error(
            "MANIFEST_INVALID", "slo.min_samples cannot exceed slo.window_size"
        )

    sha256 = raw.get("model_sha256")
    if sha256 is not None:
        sha256 = _require_string(sha256, "model_sha256").lower()
        if len(sha256) != 64 or any(character not in "0123456789abcdef" for character in sha256):
            _manifest_error("MANIFEST_INVALID", "model_sha256 must be 64 hex characters")

    return Manifest(
        schema_version=1,
        protocol_version=protocol_version,
        model_file=model_file,
        model_sha256=sha256,
        feature_columns=tuple(columns),
        feature_dimension=dimension,
        input_spec=input_spec,
        output_specs=output_specs,
        response_kind=response_kind,
        score_field=score_field,
        score_type=score_type,
        object_fields=object_fields,
        calibration_version=calibration_version,
        required_fields=required_fields,
        slo=slo,
    )


def _actual_json_type(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, numbers.Integral):
        return "integer"
    if isinstance(value, numbers.Real):
        return "float"
    if isinstance(value, str):
        return "string"
    if isinstance(value, dict):
        return "object"
    if isinstance(value, list):
        return "array"
    return type(value).__name__


def _validate_float(value: Any, column_index: int, name: str) -> None:
    if isinstance(value, bool) or not isinstance(value, numbers.Real):
        raise BusinessError(
            "FEATURE_TYPE_MISMATCH",
            "feature value must be a JSON number",
            {
                "column_index": column_index,
                "column": name,
                "expected_type": "float32",
                "actual_type": _actual_json_type(value),
            },
        )
    if not math.isfinite(float(value)):
        raise BusinessError(
            "FEATURE_TYPE_MISMATCH",
            "feature value must be finite",
            {
                "column_index": column_index,
                "column": name,
                "expected_type": "float32",
                "actual_type": "non_finite_number",
            },
        )


def prepare_feature_vector(
    manifest: Manifest,
    features: Any,
    feature_names: list[str] | None,
) -> list[float]:
    expected_names = [column.name for column in manifest.feature_columns]
    expected_dimension = manifest.feature_dimension

    if isinstance(features, dict):
        unexpected = [name for name in features if name not in expected_names]
        if unexpected:
            first_unexpected = unexpected[0]
            raise BusinessError(
                "FEATURE_COLUMN_UNEXPECTED",
                "feature object contains an unexpected column",
                {
                    "column_index": expected_dimension,
                    "column": first_unexpected,
                    "expected_count": expected_dimension,
                    "actual_count": len(features),
                },
            )
        values: list[float] = []
        for column in manifest.feature_columns:
            if column.name not in features:
                raise BusinessError(
                    "FEATURE_COLUMN_MISSING",
                    "feature object is missing a required column",
                    {
                        "column_index": column.index,
                        "column": column.name,
                        "expected_count": expected_dimension,
                        "actual_count": len(features),
                    },
                )
            value = features[column.name]
            _validate_float(value, column.index, column.name)
            values.append(float(value))
        return values

    if not isinstance(features, list):
        raise BusinessError(
            "FEATURE_TYPE_MISMATCH",
            "features must be an array or an object keyed by feature name",
            {
                "column_index": None,
                "expected_type": "array|object",
                "actual_type": _actual_json_type(features),
            },
        )
    if len(features) != expected_dimension:
        raise BusinessError(
            "FEATURE_COUNT_MISMATCH",
            "feature count does not match the target protocol",
            {
                "column_index": min(len(features), expected_dimension),
                "expected_count": expected_dimension,
                "actual_count": len(features),
            },
        )
    if feature_names is not None:
        if not all(isinstance(name, str) and name for name in feature_names):
            raise BusinessError(
                "INVALID_REQUEST",
                "feature_names must contain non-empty strings",
                {"column_index": None},
            )
        if len(feature_names) != expected_dimension:
            raise BusinessError(
                "FEATURE_COLUMN_COUNT_MISMATCH",
                "feature_names count does not match the target protocol",
                {
                    "column_index": min(len(feature_names), expected_dimension),
                    "expected_count": expected_dimension,
                    "actual_count": len(feature_names),
                },
            )
        for column in manifest.feature_columns:
            actual_name = feature_names[column.index]
            if actual_name != column.name:
                raise BusinessError(
                    "FEATURE_COLUMN_ORDER_MISMATCH",
                    "feature column order does not match the target protocol",
                    {
                        "column_index": column.index,
                        "expected_column": column.name,
                        "actual_column": actual_name,
                    },
                )

    values = []
    for column, value in zip(manifest.feature_columns, features):
        _validate_float(value, column.index, column.name)
        values.append(float(value))
    return values


def validate_protocol(declared_protocol: str, manifest: Manifest) -> None:
    if declared_protocol != manifest.protocol_version:
        raise BusinessError(
            "FEATURE_PROTOCOL_MISMATCH",
            "declared feature protocol does not match the selected model",
            {
                "column_index": None,
                "declared_protocol_version": declared_protocol,
                "expected_protocol_version": manifest.protocol_version,
            },
        )


def _validate_response_scalar(value: Any, expected_type: str) -> bool:
    if expected_type == "float":
        return not isinstance(value, bool) and isinstance(value, numbers.Real) and math.isfinite(float(value))
    if expected_type == "bool":
        return isinstance(value, bool)
    if expected_type == "string":
        return isinstance(value, str)
    return False


def validate_success_payload(manifest: Manifest, payload: dict[str, Any]) -> None:
    for field_name in manifest.required_fields:
        if field_name not in payload:
            raise ValueError(f"response field missing: {field_name}")
    score = payload[manifest.score_field]
    if manifest.response_kind == "scalar_score":
        if not _validate_response_scalar(score, "float"):
            raise ValueError("v3 score must be a finite number")
        return
    if not isinstance(score, dict):
        raise ValueError("v4 score must be an object")
    if set(score) != {name for name, _ in manifest.object_fields}:
        raise ValueError("v4 score object fields do not match the manifest")
    for field_name, expected_type in manifest.object_fields:
        if not _validate_response_scalar(score[field_name], expected_type):
            raise ValueError(f"v4 score field has invalid type: {field_name}")


def build_success_payload(
    manifest: Manifest,
    served_version: str,
    request_id: str,
    output_values: list[Any],
    batch_size: int,
) -> dict[str, Any]:
    output_by_name = {
        spec.name: output_values[index]
        for index, spec in enumerate(manifest.output_specs)
    }
    payload: dict[str, Any] = {
        "ok": True,
        "protocol_version": manifest.protocol_version,
        "request_id": request_id,
        "served_version": served_version,
    }
    score = float(output_by_name["score"].reshape(-1)[0])
    if manifest.response_kind == "scalar_score":
        payload[manifest.score_field] = score
    else:
        calibration_offset = float(
            output_by_name["calibration_offset"].reshape(-1)[0]
        )
        payload[manifest.score_field] = {
            "calibrated": True,
            "calibration_offset": calibration_offset,
            "calibration_version": manifest.calibration_version,
            "raw_score": score,
            "value": score + calibration_offset,
        }
    validate_success_payload(manifest, payload)
    return payload
