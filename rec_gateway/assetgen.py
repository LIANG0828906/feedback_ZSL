from __future__ import annotations

import hashlib
import json
import math
import random
import shutil
import struct
from pathlib import Path
from typing import Any, Iterable


PROFILE_NAMES = [f"profile_{index:02d}" for index in range(40)]
BEHAVIOR_NAMES = [f"behavior_{index:02d}" for index in range(8)]
CONTEXT_NAMES = [f"context_{index:02d}" for index in range(8)]
V3_FEATURE_NAMES = PROFILE_NAMES + BEHAVIOR_NAMES
V4_FEATURE_NAMES = BEHAVIOR_NAMES + PROFILE_NAMES + CONTEXT_NAMES
ALL_FEATURE_NAMES = PROFILE_NAMES + BEHAVIOR_NAMES + CONTEXT_NAMES
TENANT_IDS = ("tenant-a", "tenant-b", "tenant-c", "tenant-d")


def _varint(value: int) -> bytes:
    if value < 0:
        value &= (1 << 64) - 1
    output = bytearray()
    while value > 0x7F:
        output.append((value & 0x7F) | 0x80)
        value >>= 7
    output.append(value)
    return bytes(output)


def _key(field_number: int, wire_type: int) -> bytes:
    return _varint((field_number << 3) | wire_type)


def _field_varint(field_number: int, value: int) -> bytes:
    return _key(field_number, 0) + _varint(value)


def _field_bytes(field_number: int, value: bytes) -> bytes:
    return _key(field_number, 2) + _varint(len(value)) + value


def _field_string(field_number: int, value: str) -> bytes:
    return _field_bytes(field_number, value.encode("utf-8"))


def _dimension(value: int | str) -> bytes:
    if isinstance(value, str):
        return _field_string(2, value)
    return _field_varint(1, value)


def _shape(dimensions: Iterable[int | str]) -> bytes:
    tensor_shape = b"".join(
        _field_bytes(1, _dimension(dimension)) for dimension in dimensions
    )
    return _field_bytes(2, tensor_shape)


def _type_proto(dimensions: Iterable[int | str]) -> bytes:
    tensor_type = _field_varint(1, 1) + _shape(dimensions)
    return _field_bytes(1, tensor_type)


def _value_info(name: str, dimensions: Iterable[int | str]) -> bytes:
    return _field_string(1, name) + _field_bytes(2, _type_proto(dimensions))


def _tensor(name: str, dimensions: list[int], values: list[float]) -> bytes:
    payload = b"".join(struct.pack("<f", value) for value in values)
    output = bytearray()
    for dimension in dimensions:
        output.extend(_field_varint(1, dimension))
    output.extend(_field_varint(2, 1))
    output.extend(_field_string(8, name))
    output.extend(_field_bytes(9, payload))
    return bytes(output)


def _node(inputs: list[str], outputs: list[str], name: str, op_type: str) -> bytes:
    output = bytearray()
    for value in inputs:
        output.extend(_field_string(1, value))
    for value in outputs:
        output.extend(_field_string(2, value))
    output.extend(_field_string(3, name))
    output.extend(_field_string(4, op_type))
    return bytes(output)


def _graph(
    dimension: int,
    weights: list[float],
    bias: float,
    calibration_offset: float | None,
) -> bytes:
    nodes = [
        _node(["features", "weight"], ["raw_score"], "matmul", "MatMul"),
        _node(["raw_score", "bias"], ["score"], "add_bias", "Add"),
    ]
    outputs = [_value_info("score", ["batch", 1])]
    initializers = [
        _tensor("weight", [dimension, 1], weights),
        _tensor("bias", [1], [bias]),
    ]
    if calibration_offset is not None:
        nodes.append(
            _node(
                ["raw_score", "calibration_offset_value"],
                ["calibration_offset"],
                "add_calibration",
                "Add",
            )
        )
        outputs.append(_value_info("calibration_offset", ["batch", 1]))
        initializers.append(
            _tensor("calibration_offset_value", [1], [calibration_offset])
        )
    output = bytearray()
    for node in nodes:
        output.extend(_field_bytes(1, node))
    output.extend(_field_string(2, "recommendation_scoring"))
    for initializer in initializers:
        output.extend(_field_bytes(5, initializer))
    output.extend(_field_bytes(11, _value_info("features", ["batch", dimension])))
    for value in outputs:
        output.extend(_field_bytes(12, value))
    return bytes(output)


def _model_bytes(
    dimension: int, weights: list[float], bias: float, calibration_offset: float | None
) -> bytes:
    opset = _field_string(1, "") + _field_varint(2, 13)
    output = bytearray()
    output.extend(_field_varint(1, 8))
    output.extend(_field_string(2, "rec-gateway-assetgen"))
    output.extend(_field_string(3, "1.0"))
    output.extend(_field_string(4, "local.recommendation"))
    output.extend(_field_varint(5, 1))
    output.extend(_field_bytes(7, _graph(dimension, weights, bias, calibration_offset)))
    output.extend(_field_bytes(8, opset))
    return bytes(output)


def feature_weight(name: str) -> float:
    digest = hashlib.sha256(name.encode("utf-8")).digest()
    raw = int.from_bytes(digest[:4], "big") % 2001
    return round((raw - 1000) / 2000.0, 6)


def model_weights(feature_names: list[str]) -> list[float]:
    return [feature_weight(name) for name in feature_names]


def _column_specs(names: list[str]) -> list[dict[str, Any]]:
    return [
        {"index": index, "name": name, "type": "float32"}
        for index, name in enumerate(names)
    ]


