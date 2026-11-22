"""Path-independent deterministic random derivation.

Every random decision is addressed by (match seed, tick, input event index,
purpose).  No process-global RNG, call-order counter, or thread scheduler state
can affect the result.
"""

from __future__ import annotations

import hashlib

from .numeric import BPS


def derive_u64(seed: int, tick: int, event_index: int, purpose: str) -> int:
    message = (
        f"{seed}:{tick}:{event_index}:{purpose}".encode("utf-8")
    )
    digest = hashlib.blake2b(
        message,
        digest_size=8,
        person=b"tickarena",
    ).digest()
    return int.from_bytes(digest, "big", signed=False)


def roll_bps(seed: int, tick: int, event_index: int, purpose: str) -> int:
    return derive_u64(seed, tick, event_index, purpose) % BPS


def signed_jitter_bps(
    seed: int,
    tick: int,
    event_index: int,
    purpose: str,
    span_bps: int,
) -> int:
    if span_bps < 0:
        raise ValueError("span_bps must be non-negative")
    width = span_bps * 2 + 1
    return (derive_u64(seed, tick, event_index, purpose) % width) - span_bps

