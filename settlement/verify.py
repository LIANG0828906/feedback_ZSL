from __future__ import annotations

from collections import Counter
from decimal import Decimal
from typing import Any

from .state import StateStore
from .timeutil import energy_per_100km, iso_us
from .util import canonical_json
from .engine import _utc_now


def compare_plan_results(
    store: StateStore,
    *,
    push_run_id: str,
    replay_run_id: str,
) -> dict[str, Any]:
    push_rows = {
        (row["vehicle_id"], int(row["window_start_us"])): row
        for row in store.fetchall(
            "SELECT * FROM stage_vehicle_energy WHERE run_id=?", (push_run_id,)
        )
    }
    replay_rows = {
        (row["vehicle_id"], int(row["window_start_us"])): row
        for row in store.fetchall(
            "SELECT * FROM stage_vehicle_energy WHERE run_id=?", (replay_run_id,)
        )
    }
    all_keys = sorted(set(push_rows) | set(replay_rows))
    mismatches: list[dict[str, Any]] = []
    max_mileage_diff = 0
    max_energy_diff = 0
    max_event_count_diff = 0
    max_ratio_diff = Decimal("0")
    for vehicle, window_us in all_keys:
        push = push_rows.get((vehicle, window_us))
        replay = replay_rows.get((vehicle, window_us))
        if push is None or replay is None:
            mismatches.append(
                {
                    "vehicle_id": vehicle,
                    "window_start": iso_us(window_us),
                    "reason": "missing_on_one_path",
                }
            )
            continue
        mileage_diff = abs(int(push["mileage_u"]) - int(replay["mileage_u"]))
        energy_diff = abs(int(push["energy_u"]) - int(replay["energy_u"]))
        event_count_diff = abs(int(push["event_count"]) - int(replay["event_count"]))
        telemetry_count_diff = abs(
            int(push["telemetry_count"]) - int(replay["telemetry_count"])
        )
        supplement_count_diff = abs(
            int(push["supplement_count"]) - int(replay["supplement_count"])
        )
        push_ratio = Decimal(energy_per_100km(int(push["mileage_u"]), int(push["energy_u"])))
        replay_ratio = Decimal(
            energy_per_100km(int(replay["mileage_u"]), int(replay["energy_u"]))
        )
        ratio_diff = abs(push_ratio - replay_ratio)
        max_mileage_diff = max(max_mileage_diff, mileage_diff)
        max_energy_diff = max(max_energy_diff, energy_diff)
        max_event_count_diff = max(max_event_count_diff, event_count_diff)
        max_ratio_diff = max(max_ratio_diff, ratio_diff)
        if (
            mileage_diff
            or energy_diff
            or event_count_diff
            or telemetry_count_diff
            or supplement_count_diff
            or ratio_diff > Decimal("0.000000")
        ):
            mismatches.append(
                {
                    "vehicle_id": vehicle,
                    "window_start": iso_us(window_us),
                    "mileage_diff_micro_units": mileage_diff,
                    "energy_diff_micro_units": energy_diff,
                    "event_count_diff": event_count_diff,
                    "telemetry_count_diff": telemetry_count_diff,
                    "supplement_count_diff": supplement_count_diff,
                    "energy_per_100km_diff": str(ratio_diff),
                }
            )

    per_vehicle: dict[str, dict[str, Any]] = {}
    for (vehicle, window_us), row in push_rows.items():
        item = per_vehicle.setdefault(
            vehicle,
            {
                "vehicle_id": vehicle,
                "push_mileage_u": 0,
                "push_energy_u": 0,
                "replay_mileage_u": 0,
                "replay_energy_u": 0,
                "push_event_count": 0,
                "replay_event_count": 0,
            },
        )
        item["push_mileage_u"] += int(row["mileage_u"])
        item["push_energy_u"] += int(row["energy_u"])
        item["push_event_count"] += int(row["event_count"])
    for (vehicle, window_us), row in replay_rows.items():
        item = per_vehicle.setdefault(
            vehicle,
            {
                "vehicle_id": vehicle,
                "push_mileage_u": 0,
                "push_energy_u": 0,
                "replay_mileage_u": 0,
                "replay_energy_u": 0,
                "push_event_count": 0,
                "replay_event_count": 0,
            },
        )
        item["replay_mileage_u"] += int(row["mileage_u"])
        item["replay_energy_u"] += int(row["energy_u"])
        item["replay_event_count"] += int(row["event_count"])
    vehicle_mismatches: list[dict[str, Any]] = []
    max_vehicle_ratio_diff = Decimal("0")
    for vehicle in sorted(per_vehicle):
        item = per_vehicle[vehicle]
        push_ratio = Decimal(
            energy_per_100km(item["push_mileage_u"], item["push_energy_u"])
        )
        replay_ratio = Decimal(
            energy_per_100km(item["replay_mileage_u"], item["replay_energy_u"])
        )
        diff = abs(push_ratio - replay_ratio)
        max_vehicle_ratio_diff = max(max_vehicle_ratio_diff, diff)
        if (
            item["push_mileage_u"] != item["replay_mileage_u"]
            or item["push_energy_u"] != item["replay_energy_u"]
            or item["push_event_count"] != item["replay_event_count"]
            or diff > Decimal("0.000000")
        ):
            vehicle_mismatches.append(
                {
                    "vehicle_id": vehicle,
                    "push_mileage_u": item["push_mileage_u"],
                    "replay_mileage_u": item["replay_mileage_u"],
                    "push_energy_u": item["push_energy_u"],
                    "replay_energy_u": item["replay_energy_u"],
                    "push_event_count": item["push_event_count"],
                    "replay_event_count": item["replay_event_count"],
                    "energy_per_100km_diff": str(diff),
                }
            )

    push_late = _issue_signature(store, push_run_id, "late_event")
    replay_late = _issue_signature(store, replay_run_id, "late_event")
    push_dedup = _issue_signature(store, push_run_id, "duplicate_supplement")
    replay_dedup = _issue_signature(store, replay_run_id, "duplicate_supplement")
    push_orphan = _issue_signature(store, push_run_id, "orphan_supplement")
    replay_orphan = _issue_signature(store, replay_run_id, "orphan_supplement")
    late_mismatches = _multiset_difference(push_late, replay_late)
    dedup_mismatches = _multiset_difference(push_dedup, replay_dedup)
    orphan_mismatches = _multiset_difference(push_orphan, replay_orphan)
    push_slot_max = _max_slot_share(store, push_run_id)
    replay_slot_max = _max_slot_share(store, replay_run_id)
    equivalent = (
        not mismatches
        and not vehicle_mismatches
        and not late_mismatches
        and not dedup_mismatches
        and not orphan_mismatches
        and push_slot_max <= 0.4
        and replay_slot_max <= 0.4
    )
    result = {
        "equivalent": equivalent,
        "pushdown_run_id": push_run_id,
        "replay_run_id": replay_run_id,
        "window_rows": len(all_keys),
        "vehicle_rows": len(per_vehicle),
        "mismatched_windows": len(mismatches),
        "mismatched_vehicles": len(vehicle_mismatches),
        "max_mileage_diff_micro_units": max_mileage_diff,
        "max_energy_diff_micro_units": max_energy_diff,
        "max_event_count_diff": max_event_count_diff,
        "max_window_energy_per_100km_diff": str(max_ratio_diff),
        "max_vehicle_energy_per_100km_diff": str(max_vehicle_ratio_diff),
        "tolerance": "0.01",
        "window_mismatch_details": sorted(
            mismatches,
            key=lambda item: (item["vehicle_id"], item["window_start"], item.get("reason", "")),
        ),
        "vehicle_mismatch_details": sorted(
            vehicle_mismatches, key=lambda item: item["vehicle_id"]
        ),
        "late": {
            "pushdown_count": sum(push_late.values()),
            "replay_count": sum(replay_late.values()),
            "counts_match": push_late == replay_late,
            "difference_details": late_mismatches,
        },
        "dedup": {
            "pushdown_count": sum(push_dedup.values()),
            "replay_count": sum(replay_dedup.values()),
            "counts_match": push_dedup == replay_dedup,
            "difference_details": dedup_mismatches,
        },
        "orphan_supplements": {
            "pushdown_count": sum(push_orphan.values()),
            "replay_count": sum(replay_orphan.values()),
            "counts_match": push_orphan == replay_orphan,
            "difference_details": orphan_mismatches,
        },
        "slots": {
            "pushdown_max_share": push_slot_max,
            "replay_max_share": replay_slot_max,
            "bound": 0.4,
            "within_bound": push_slot_max <= 0.4 and replay_slot_max <= 0.4,
        },
    }
    now = _utc_now()
    encoded = canonical_json(result)
    with store.tx() as conn:
        conn.execute(
            "INSERT OR REPLACE INTO verification_results(run_id,comparison,result_json,created_at) "
            "VALUES (?,?,?,?)",
            (push_run_id, f"against:{replay_run_id}", encoded, now),
        )
        conn.execute(
            "INSERT OR REPLACE INTO verification_results(run_id,comparison,result_json,created_at) "
            "VALUES (?,?,?,?)",
            (replay_run_id, f"against:{push_run_id}", encoded, now),
        )
    return result


