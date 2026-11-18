from __future__ import annotations

import sys
from array import array
from typing import Any, Dict, Iterable, List, Mapping, Sequence, Tuple

import torch


FLOAT32_BYTES = 4


def tensor_to_bytes(tensor: torch.Tensor) -> bytes:
    value = tensor.detach().to(device="cpu", dtype=torch.float32).contiguous()
    values = array("f", value.view(-1).tolist())
    if sys.byteorder != "little":
        values.byteswap()
    return values.tobytes()


def bytes_to_tensor(data: bytes, shape: Sequence[int]) -> torch.Tensor:
    expected = FLOAT32_BYTES
    for dimension in shape:
        expected *= int(dimension)
    if len(data) != expected:
        raise ValueError(f"tensor payload has {len(data)} bytes, expected {expected}")
    if sys.byteorder == "little":
        tensor = torch.frombuffer(bytearray(data), dtype=torch.float32).clone()
    else:
        values = array("f")
        values.frombytes(data)
        values.byteswap()
        tensor = torch.tensor(values.tolist(), dtype=torch.float32)
    return tensor.reshape(tuple(int(v) for v in shape))


def pack_named_tensors(
    ordered_names: Sequence[str],
    tensors: Mapping[str, torch.Tensor],
) -> Tuple[List[Dict[str, Any]], bytes]:
    descriptors: List[Dict[str, Any]] = []
    chunks: List[bytes] = []
    offset = 0
    for name in ordered_names:
        tensor = tensors[name]
        data = tensor_to_bytes(tensor)
        descriptors.append(
            {
                "name": name,
                "shape": [int(v) for v in tensor.shape],
                "offset": offset,
                "nbytes": len(data),
            }
        )
        chunks.append(data)
        offset += len(data)
    return descriptors, b"".join(chunks)


def unpack_named_tensors(
    descriptors: Sequence[Mapping[str, Any]],
    payload: bytes,
    expected_order: Sequence[str],
    expected_shapes: Mapping[str, Sequence[int]],
) -> Dict[str, torch.Tensor]:
    if [str(item.get("name")) for item in descriptors] != list(expected_order):
        raise ValueError("tensor segment order does not match the expected parameter order")
    result: Dict[str, torch.Tensor] = {}
    for descriptor in descriptors:
        name = str(descriptor["name"])
        shape = tuple(int(v) for v in descriptor["shape"])
        expected_shape = tuple(int(v) for v in expected_shapes[name])
        if shape != expected_shape:
            raise ValueError(
                f"tensor shape mismatch for {name}: got {shape}, expected {expected_shape}"
            )
        offset = int(descriptor["offset"])
        nbytes = int(descriptor["nbytes"])
        if offset < 0 or nbytes < 0 or offset + nbytes > len(payload):
            raise ValueError(f"tensor segment for {name} is outside the payload")
        result[name] = bytes_to_tensor(payload[offset : offset + nbytes], shape)
    return result