def _manifest(
    version: str,
    feature_names: list[str],
    bias: float,
    calibration_offset: float | None,
    model_sha256: str,
) -> dict[str, Any]:
    outputs = [{"name": "score", "shape": ["batch", 1], "dtype": "float32"}]
    response: dict[str, Any]
    if version == "v3":
        response = {
            "kind": "scalar_score",
            "score_field": "score",
            "score_type": "float",
            "required_fields": [
                "ok",
                "protocol_version",
                "request_id",
                "score",
                "served_version",
            ],
        }
    else:
        outputs.append(
            {
                "name": "calibration_offset",
                "shape": ["batch", 1],
                "dtype": "float32",
            }
        )
        response = {
            "kind": "calibrated_object",
            "score_field": "score",
            "calibration_version": "v4-platt-local-1",
            "object_fields": {
                "calibrated": "bool",
                "calibration_offset": "float",
                "calibration_version": "string",
                "raw_score": "float",
                "value": "float",
            },
            "required_fields": [
                "ok",
                "protocol_version",
                "request_id",
                "score",
                "served_version",
            ],
        }
    return {
        "schema_version": 1,
        "protocol_version": version,
        "model_file": "model.onnx",
        "model_sha256": model_sha256,
        "feature_protocol": {
            "dimension": len(feature_names),
            "columns": _column_specs(feature_names),
        },
        "input": {"name": "features", "shape": ["batch", len(feature_names)], "dtype": "float32"},
        "outputs": outputs,
        "response": response,
        "slo": {
            "latency_p99_ms": 250.0 if version == "v3" else 120.0,
            "error_rate": 0.1,
            "window_size": 200,
            "min_samples": 40,
            "request_timeout_ms": 600.0,
        },
        "model_defaults": {
            "bias": bias,
            "calibration_offset": calibration_offset,
        },
    }


def _sample_value(sample_index: int, feature_index: int) -> float:
    numerator = (sample_index * 17 + feature_index * 13 + 7) % 401
    return round((numerator - 200) / 200.0, 6)


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def generate_assets(
    output_dir: str | Path,
    sample_count: int = 10000,
    seed: int = 294,
    inject_v4_load_failure: bool = False,
) -> dict[str, Any]:
    root = Path(output_dir).resolve()
    models_dir = root / "models"
    samples_dir = root / "samples"
    if models_dir.exists():
        shutil.rmtree(models_dir)
    samples_dir.mkdir(parents=True, exist_ok=True)

    protocol_specs = {
        "v3": {
            "names": V3_FEATURE_NAMES,
            "bias": 0.125,
            "calibration_offset": None,
        },
        "v4": {
            "names": V4_FEATURE_NAMES,
            "bias": 0.375,
            "calibration_offset": 0.0625,
        },
    }
    manifest_hashes: dict[str, str] = {}
    for version, spec in protocol_specs.items():
        weights = model_weights(spec["names"])
        model = _model_bytes(
            len(spec["names"]),
            weights,
            spec["bias"],
            spec["calibration_offset"],
        )
        version_dir = models_dir / version
        version_dir.mkdir(parents=True)
        model_path = version_dir / "model.onnx"
        model_path.write_bytes(model)
        digest = hashlib.sha256(model).hexdigest()
        manifest_hashes[version] = digest
        manifest = _manifest(
            version,
            spec["names"],
            spec["bias"],
            spec["calibration_offset"],
            digest,
        )
        if version == "v4" and inject_v4_load_failure:
            manifest["feature_protocol"]["dimension"] = 55
            manifest["input"]["shape"] = ["batch", 55]
            manifest["outputs"][0]["shape"] = ["batch", 2]
        _write_json(version_dir / "manifest.json", manifest)

    rng = random.Random(seed)
    samples_path = samples_dir / "requests.jsonl"
    protocols = ("v3", "v4")
    with samples_path.open("w", encoding="utf-8", newline="\n") as handle:
        for index in range(sample_count):
            protocol = protocols[(index // len(TENANT_IDS)) % 2]
            names = protocol_specs[protocol]["names"]
            protocol_index = index // 2
            tenant_id = TENANT_IDS[index % len(TENANT_IDS)]
            client_number = (protocol_index // 3) % 25
            client_id: str | None = f"client-{tenant_id}-{client_number:02d}"
            if index % 29 == 0:
                client_id = None
            values = [
                _sample_value(index, ALL_FEATURE_NAMES.index(name)) for name in names
            ]
            if index % 97 == 0:
                jitter = rng.random() * 0.0001
                values[0] = round(values[0] + jitter, 8)
            sample = {
                "client_id": client_id,
                "feature_names": names,
                "features": values,
                "protocol_version": protocol,
                "request_id": f"sample-{index:06d}",
                "tenant_id": tenant_id,
            }
            handle.write(
                json.dumps(sample, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                + "\n"
            )

    summary = {
        "sample_count": sample_count,
        "sample_path": "samples/requests.jsonl",
        "model_hashes": manifest_hashes,
        "protocols": {
            "v3": {"dimension": len(V3_FEATURE_NAMES)},
            "v4": {"dimension": len(V4_FEATURE_NAMES)},
        },
        "v4_load_failure_injected": inject_v4_load_failure,
    }
    _write_json(root / "assets.json", summary)
    return summary


def expected_score(version: str, feature_values_by_name: dict[str, float]) -> float:
    names = V3_FEATURE_NAMES if version == "v3" else V4_FEATURE_NAMES
    base = sum(feature_weight(name) * feature_values_by_name[name] for name in names)
    bias = 0.125 if version == "v3" else 0.375
    return round(base + bias, 6)


def percentile(values: Iterable[float], quantile: float) -> float | None:
    ordered = sorted(values)
    if not ordered:
        return None
    index = max(0, math.ceil(quantile * len(ordered)) - 1)
    return ordered[index]
