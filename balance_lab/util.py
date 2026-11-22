from __future__ import annotations

import hashlib
import json
import math
from typing import Any


MASK64 = (1 << 64) - 1


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def content_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def stable_seed(*parts: int | str) -> int:
    material = "|".join(str(part) for part in parts).encode("utf-8")
    return int.from_bytes(hashlib.blake2b(material, digest_size=8).digest(), "big")


def derive_u64(seed: int, round_index: int, side: str) -> int:
    """Randomness is a pure function of a match seed and round/side labels.

    No process-global RNG is consulted, so scheduling order and worker thread
    cannot affect a combat result.
    """

    payload = f"{seed}:{round_index}:{side}".encode("ascii")
    return int.from_bytes(hashlib.blake2b(payload, digest_size=8).digest(), "big")


def match_seed(scenario_seed: int, sample_index: int) -> int:
    # Deterministic integer mixing avoids Python's randomized hash().
    value = (int(scenario_seed) + 0x9E3779B97F4A7C15 * (sample_index + 1)) & MASK64
    value ^= value >> 30
    value = (value * 0xBF58476D1CE4E5B9) & MASK64
    value ^= value >> 27
    value = (value * 0x94D049BB133111EB) & MASK64
    value ^= value >> 31
    return value


def wilson_interval(successes: int, trials: int, z: float = 1.959963984540054) -> tuple[int, int, int]:
    """Return a 95% Wilson interval in basis points.

    Statistics use floating point, while combat/damage accounting never does.
    """

    if trials <= 0:
        return 0, 10000, 10000
    successes = max(0, min(trials, int(successes)))
    p = successes / trials
    z2 = z * z
    denom = 1.0 + z2 / trials
    center = (p + z2 / (2.0 * trials)) / denom
    margin = z * math.sqrt((p * (1.0 - p) / trials + z2 / (4.0 * trials * trials)) / (denom * denom))
    low = max(0, min(10000, int(math.floor((center - margin) * 10000))))
    high = max(0, min(10000, int(math.ceil((center + margin) * 10000))))
    return low, high, high - low


def rate_bp(successes: int, trials: int) -> int:
    if trials <= 0:
        return 0
    return (int(successes) * 10000 + trials // 2) // trials


def safe_ratio_bp(numerator: int, denominator: int) -> int:
    if denominator == 0:
        return 0
    return (int(numerator) * 10000 + abs(denominator) // 2) // denominator


def apply_scale_bp(value: int, factor_bp: int) -> int:
    return (int(value) * int(factor_bp) + 5000) // 10000


def human_hp(value_milli: int) -> str:
    sign = "-" if value_milli < 0 else ""
    value = abs(int(value_milli))
    return f"{sign}{value // 1000}.{value % 1000:03d}"


def human_bp(value_bp: int) -> str:
    return f"{value_bp / 100:.2f}%"