def _issue_signature(store: StateStore, run_id: str, issue_type: str) -> Counter[tuple]:
    rows = store.fetchall(
        "SELECT vehicle_id,trip_id,window_start_us,event_time_us,batch_id,source_id "
        "FROM event_issues WHERE run_id=? AND issue_type=?",
        (run_id, issue_type),
    )
    return Counter(
        (
            row["vehicle_id"],
            row["trip_id"],
            int(row["window_start_us"]),
            int(row["event_time_us"]),
            row["batch_id"],
            row["source_id"],
        )
        for row in rows
    )


def _multiset_difference(left: Counter, right: Counter) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for key in sorted(set(left) | set(right)):
        left_count = int(left.get(key, 0))
        right_count = int(right.get(key, 0))
        if left_count != right_count:
            result.append(
                {
                    "vehicle_id": key[0],
                    "trip_id": key[1],
                    "window_start": iso_us(key[2]),
                    "event_time": iso_us(key[3]),
                    "batch_id": key[4],
                    "source_id": key[5],
                    "pushdown_count": left_count,
                    "replay_count": right_count,
                }
            )
    return result


def _max_slot_share(store: StateStore, run_id: str) -> float:
    rows = store.fetchall(
        "SELECT event_count FROM slot_stats WHERE run_id=?", (run_id,)
    )
    total = sum(int(row["event_count"]) for row in rows)
    if total <= 0:
        return 0.0
    return round(max(int(row["event_count"]) for row in rows) / total, 9)

