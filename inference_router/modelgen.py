"""Deterministic ONNX model generation without an extra ONNX package dependency."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np


def _varint(value: int) -> bytes:
    if value < 0:
        value &= (1 << 64) - 1
    out = bytearray()
    while value > 0x7F:
        out.append((value & 0x7F) | 0x80)
        value >>= 7
    out.append(value)
    return bytes(out)


def _tag(field: int, wire_type: int) -> bytes:
    return _varint((field << 3) | wire_type)


def _vint(field: int, value: int) -> bytes:
    return _tag(field, 0) + _varint(value)


def _bytes(field: int, value: bytes) -> bytes:
    return _tag(field, 2) + _varint(len(value)) + value


def _string(field: int, value: str) -> bytes:
    return _bytes(field, value.encode("utf-8"))


def _dim(value: int) -> bytes:
    return _vint(1, value)


def _shape(dims: list[int]) -> bytes:
    shape = b"".join(_bytes(1, _dim(dim)) for dim in dims)
    return _bytes(2, shape)


def _value_info(name: str, shape: list[int]) -> bytes:
    tensor = _vint(1, 1) + _shape(shape)
    value_type = _bytes(1, tensor)
    return _string(1, name) + _bytes(2, value_type)


def _tensor(name: str, dims: list[int], values: np.ndarray) -> bytes:
    payload = values.astype("<f4", copy=False).tobytes(order="C")
    out = bytearray()
    for dim in dims:
        out += _vint(1, dim)
    out += _vint(2, 1)
    out += _string(8, name)
    out += _bytes(9, payload)
    return bytes(out)


def build_matmul_model(seed: int, input_dim: int = 64, output_dim: int = 64) -> bytes:
    rng = np.random.default_rng(seed)
    scale = float(np.sqrt(2.0 / max(1, input_dim)))
    weight = rng.normal(0.0, scale, size=(input_dim, output_dim)).astype(np.float32)

    node = (
        _string(1, "features")
        + _string(1, "weight")
        + _string(2, "output")
        + _string(3, "matmul")
        + _string(4, "MatMul")
    )
    graph = (
        _bytes(1, node)
        + _string(2, "two_tier_linear")
        + _bytes(5, _tensor("weight", [input_dim, output_dim], weight))
        + _bytes(11, _value_info("features", [1, input_dim]))
        + _bytes(12, _value_info("output", [1, output_dim]))
    )
    opset = _string(1, "") + _vint(2, 13)
    model = (
        _vint(1, 8)
        + _string(2, "inference-router-modelgen")
        + _string(3, "1.0")
        + _bytes(7, graph)
        + _bytes(8, opset)
    )
    return model


def generate_models(out_dir: Path, seed: int = 20261119) -> dict:
    out_dir.mkdir(parents=True, exist_ok=True)
    specs = [
        ("l1-a", "l1", 0),
        ("l1-b", "l1", 1),
        ("l1-c", "l1", 2),
        ("l2-a", "l2", 10),
        ("l2-b", "l2", 11),
    ]
    manifest = {"seed": seed, "models": []}
    for replica_id, layer, index in specs:
        model = build_matmul_model(seed + index)
        path = out_dir / f"{replica_id}.onnx"
        path.write_bytes(model)
        manifest["models"].append(
            {
                "replica_id": replica_id,
                "layer": layer,
                "path": str(path),
                "input_dim": 64,
                "output_dim": 64,
                "sha256": hashlib.sha256(model).hexdigest(),
            }
        )
    manifest_path = out_dir / "models.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest
