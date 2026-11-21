from __future__ import annotations

import hashlib
import json
import math
from typing import Any

PRECISION = 9


def qfloat(value: float, precision: int = PRECISION) -> float:
    value = float(value)
    if not math.isfinite(value):
        raise ValueError(f"non-finite number: {value!r}")
    rounded = round(value, precision)
    if rounded == 0:
        return 0.0
    return rounded


def normalize(value: Any) -> Any:
    if isinstance(value, float):
        return qfloat(value)
    if isinstance(value, (list, tuple)):
        return [normalize(item) for item in value]
    if isinstance(value, dict):
        return {str(key): normalize(item) for key, item in value.items()}
    if hasattr(value, "to_dict") and callable(value.to_dict):
        return normalize(value.to_dict())
    return value


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        normalize(value),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def stable_int(*parts: Any, bits: int = 64) -> int:
    payload = canonical_json_bytes(list(parts))
    digest = hashlib.blake2b(payload, digest_size=8).digest()
    return int.from_bytes(digest, "big") & ((1 << bits) - 1)


class DeterministicRNG:
    """SplitMix64-based RNG whose sequence is independent of Python's random."""

    MASK = (1 << 64) - 1

    def __init__(self, seed: int):
        self.state = int(seed) & self.MASK

    def next_u64(self) -> int:
        self.state = (self.state + 0x9E3779B97F4A7C15) & self.MASK
        z = self.state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & self.MASK
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & self.MASK
        return (z ^ (z >> 31)) & self.MASK

    def random(self) -> float:
        return (self.next_u64() >> 11) / float(1 << 53)

    def uniform(self, low: float, high: float) -> float:
        return low + (high - low) * self.random()

    def randint(self, low: int, high: int) -> int:
        if high < low:
            raise ValueError("empty integer range")
        return low + self.next_u64() % (high - low + 1)

