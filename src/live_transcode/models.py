from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, replace
from math import gcd
from typing import Any

from .errors import E_INVALID_ARGUMENT, TaskError


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


@dataclass(frozen=True)
class WavInfo:
    material_id: str
    sample_rate: int
    channels: int
    bits_per_sample: int
    channel_mask: int
    layout: str
    frames: int
    data_bytes: int
    format_tag: int
    source_hash: str
    file_size: int

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class TierParams:
    tier_id: str
    target_rate: int
    filter_length: int
    quant_bits: int
    param_version: int
    slice_samples: int = 2048

    def __post_init__(self) -> None:
        if not self.tier_id or any(ch.isspace() for ch in self.tier_id):
            raise TaskError(
                E_INVALID_ARGUMENT,
                "tier_id must be a non-empty token without whitespace",
                field="tier_id",
            )
        if not 8000 <= self.target_rate <= 192000:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "target_rate must be between 8000 and 192000",
                field="target_rate",
            )
        if self.filter_length < 8 or self.filter_length > 1024:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "filter_length must be between 8 and 1024",
                field="filter_length",
            )
        if self.filter_length % 2:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "filter_length must be even for symmetric half-window bookkeeping",
                field="filter_length",
            )
        if self.quant_bits not in (16, 24):
            raise TaskError(
                E_INVALID_ARGUMENT,
                "quant_bits must be 16 or 24",
                field="quant_bits",
            )
        if self.param_version < 1:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "param_version must be monotonic and >= 1",
                field="param_version",
            )
        if self.slice_samples < 32 or self.slice_samples > 65536:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "slice_samples must be between 32 and 65536",
                field="slice_samples",
            )

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)

    def changed(self, **changes: Any) -> "TierParams":
        next_version = max(
            self.param_version + 1,
            int(changes.pop("param_version", self.param_version + 1)),
        )
        return replace(self, **changes, param_version=next_version)

    def config_hash(self) -> str:
        payload = self.as_dict()
        payload.pop("param_version", None)
        return sha256_bytes(canonical_json_bytes(payload))


@dataclass(frozen=True)
class ResampleRatio:
    source_rate: int
    target_rate: int
    step_num: int
    step_den: int

    @classmethod
    def build(cls, source_rate: int, target_rate: int) -> "ResampleRatio":
        divisor = gcd(source_rate, target_rate)
        return cls(
            source_rate=source_rate,
            target_rate=target_rate,
            step_num=source_rate // divisor,
            step_den=target_rate // divisor,
        )

    def output_length(self, source_frames: int) -> int:
        return source_frames * self.target_rate // self.source_rate

    def phase(self, output_index: int) -> int:
        return (output_index * self.step_num) % self.step_den

    def phase_remainder(self, output_index: int, filter_length: int) -> int:
        half = filter_length // 2
        return (self.phase(output_index) + half * self.step_num) % self.step_den

