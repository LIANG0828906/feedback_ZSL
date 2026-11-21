"""Canonical state payloads and hash-chain construction."""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from .model import GENESIS_HASH, canonical_json, sha256_text


def state_payload(
    *,
    step: int,
    covers: Iterable[dict[str, Any]],
    pending: dict[str, int],
    revocations: Iterable[dict[str, Any]],
    previous_hash: str,
) -> dict[str, Any]:
    """Build the only payload allowed to influence state hashes.

    The key and array order are deliberate; callers must not sort the JSON
    output. Durability and pending damage stay in integer micro-units.
    """

    return {
        "step": int(step),
        "covers": [
            {
                "cover_id": item["cover_id"],
                "remaining": int(item["remaining"]),
                "cracked": bool(item["cracked"]),
                "alive": bool(item["alive"]),
            }
            for item in covers
        ],
        "pending": [
            [cover_id, int(pending[cover_id])] for cover_id in sorted(pending)
        ],
        "revocations": list(revocations),
        "previous_hash": previous_hash,
    }


def compute_state_hash(payload: dict[str, Any]) -> str:
    return sha256_text(canonical_json(payload))


def next_chain_link(
    *,
    step: int,
    covers: Iterable[dict[str, Any]],
    pending: dict[str, int],
    revocations: Iterable[dict[str, Any]],
    previous_hash: str,
) -> tuple[dict[str, Any], dict[str, Any]]:
    payload = state_payload(
        step=step,
        covers=covers,
        pending=pending,
        revocations=revocations,
        previous_hash=previous_hash,
    )
    digest = compute_state_hash(payload)
    link = {
        "step": step,
        "previous_hash": previous_hash,
        "state_hash": digest,
    }
    return payload, link


def chain_prefix_digest(chain: Iterable[dict[str, Any]]) -> str:
    return sha256_text(canonical_json(list(chain)))


def chain_tail(chain: Iterable[dict[str, Any]]) -> str:
    last = GENESIS_HASH
    for link in chain:
        last = str(link["state_hash"])
    return last

