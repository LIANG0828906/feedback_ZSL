"""Baseline comparison and stable review-report construction."""

from __future__ import annotations

from collections import Counter
from collections.abc import Iterable, Mapping
from typing import Any

from .model import InitialState
from .simulation import Simulation

FATAL_CODES = {
    "E_DAMAGE_IMBALANCE",
    "E_RECOVERY_INVALID",
    "E_BASELINE_MISMATCH",
    "E_BASELINE_HASH_MISMATCH",
    "E_PROGRESS_INJECTED",
    "E_RECOMPUTE_INJECTED",
    "E_PROGRESS_INVALID",
}


def compare_baseline(
    baseline: Mapping[str, Any],
    summaries: Iterable[Mapping[str, Any]],
    *,
    recompute_fault_step: int | None = None,
) -> list[dict[str, Any]]:
    expected_steps = {
        int(item["step"]): item for item in baseline.get("steps", [])
    }
    errors: list[dict[str, Any]] = []
    for summary in summaries:
        step = int(summary["step"])
        if recompute_fault_step == step:
            errors.append(
                {
                    "code": "E_RECOMPUTE_INJECTED",
                    "step": step,
                    "message": "baseline recomputation fault injected by input parameter",
                }
            )
            continue
        expected = expected_steps.get(step)
        if expected is None:
            errors.append(
                {
                    "code": "E_BASELINE_MISMATCH",
                    "step": step,
                    "field": "step",
                    "expected": "present",
                    "actual": "missing from baseline",
                }
            )
            continue
        expected_by_id = {
            item["cover_id"]: item for item in expected.get("covers", [])
        }
        actual_by_id = {
            item["cover_id"]: item for item in summary.get("covers", [])
        }
        for cover_id in sorted(set(expected_by_id) | set(actual_by_id)):
            expected_cover = expected_by_id.get(cover_id)
            actual_cover = actual_by_id.get(cover_id)
            if expected_cover is None or actual_cover is None:
                errors.append(
                    {
                        "code": "E_BASELINE_MISMATCH",
                        "step": step,
                        "cover_id": cover_id,
                        "field": "cover",
                        "expected": expected_cover,
                        "actual": actual_cover,
                    }
                )
                continue
            for field in ("remaining", "cracked", "alive", "crack_events"):
                if expected_cover.get(field) != actual_cover.get(field):
                    errors.append(
                        {
                            "code": "E_BASELINE_MISMATCH",
                            "step": step,
                            "cover_id": cover_id,
                            "field": field,
                            "expected": expected_cover.get(field),
                            "actual": actual_cover.get(field),
                        }
                    )
        if str(expected.get("state_hash")) != str(summary.get("state_hash")):
            errors.append(
                {
                    "code": "E_BASELINE_HASH_MISMATCH",
                    "step": step,
                    "expected": expected.get("state_hash"),
                    "actual": summary.get("state_hash"),
                }
            )
    return errors


def build_report(
    *,
    initial: InitialState,
    shots_payload: Mapping[str, Any],
    simulation: Simulation,
    summaries: list[Mapping[str, Any]],
    baseline: Mapping[str, Any],
    baseline_errors: list[dict[str, Any]],
    restored_from_step: int | None = None,
    sample_manifest: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    operational = list(simulation.state.operational_reports)
    all_errors = operational + baseline_errors
    counts = Counter(str(item.get("code", "UNKNOWN")) for item in all_errors)
    final_step = summaries[-1]["step"] if summaries else simulation.state.step
    final_covers = {
        cover_id: simulation.state.covers[cover_id].state_dict()
        for cover_id in initial.cover_ids
    }
    total_remaining = sum(
        cover["remaining"] for cover in final_covers.values()
    )
    total_damage = sum(cover["damage"] for cover in final_covers.values())
    conservation = {
        "initial_total": initial.total_durability,
        "remaining_total": total_remaining,
        "applied_damage_total": total_damage,
        "balanced": total_remaining + total_damage == initial.total_durability,
    }
    nonfatal_codes = {
        "E_OP_LATE",
        "E_OP_DUPLICATE",
        "E_REVOKE_CLOSED",
        "E_REVOKE_NOT_FOUND",
        "E_DAMAGE_CONFLICT",
        "E_NO_HIT",
        "E_DAMAGE_REFUNDED",
    }
    fatal = sorted(
        {
            str(item.get("code"))
            for item in all_errors
            if str(item.get("code")) in FATAL_CODES
        }
    )
    if not conservation["balanced"]:
        fatal.append("E_DAMAGE_IMBALANCE")
    return {
        "schema_version": 1,
        "match_id": initial.match_id,
        "sample_seed": shots_payload.get("seed"),
        "sample_manifest": dict(sample_manifest or {}),
        "window": {
            "start_step": initial.start_step,
            "end_step": initial.end_step,
            "step_ms": initial.step_ms,
            "sample_rule": initial.sample_rule,
            "truth_source": initial.truth_source,
        },
        "restored_from_step": restored_from_step,
        "published_steps": [int(item["step"]) for item in summaries],
        "final_step": final_step,
        "initial_hash": initial.initial_hash,
        "final_hash": (
            simulation.state.hash_chain[-1]["state_hash"]
            if simulation.state.hash_chain
            else None
        ),
        "hash_chain": list(simulation.state.hash_chain),
        "baseline_final_hash": baseline.get("final_hash"),
        "covers": final_covers,
        "conservation": conservation,
        "business_code_counts": dict(sorted(counts.items())),
        "fatal_codes": sorted(set(fatal)),
        "fatal_error": bool(fatal),
        "expected_codes_observed": sorted(
            code for code in counts if code in nonfatal_codes
        ),
        "conflicts": [
            item for item in operational if item.get("code") == "E_DAMAGE_CONFLICT"
        ],
        "late_shots": [
            item for item in operational if item.get("code") == "E_OP_LATE"
        ],
        "duplicates": [
            item for item in operational if item.get("code") == "E_OP_DUPLICATE"
        ],
        "revocations": [
            item
            for item in simulation.state.revocations
        ],
        "baseline_differences": baseline_errors,
        "operational_reports": operational,
        "duplicate_count": simulation.state.duplicate_count,
        "snapshot_step_count": len(summaries),
    }

