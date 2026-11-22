"""Stroke seed derivation.

The *only* inputs to a stroke's random channels are the scene content hash and
the global stroke index.  No thread id, wall clock, process id, or global RNG is
read here.  This is deliberately boring code so it can be audited and tested.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from functools import lru_cache

from .errors import SeedDivergenceError


def derive_u64(content_hash: str, stroke_id: int, channel: str) -> int:
    if stroke_id < 0:
        raise ValueError("stroke_id must be non-negative")
    material = f"npr-seed-v1|{content_hash}|{int(stroke_id)}|{channel}".encode("ascii")
    digest = hashlib.sha256(material).digest()
    return int.from_bytes(digest[:8], "big", signed=False)


def derive_unit(content_hash: str, stroke_id: int, channel: str) -> float:
    # (u + 0.5) / 2**64 keeps every bucket strictly inside (0, 1).
    return (derive_u64(content_hash, stroke_id, channel) + 0.5) / float(1 << 64)


@dataclass(frozen=True)
class StrokeRandomChannels:
    order: float
    presence: float
    anchor_jitter_x: float
    anchor_jitter_y: float
    length_perturbation: float
    angle: float
    bend_sign: float
    bend_amount: float
    width_perturbation: float
    tint_perturbation: float

    def fingerprint_material(self) -> bytes:
        values = (
            self.order,
            self.presence,
            self.anchor_jitter_x,
            self.anchor_jitter_y,
            self.length_perturbation,
            self.angle,
            self.bend_sign,
            self.bend_amount,
            self.width_perturbation,
            self.tint_perturbation,
        )
        return "|".join(f"{value:.17g}" for value in values).encode("ascii")


@lru_cache(maxsize=65_536)
def stroke_channels(content_hash: str, stroke_id: int) -> StrokeRandomChannels:
    """Derive all channels with one SHA-512 digest for the pair.

    The cache key is deliberately only ``(content_hash, stroke_id)``.  Even a
    cache hit can therefore never leak a worker-local or time-local value into
    a stroke.
    """
    material = f"npr-seed-v1|{content_hash}|{int(stroke_id)}|channels".encode("ascii")
    digest = hashlib.sha512(material).digest()
    units = tuple(
        (int.from_bytes(digest[offset : offset + 8], "big") + 0.5) / float(1 << 64)
        for offset in range(0, 80, 8)
    )
    return StrokeRandomChannels(
        order=units[0],
        presence=units[1],
        anchor_jitter_x=units[2],
        anchor_jitter_y=units[3],
        length_perturbation=units[4],
        angle=units[5],
        bend_sign=units[6],
        bend_amount=units[7],
        width_perturbation=units[8],
        tint_perturbation=units[9],
    )


@lru_cache(maxsize=65_536)
def stroke_fingerprint(content_hash: str, stroke_id: int) -> str:
    channels = stroke_channels(content_hash, stroke_id)
    return hashlib.sha256(
        f"{content_hash}|{stroke_id}|".encode("ascii") + channels.fingerprint_material()
    ).hexdigest()


def verify_stroke_fingerprint(
    content_hash: str,
    stroke_id: int,
    observed: str,
) -> None:
    expected = stroke_fingerprint(content_hash, stroke_id)
    if observed != expected:
        raise SeedDivergenceError(
            stroke_id,
            "fingerprint does not match content-hash/index derivation",
        )

