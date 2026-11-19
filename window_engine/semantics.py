"""Shared event-time semantics used by BOTH the pure-batch and streaming paths.

Rules (identical for batch and stream, so results can never drift):

* Tumbling event-time windows of ``window_size_ms`` aligned to the epoch.
* Watermark = max(declared source event time) - allowed_lateness_ms, monotonic.
* A window [w, w+size) is closed once watermark >= w + size.
* An event whose window is already closed at arrival is *late*: counted, never
  applied to the emitted window result.
* Dedup key is (payment_id, window_start, event_id): the first occurrence wins.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .util import stable_hash

WATERMARK_INF = 10**15


def window_start(event_time_ms: int, window_size_ms: int) -> int:
    return (event_time_ms // window_size_ms) * window_size_ms


def is_window_closed(watermark_ms: int, w_start: int, window_size_ms: int) -> bool:
    return watermark_ms >= w_start + window_size_ms


def dedup_key(payment_id: str, w_start: int, event_id: str) -> str:
    return f"{payment_id}|{w_start}|{event_id}"


class Watermark:
    """Monotonic event-time watermark driven by source-declared timestamps."""

    def __init__(self, allowed_lateness_ms: int):
        self.allowed_lateness_ms = allowed_lateness_ms
        self.max_declared = 0
        self.value = 0

    def observe(
        self,
        declared_event_time_ms: int,
        *,
        force_lateness_zero: bool = False,
    ) -> int:
        """Advance the watermark after a source unit has been consumed."""

        if declared_event_time_ms > self.max_declared:
            self.max_declared = declared_event_time_ms
        lateness = 0 if force_lateness_zero else self.allowed_lateness_ms
        candidate = self.max_declared - lateness
        if candidate > self.value:
            self.value = candidate
        return self.value

    def finalize(self) -> int:
        """End of input: every remaining window must close."""

        self.value = WATERMARK_INF
        return self.value

    def snapshot(self) -> dict:
        return {
            "allowed_lateness_ms": self.allowed_lateness_ms,
            "max_declared": self.max_declared,
            "value": self.value,
        }

    @classmethod
    def restore(cls, state: dict) -> "Watermark":
        wm = cls(state["allowed_lateness_ms"])
        wm.max_declared = state["max_declared"]
        wm.value = state["value"]
        return wm


@dataclass
class SplitPlan:
    """Deterministic mapping of payment keys onto processing units.

    Hot keys that would push a unit above ``skew_ratio`` of total events are
    split into sub-units and re-merged at output time.
    """

    partitions: int
    skew_ratio: float
    total_events: int
    hot_keys: dict[str, int] = field(default_factory=dict)
    # key -> number of sub-units it was split into
    split_subunits: dict[str, int] = field(default_factory=dict)
    records: list[dict] = field(default_factory=list)

    def is_split(self, payment_id: str) -> bool:
        return payment_id in self.split_subunits

    def unit_for(self, payment_id: str, event_id: str) -> int:
        """Return the physical processing unit id for one event."""

        if payment_id in self.split_subunits:
            sub_count = self.split_subunits[payment_id]
            subunit = stable_hash(f"{payment_id}#{event_id}") % sub_count
            return subunit % self.partitions
        return stable_hash(payment_id) % self.partitions

    @staticmethod
    def build(
        key_counts: dict[str, int], partitions: int, skew_ratio: float
    ) -> "SplitPlan":
        total = sum(key_counts.values())
        plan = SplitPlan(
            partitions=partitions,
            skew_ratio=skew_ratio,
            total_events=total,
            hot_keys=dict(key_counts),
        )
        if total == 0:
            return plan
        threshold = skew_ratio * total

        unit_loads = [0] * partitions
        assignments: dict[str, int] = {}
        for key in sorted(key_counts):
            unit = stable_hash(key) % partitions
            assignments[key] = unit
            unit_loads[unit] += key_counts[key]

        # Iteratively split the largest key of any overloaded unit. Only keys
        # individually above the per-unit threshold are eligible.
        guard = 0
        while max(unit_loads) > threshold and guard < len(key_counts) + 1:
            guard += 1
            overloaded = max(
                range(partitions), key=lambda u: (unit_loads[u], -u)
            )
            members = [
                (key, key_counts[key])
                for key, unit in assignments.items()
                if unit == overloaded and key not in plan.split_subunits
            ]
            if not members:
                break
            key, count = max(members, key=lambda item: (item[1], item[0]))
            if count <= threshold:
                break
            old_unit = assignments[key]
            # Virtual sub-units map onto the fixed physical units; record the
            # smallest multiple of partitions that gives a stable event-level
            # hash spread while each physical unit stays under the threshold.
            sub_count = partitions
            while count / partitions > threshold and sub_count < count:
                sub_count += partitions
            plan.split_subunits[key] = sub_count
            plan.records.append(
                {
                    "payment_id": key,
                    "events": count,
                    "original_unit": old_unit,
                    "subunits": sub_count,
                }
            )
            unit_loads[old_unit] -= count
            base, remainder = divmod(count, partitions)
            for unit in range(partitions):
                unit_loads[unit] += base + (1 if unit < remainder else 0)
        return plan

    def to_dict(self) -> dict:
        return {
            "partitions": self.partitions,
            "skew_ratio": self.skew_ratio,
            "total_events": self.total_events,
            "split_records": sorted(self.records, key=lambda r: r["payment_id"]),
        }
