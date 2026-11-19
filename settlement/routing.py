from __future__ import annotations

import hashlib
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from typing import Any, Iterable


@dataclass
class SkewRecord:
    vehicle_id: str
    telemetry_count: int
    share: float
    split: bool
    slots: list[int]
    unit_count: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "vehicle_id": self.vehicle_id,
            "telemetry_count": self.telemetry_count,
            "share": round(self.share, 9),
            "split": self.split,
            "slots": sorted(self.slots),
            "unit_count": self.unit_count,
        }


@dataclass
class RoutingPlan:
    slots: int
    skew_threshold_ratio: float
    vehicle_slots: dict[str, int] = field(default_factory=dict)
    unit_slots: dict[tuple[str, str, int], int] = field(default_factory=dict)
    split_vehicles: dict[str, list[int]] = field(default_factory=dict)
    vehicle_counts: dict[str, int] = field(default_factory=dict)
    skew_records: list[SkewRecord] = field(default_factory=list)

    def route(self, event: dict[str, Any]) -> int:
        vehicle = event["vehicle_id"]
        if vehicle in self.split_vehicles:
            key = (vehicle, event["trip_id"], int(event["window_start_us"]))
            slot = self.unit_slots.get(key)
            if slot is not None:
                return slot
            # Supplements sometimes have no telemetry event in the same
            # window. A stable fallback keeps both paths identical.
            choices = self.split_vehicles[vehicle]
            digest = hashlib.blake2b(
                f"{vehicle}|{event['trip_id']}|{event['window_start_us']}".encode("utf-8"),
                digest_size=8,
            ).digest()
            return choices[int.from_bytes(digest, "big") % len(choices)]
        return self.vehicle_slots[vehicle]

    def stats(self) -> list[dict[str, Any]]:
        return [record.to_dict() for record in sorted(self.skew_records, key=lambda item: item.vehicle_id)]


def build_routing(
    telemetry_events: Iterable[dict[str, Any]],
    *,
    slots: int,
    skew_threshold_ratio: float,
) -> RoutingPlan:
    if slots < 3:
        raise ValueError("at least three logical processing slots are required")
    vehicle_counts: Counter[str] = Counter()
    unit_counts: Counter[tuple[str, str, int]] = Counter()
    for event in telemetry_events:
        vehicle = event["vehicle_id"]
        vehicle_counts[vehicle] += 1
        unit_counts[(vehicle, event["trip_id"], int(event["window_start_us"]))] += 1
    total = sum(vehicle_counts.values())
    if total <= 0:
        return RoutingPlan(slots=slots, skew_threshold_ratio=skew_threshold_ratio)

    split_vehicles: dict[str, list[int]] = {}
    skew_records: list[SkewRecord] = []
    for vehicle, count in sorted(vehicle_counts.items()):
        share = count / total
        if share > skew_threshold_ratio:
            split_vehicles[vehicle] = []
            skew_records.append(
                SkewRecord(
                    vehicle_id=vehicle,
                    telemetry_count=count,
                    share=share,
                    split=True,
                    slots=[],
                    unit_count=sum(1 for key in unit_counts if key[0] == vehicle),
                )
            )

    loads = [0] * slots
    vehicle_slots: dict[str, int] = {}
    unit_slots: dict[tuple[str, str, int], int] = {}

    units: list[tuple[int, int, str, tuple[str, str, int] | None, str]] = []
    for vehicle, count in vehicle_counts.items():
        if vehicle in split_vehicles:
            for key, unit_count in unit_counts.items():
                if key[0] == vehicle:
                    digest = hashlib.blake2b(
                        f"{key[0]}|{key[1]}|{key[2]}".encode("utf-8"), digest_size=8
                    ).hexdigest()
                    units.append((unit_count, 1, digest, key, vehicle))
        else:
            units.append((count, 0, vehicle, None, vehicle))
    units.sort(key=lambda item: (-item[0], item[1], item[2]))

    for count, _kind, _sort_key, key, vehicle in units:
        slot = min(range(slots), key=lambda candidate: (loads[candidate], candidate))
        loads[slot] += count
        if key is None:
            vehicle_slots[vehicle] = slot
        else:
            unit_slots[key] = slot
            split_vehicles[vehicle].append(slot)

    for record in skew_records:
        record.slots = sorted(set(split_vehicles[record.vehicle_id]))
    return RoutingPlan(
        slots=slots,
        skew_threshold_ratio=skew_threshold_ratio,
        vehicle_slots=vehicle_slots,
        unit_slots=unit_slots,
        split_vehicles=split_vehicles,
        vehicle_counts=dict(vehicle_counts),
        skew_records=skew_records,
    )

