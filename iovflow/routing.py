"""Deterministic, skew-aware routing by vehicle key.

Normally all events for a vehicle -- including payment and backfill events --
go to one shard.  If that vehicle would make a shard exceed the configured
fraction of all events, its trips are split across logical sub-shards.  The
same trip (and therefore its payment/backfill pair) always stays together
unless a single trip itself is the hot key; in that case event fingerprints are
used as the final split dimension.  The merge stage unions the sub-shards and
emits exactly one window row per vehicle/window.
"""

from __future__ import annotations

import heapq
import math
from dataclasses import dataclass, field
from typing import Any

from .errors import EngineError
from .util import stable_hash64


@dataclass
class VehicleRoute:
    vehicle_id: str
    part_count: int
    shards: list[int]
    trip_parts: dict[str, int]
    event_split: bool
    counts: list[int]


@dataclass
class RoutingPlan:
    total_events: int
    shard_count: int
    skew_cap: float
    vehicles: dict[str, VehicleRoute]
    shard_counts: list[int]
    split_vehicles: list[str] = field(default_factory=list)

    def shard_for(self, vehicle_id: str, trip_id: Any, fp: str) -> int:
        route = self.vehicles.get(str(vehicle_id))
        if route is None:
            return stable_hash64(str(vehicle_id)) % self.shard_count
        trip = "" if trip_id is None else str(trip_id)
        if route.event_split:
            part = stable_hash64(f"{vehicle_id}\x1f{trip}\x1f{fp}") % route.part_count
        else:
            part = route.trip_parts.get(trip)
            if part is None:
                part = stable_hash64(str(vehicle_id) + "\x1f" + trip) % route.part_count
        return route.shards[part]

    def shards_for_vehicle(self, vehicle_id: str) -> list[int]:
        route = self.vehicles.get(str(vehicle_id))
        if route is None:
            return [stable_hash64(str(vehicle_id)) % self.shard_count]
        return route.shards

    def to_dict(self) -> dict[str, Any]:
        return {
            "total_events": self.total_events,
            "shard_count": self.shard_count,
            "skew_cap": self.skew_cap,
            "shard_counts": self.shard_counts,
            "shard_ratios": [
                (c / self.total_events if self.total_events else 0.0) for c in self.shard_counts
            ],
            "split_vehicles": self.split_vehicles,
            "vehicles": {
                vid: {
                    "part_count": r.part_count,
                    "shards": r.shards,
                    "trip_parts": r.trip_parts,
                    "event_split": r.event_split,
                    "counts": r.counts,
                }
                for vid, r in sorted(self.vehicles.items())
            },
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "RoutingPlan":
        vehicles = {
            vid: VehicleRoute(
                vehicle_id=vid,
                part_count=int(r["part_count"]),
                shards=[int(x) for x in r["shards"]],
                trip_parts={str(k): int(v) for k, v in r.get("trip_parts", {}).items()},
                event_split=bool(r.get("event_split")),
                counts=[int(x) for x in r.get("counts", [])],
            )
            for vid, r in (data.get("vehicles") or {}).items()
        }
        return cls(
            total_events=int(data["total_events"]),
            shard_count=int(data["shard_count"]),
            skew_cap=float(data.get("skew_cap", 0.4)),
            vehicles=vehicles,
            shard_counts=[int(x) for x in data.get("shard_counts", [])],
            split_vehicles=list(data.get("split_vehicles", [])),
        )


def build_routing_plan(
    vehicle_trips: dict[str, dict[str, int]],
    total_events: int,
    parallelism: int,
    max_shards: int,
    skew_cap: float,
    samples: list[tuple[str, str, str]] | None = None,
) -> RoutingPlan:
    if total_events <= 0:
        raise EngineError("IOV_INPUT_EMPTY", "cannot route an empty input")
    cap_events = max(1, int(math.floor(total_events * skew_cap)))
    min_shards = max(parallelism, int(math.ceil(1.0 / skew_cap)))
    shard_count = max(min_shards, min(max_shards, min_shards + 1))
    shard_count = min(max_shards, shard_count)
    shard_count = max(shard_count, parallelism)

    # Start each vehicle as one part; split it when its count exceeds the cap.
    target_parts: dict[str, int] = {}
    event_split: set[str] = set()
    for vid, trips in vehicle_trips.items():
        count = sum(trips.values())
        parts = max(1, int(math.ceil(count / cap_events)))
        if parts > max(1, len(trips)):
            event_split.add(vid)
            parts = max(parts, int(math.ceil(count / cap_events)) + 1)
        target_parts[vid] = parts

    shard_counts: list[int] = []
    routes: dict[str, VehicleRoute] = {}
    for _ in range(200):
        routes, shard_counts = _assign(
            vehicle_trips, total_events, shard_count, cap_events, target_parts, event_split
        )
        if samples is not None:
            probe = RoutingPlan(
                total_events=total_events,
                shard_count=shard_count,
                skew_cap=skew_cap,
                vehicles=routes,
                shard_counts=shard_counts,
            )
            shard_counts = _actual_shard_counts(probe, samples)
        max_count = max(shard_counts) if shard_counts else 0
        if max_count <= cap_events:
            break
        # Increase splitting for the vehicle that contributes most to the
        # currently largest shard.
        worst_shard = max(range(len(shard_counts)), key=lambda i: shard_counts[i])
        contributions: dict[str, int] = {}
        if samples is not None:
            probe = RoutingPlan(
                total_events=total_events,
                shard_count=shard_count,
                skew_cap=skew_cap,
                vehicles=routes,
                shard_counts=shard_counts,
            )
            for vid, trip, fp in samples:
                if probe.shard_for(vid, trip, fp) == worst_shard:
                    contributions[vid] = contributions.get(vid, 0) + 1
        else:
            for vid, route in routes.items():
                for part, shard in enumerate(route.shards):
                    if shard == worst_shard:
                        contributions[vid] = contributions.get(vid, 0) + route.counts[part]
        if not contributions:
            raise EngineError("IOV_INPUT_INVALID", "routing imbalance could not be attributed")
        victim = max(contributions, key=lambda v: contributions[v])
        target_parts[victim] = target_parts.get(victim, 1) + 1
        if target_parts[victim] > max(2, len(vehicle_trips[victim])) + 2:
            event_split.add(victim)
    else:
        raise EngineError(
            "IOV_INPUT_INVALID",
            "could not satisfy the shard skew cap after 200 routing iterations",
            context={"cap_events": cap_events, "shard_counts": shard_counts},
        )

    routes, shard_counts = _assign(
        vehicle_trips, total_events, shard_count, cap_events, target_parts, event_split
    )
    if samples is not None:
        probe = RoutingPlan(
            total_events=total_events,
            shard_count=shard_count,
            skew_cap=skew_cap,
            vehicles=routes,
            shard_counts=shard_counts,
        )
        shard_counts = _actual_shard_counts(probe, samples)
    if max(shard_counts) > cap_events:
        raise EngineError(
            "IOV_INPUT_INVALID",
            "routing plan violates the shard skew cap",
            context={"cap_events": cap_events, "shard_counts": shard_counts},
        )
    split = sorted(
        vid
        for vid, route in routes.items()
        if route.part_count > 1 or len(set(route.shards)) > 1
    )
    return RoutingPlan(
        total_events=total_events,
        shard_count=shard_count,
        skew_cap=skew_cap,
        vehicles=routes,
        shard_counts=shard_counts,
        split_vehicles=split,
    )


def _actual_shard_counts(
    plan: RoutingPlan, samples: list[tuple[str, str, str]]
) -> list[int]:
    counts = [0] * plan.shard_count
    for vid, trip, fp in samples:
        counts[plan.shard_for(vid, trip, fp)] += 1
    return counts


def _assign(
    vehicle_trips: dict[str, dict[str, int]],
    total_events: int,
    shard_count: int,
    cap_events: int,
    target_parts: dict[str, int],
    event_split: set[str],
) -> tuple[dict[str, VehicleRoute], list[int]]:
    routes: dict[str, VehicleRoute] = {}
    items: list[tuple[int, str, int, list[int]]] = []
    for vid, trips in vehicle_trips.items():
        count = sum(trips.values())
        parts = max(1, int(target_parts.get(vid, 1)))
        if vid in event_split:
            # We cannot know the exact trip mapping for an event-split key, but
            # the expected part weight is enough for LPT placement.
            counts = _split_total(count, parts)
            trip_parts: dict[str, int] = {}
            for part, weight in enumerate(counts):
                items.append((weight, vid, part, counts))
            routes[vid] = VehicleRoute(
                vehicle_id=vid,
                part_count=parts,
                shards=[],
                trip_parts=trip_parts,
                event_split=True,
                counts=counts,
            )
        else:
            trip_parts = _balance_trips(trips, parts)
            counts = [0] * parts
            for trip, part in trip_parts.items():
                counts[part] += trips[trip]
            for part, weight in enumerate(counts):
                items.append((weight, vid, part, counts))
            routes[vid] = VehicleRoute(
                vehicle_id=vid,
                part_count=parts,
                shards=[],
                trip_parts=trip_parts,
                event_split=False,
                counts=counts,
            )
    # Longest-processing-time assignment with deterministic tie breaking.
    placements: dict[tuple[str, int], int] = {}
    shard_counts = [0] * shard_count
    heap = [(0, i) for i in range(shard_count)]
    heapq.heapify(heap)
    for weight, vid, part, counts in sorted(items, key=lambda x: (-x[0], x[1], x[2])):
        load, shard = heapq.heappop(heap)
        placements[(vid, part)] = shard
        shard_counts[shard] += weight
        heapq.heappush(heap, (load + weight, shard))
    for vid, route in routes.items():
        route.shards = [placements[(vid, part)] for part in range(route.part_count)]
    return routes, shard_counts


def _balance_trips(trips: dict[str, int], parts: int) -> dict[str, int]:
    loads = [0] * parts
    result: dict[str, int] = {}
    for trip, count in sorted(trips.items(), key=lambda kv: (-kv[1], kv[0])):
        part = min(range(parts), key=lambda i: (loads[i], i))
        result[trip] = part
        loads[part] += count
    return result


def _split_total(total: int, parts: int) -> list[int]:
    base = total // parts
    rem = total % parts
    return [base + (1 if i < rem else 0) for i in range(parts)]


def simulate_shard_counts(
    plan: RoutingPlan, vehicle_trips: dict[str, dict[str, int]]
) -> list[int]:
    counts = [0] * plan.shard_count
    for vid, trips in vehicle_trips.items():
        route = plan.vehicles.get(vid)
        if route is None:
            counts[stable_hash64(vid) % plan.shard_count] += sum(trips.values())
            continue
        if route.event_split:
            # Exact event hashes are not available here; distribute by the
            # recorded part weights.  The prescan path can sample hashes, but
            # the generator keeps hot vehicles multi-trip so this branch is
            # only a safety net.
            for part, weight in enumerate(route.counts):
                counts[route.shards[part]] += weight
        else:
            for trip, count in trips.items():
                part = route.trip_parts.get(trip)
                if part is None:
                    part = stable_hash64(vid + "\x1f" + trip) % route.part_count
                counts[route.shards[part]] += count
    return counts

