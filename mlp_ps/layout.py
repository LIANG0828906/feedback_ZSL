from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Mapping, Sequence, Tuple

from .config import SHARD_COUNT
from .errors import BusinessError, E_LAYOUT_CONFLICT
from .util import canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class LayoutEntry:
    shard_id: int
    param_name: str
    full_shape: Tuple[int, ...]
    start: int
    length: int

    @property
    def slice_shape(self) -> Tuple[int, ...]:
        return (self.length,) + self.full_shape[1:]

    @property
    def numel(self) -> int:
        return math.prod(self.slice_shape)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "shard_id": self.shard_id,
            "param_name": self.param_name,
            "full_shape": list(self.full_shape),
            "start": self.start,
            "length": self.length,
            "slice_shape": list(self.slice_shape),
            "numel": self.numel,
        }


@dataclass(frozen=True)
class Layout:
    spec: Dict[str, Any]
    entries: Tuple[LayoutEntry, ...]
    layout_id: str
    parameter_names: Tuple[str, ...]
    parameter_shapes: Mapping[str, Tuple[int, ...]]

    def entries_for_shard(self, shard_id: int) -> Tuple[LayoutEntry, ...]:
        return tuple(entry for entry in self.entries if entry.shard_id == shard_id)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "layout_id": self.layout_id,
            "spec": self.spec,
            "entries": [entry.to_dict() for entry in self.entries],
        }


def _split_ranges(size: int, count: int) -> List[Tuple[int, int]]:
    if size < count:
        raise BusinessError(
            E_LAYOUT_CONFLICT,
            "tensor dimension is too small for the requested shard count",
            {"dimension": size, "shard_count": count},
        )
    base = size // count
    remainder = size % count
    ranges: List[Tuple[int, int]] = []
    start = 0
    for shard_id in range(count):
        length = base + (1 if shard_id < remainder else 0)
        ranges.append((start, length))
        start += length
    return ranges


def build_layout(parameter_shapes: Sequence[Tuple[str, Tuple[int, ...]]], shard_count: int = SHARD_COUNT) -> Layout:
    if shard_count <= 0:
        raise BusinessError(E_LAYOUT_CONFLICT, "shard_count must be positive")
    if not parameter_shapes:
        raise BusinessError(E_LAYOUT_CONFLICT, "model has no parameters")

    parameter_entries: List[Dict[str, Any]] = []
    entries: List[LayoutEntry] = []
    name_to_shape: Dict[str, Tuple[int, ...]] = {}
    for name, shape_tuple in parameter_shapes:
        shape = tuple(int(v) for v in shape_tuple)
        if not shape:
            raise BusinessError(E_LAYOUT_CONFLICT, "scalar parameters cannot be sharded by tensor dimension")
        ranges = _split_ranges(shape[0], shard_count)
        slices: List[Dict[str, Any]] = []
        for shard_id, (start, length) in enumerate(ranges):
            entry = LayoutEntry(
                shard_id=shard_id,
                param_name=name,
                full_shape=shape,
                start=start,
                length=length,
            )
            entries.append(entry)
            slices.append(
                {
                    "shard_id": shard_id,
                    "start": start,
                    "length": length,
                    "shape": list(entry.slice_shape),
                    "numel": entry.numel,
                }
            )
        parameter_entries.append(
            {
                "name": name,
                "shape": list(shape),
                "slices": slices,
            }
        )
        name_to_shape[name] = shape

    spec = {
        "format": "mlp-ps-layout-v1",
        "shard_count": shard_count,
        "parameters": parameter_entries,
    }
    validate_layout_spec(spec)
    layout_id = sha256_bytes(canonical_json_bytes(spec))[:16]
    return Layout(
        spec=spec,
        entries=tuple(entries),
        layout_id=layout_id,
        parameter_names=tuple(name for name, _ in parameter_shapes),
        parameter_shapes=name_to_shape,
    )


def validate_layout_spec(spec: Mapping[str, Any]) -> None:
    if spec.get("format") != "mlp-ps-layout-v1":
        raise BusinessError(E_LAYOUT_CONFLICT, "unsupported layout format")
    shard_count = int(spec.get("shard_count", 0))
    if shard_count <= 0:
        raise BusinessError(E_LAYOUT_CONFLICT, "layout shard_count must be positive")
    parameters = spec.get("parameters")
    if not isinstance(parameters, list) or not parameters:
        raise BusinessError(E_LAYOUT_CONFLICT, "layout has no parameter entries")

    seen_names = set()
    for parameter in parameters:
        name = parameter.get("name")
        shape = tuple(int(v) for v in parameter.get("shape", []))
        if not name or not shape or name in seen_names:
            raise BusinessError(E_LAYOUT_CONFLICT, "layout parameter names/shapes are invalid")
        seen_names.add(name)
        slices = parameter.get("slices")
        if not isinstance(slices, list) or len(slices) != shard_count:
            raise BusinessError(
                E_LAYOUT_CONFLICT,
                "every parameter must have exactly one slice per shard",
                {"parameter": name},
            )

        covered = [0] * shape[0]
        seen_shards = set()
        for slice_info in slices:
            shard_id = int(slice_info["shard_id"])
            start = int(slice_info["start"])
            length = int(slice_info["length"])
            slice_shape = tuple(int(v) for v in slice_info["shape"])
            if shard_id in seen_shards:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "duplicate shard declaration for a parameter",
                    {"parameter": name, "shard_id": shard_id},
                )
            seen_shards.add(shard_id)
            if start < 0 or length <= 0 or start + length > shape[0]:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "parameter slice is outside the tensor dimension",
                    {"parameter": name, "shard_id": shard_id},
                )
            expected_shape = (length,) + shape[1:]
            if slice_shape != expected_shape:
                raise BusinessError(
                    E_LAYOUT_CONFLICT,
                    "parameter slice shape does not match its range",
                    {"parameter": name, "shard_id": shard_id},
                )
            for index in range(start, start + length):
                covered[index] += 1

        if seen_shards != set(range(shard_count)):
            raise BusinessError(
                E_LAYOUT_CONFLICT,
                "parameter slices do not cover every shard id",
                {"parameter": name},
            )
        if any(count != 1 for count in covered):
            raise BusinessError(
                E_LAYOUT_CONFLICT,
                "parameter slices must be non-overlapping and cover the full tensor",
                {"parameter": name},
            )


def entries_from_spec(spec: Mapping[str, Any]) -> Tuple[LayoutEntry, ...]:
    validate_layout_spec(spec)
    entries: List[LayoutEntry] = []
    for parameter in spec["parameters"]:
        name = str(parameter["name"])
        full_shape = tuple(int(v) for v in parameter["shape"])
        for slice_info in parameter["slices"]:
            entries.append(
                LayoutEntry(
                    shard_id=int(slice_info["shard_id"]),
                    param_name=name,
                    full_shape=full_shape,
                    start=int(slice_info["start"]),
                    length=int(slice_info["length"]),
                )
            )
    return tuple(entries)


def parameter_shapes_from_spec(spec: Mapping[str, Any]) -> Dict[str, Tuple[int, ...]]:
    validate_layout_spec(spec)
    return {
        str(parameter["name"]): tuple(int(v) for v in parameter["shape"])
        for parameter in spec["parameters"]
    }
