"""Key-based slot assignment with skew splitting.

Both join sides are distributed by the same business key (``order_id``),
so orders and refunds of one key always land on the same slot.  Skew is
judged against the job's *cumulative* event count: whenever a slot's
cumulative load would exceed ``threshold`` of all events seen so far,
the hottest key on that slot is split into more sub-keys salted by
``event_id``.  The same salt function is applied to both join sides, so
every (key, part) still holds both sides of that key and partial
aggregates merge back to the exact unsplit result.
"""

from __future__ import annotations

import hashlib
import math
from collections import Counter

_MAX_ATTEMPTS = 256


def _hash(text: str) -> int:
    return int.from_bytes(hashlib.sha256(text.encode("utf-8")).digest()[:8], "big")


def assign_slots(
    events: list[dict],
    slots: int,
    threshold: float,
    cum_counts: dict[str, int],
    cum_loads: list[int],
    total_cum: int,
) -> tuple[list[int], list[dict], float]:
    """Assign each event of the current batch to a processing slot.

    ``cum_counts`` / ``cum_loads`` / ``total_cum`` already include this
    batch's events.  Returns ``(slot_per_event, split_records,
    max_cumulative_load_ratio)``; ``split_records`` lists
    ``{"key", "parts"}`` for every hot key split in this batch.
    """
    total = len(events)
    if total == 0:
        ratio = max(cum_loads) / total_cum if total_cum else 0.0
        return [], [], ratio
    slots = max(1, slots)
    limit = max(1.0, threshold * total_cum)

    batch_counts = Counter(e["order_id"] for e in events)
    parts: dict[str, int] = {
        k: max(2, math.ceil(cum_counts[k] / limit))
        for k in batch_counts
        if cum_counts[k] > limit
    }
    salt = 0

    best: tuple[float, list[int], list[int]] | None = None
    for _ in range(_MAX_ATTEMPTS):
        slot_of: list[int] = []
        loads = list(cum_loads)
        for e in events:
            key = e["order_id"]
            n_parts = parts.get(key, 1)
            if n_parts == 1:
                slot = _hash(f"{salt}|{key}") % slots
            else:
                part = _hash(f"{salt}|{e['event_id']}") % n_parts
                slot = _hash(f"{salt}|{key}#{part}") % slots
            slot_of.append(slot)
            loads[slot] += 1
        ratio = max(loads) / total_cum
        if best is None or ratio < best[0]:
            best = (ratio, slot_of, loads)
        if max(loads) <= limit:
            best = (ratio, slot_of, loads)
            break
        # split the hottest key on the most loaded slot (more parts), or
        # re-hash with a fresh salt when no key there can be split further
        hottest_slot = loads.index(max(loads))
        candidates: dict[str, int] = {}
        for e, slot in zip(events, slot_of):
            if slot != hottest_slot:
                continue
            key = e["order_id"]
            n_parts = parts.get(key, 1)
            if n_parts < cum_counts[key]:
                candidates[key] = cum_counts[key] // n_parts
        if candidates:
            hottest_key = max(sorted(candidates), key=lambda k: candidates[k])
            parts[hottest_key] = parts.get(hottest_key, 1) + 1
        else:
            salt += 1

    ratio, slot_of, loads = best
    cum_loads[:] = loads
    splits = [{"key": k, "parts": parts[k]} for k in sorted(parts) if parts[k] > 1]
    return slot_of, splits, ratio
