"""Deterministic JSON/tensor serialization helpers."""

from __future__ import annotations

import base64
import hashlib
import json
from typing import Any

import numpy as np
import torch

from .errors import fail


def tensor_to_payload(tensor: torch.Tensor) -> dict[str, Any]:
    array = tensor.detach().cpu().contiguous().numpy()
    return {
        "dtype": str(array.dtype),
        "shape": list(array.shape),
        "data": base64.b64encode(array.tobytes(order="C")).decode("ascii"),
    }


def tensor_from_payload(payload: dict[str, Any], *, requires_grad: bool = False) -> torch.Tensor:
    try:
        dtype = np.dtype(payload["dtype"])
        shape = tuple(int(v) for v in payload["shape"])
        data = base64.b64decode(payload["data"].encode("ascii"))
    except (KeyError, TypeError, ValueError) as exc:
        fail("E_CHECKPOINT", f"invalid tensor payload: {exc}", "tensor")
    expected = int(np.prod(shape)) * dtype.itemsize
    if len(data) != expected:
        fail(
            "E_CHECKPOINT",
            "tensor payload byte size does not match dtype and shape",
            "tensor",
            expected=expected,
            actual=len(data),
        )
    array = np.frombuffer(data, dtype=dtype).reshape(shape).copy()
    return torch.tensor(array, dtype=torch.from_numpy(array).dtype).requires_grad_(requires_grad)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def canonical_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def tensor_content_hash(tensor: torch.Tensor) -> str:
    array = tensor.detach().cpu().contiguous().numpy()
    digest = hashlib.sha256()
    digest.update(str(array.dtype).encode("ascii"))
    digest.update(str(tuple(array.shape)).encode("ascii"))
    digest.update(array.tobytes(order="C"))
    return digest.hexdigest()

