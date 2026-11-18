"""Deterministic logical slot assignment with hot-key window splitting."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .util import stable_hash64


@dataclass(frozen=True)
class SlotAssignment:
    slot_by_index: dict[int, int]
    slot_loads: list[int]
    total_events: int
    max_share: float
    splits: list[dict[str, Any]]
    heavy_keys: list[str]
    unsplittable_heavy_keys: list[str]

    def group_indices_by_slot(self) -> dict[int, list[int]]:
        result: dict[int, list[int]] = {}
        for index, slot in self.slot_by_index.items():
            result.setdefault(slot, []).append(index)
        for indices in result.values():
            indices.sort()
        return result


def assign_slots(
    groups: Iterable[dict[str, Any]],
    *,
    slots: int,
    threshold: float = 0.40,
) -> SlotAssignment:
    """Assign key-window groups to slots.

    Normal keys stay together on one slot.  A key whose canonical event load
    exceeds the threshold is expanded into one unit per minute window; the
    payment and refund events of a given key-window always travel together.
    Units are placed largest-first on the least loaded slot, with stable
    tie-breaking.
    """

    if slots < 1:
        raise ValueError("slots must be >= 1")
    materialized = list(groups)
    def event_count(group: dict[str, Any]) -> int:
        if "event_count" in group:
            return int(group["event_count"])
        return len(group.get("events", []))

    total = sum(event_count(group) for group in materialized)
    if total == 0:
        return SlotAssignment({}, [0] * slots, 0, 0.0, [], [], [])

    by_key: dict[str, list[int]] = {}
    key_load: dict[str, int] = {}
    for index, group in enumerate(materialized):
        key = str(group["payment_id"])
        by_key.setdefault(key, []).append(index)
        key_load[key] = key_load.get(key, 0) + event_count(group)

    heavy: set[str] = set()
    unsplittable: list[str] = []
    for key, indices in by_key.items():
        if key_load[key] > threshold * total:
            if len(indices) > 1 and slots > 1:
                heavy.add(key)
            else:
                unsplittable.append(key)

    units: list[tuple[int, str, list[int], str]] = []
    for key, indices in by_key.items():
        if key in heavy:
            for index in indices:
                group = materialized[index]
                unit_key = f"window|{key}|{int(group['window_start'])}"
                units.append((event_count(group), unit_key, [index], key))
        else:
            load = sum(event_count(materialized[index]) for index in indices)
            unit_key = f"key|{stable_hash64(key):016x}|{key}"
            units.append((load, unit_key, indices, key))

    units.sort(key=lambda item: (-item[0], item[1]))
    slot_loads = [0] * slots
    slot_by_index: dict[int, int] = {}
    key_slots: dict[str, set[int]] = {key: set() for key in heavy}
    for load, unit_key, indices, key in units:
        minimum = min(slot_loads)
        candidates = [
            candidate for candidate, current in enumerate(slot_loads) if current == minimum
        ]
        slot = candidates[stable_hash64(unit_key) % len(candidates)]
        slot_loads[slot] += load
        for index in indices:
            slot_by_index[index] = slot
        if key in key_slots:
            key_slots[key].add(slot)

    splits: list[dict[str, Any]] = []
    for key in sorted(key_slots):
        slots_used = sorted(key_slots[key])
        if len(slots_used) > 1:
            splits.append(
                {
                    "payment_id": key,
                    "event_count": key_load[key],
                    "share": round(key_load[key] / total, 8),
                    "window_count": len(by_key[key]),
                    "slots": slots_used,
                    "reason": "canonical key load exceeds slot threshold; split by minute window",
                }
            )

    max_load = max(slot_loads) if slot_loads else 0
    return SlotAssignment(
        slot_by_index=slot_by_index,
        slot_loads=slot_loads,
        total_events=total,
        max_share=(max_load / total) if total else 0.0,
        splits=splits,
        heavy_keys=sorted(heavy),
        unsplittable_heavy_keys=sorted(unsplittable),
    )
