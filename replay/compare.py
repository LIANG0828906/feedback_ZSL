"""Dual numeric-path deviation report and determinism checks."""

from __future__ import annotations

import hashlib
from decimal import Decimal
from typing import Any

from .engine import RunResult, run_recording
from .model import Recording, decimal_string, float_string
from .numeric import VelocityCache


def _as_decimal(value: Any) -> Decimal:
    if isinstance(value, Decimal):
        return value
    if isinstance(value, float):
        return Decimal(float_string(value))
    return Decimal(str(value))


def compare_paths(
    recording: Recording,
    *,
    fixed_run: RunResult | None = None,
    float_run: RunResult | None = None,
) -> dict[str, Any]:
    fixed = fixed_run or run_recording(recording, "fixed", cache=VelocityCache())
    floating = float_run or run_recording(recording, "float", cache=VelocityCache())
    tolerances = recording.config.tolerances
    deviations: list[dict[str, Any]] = []
    exceeded_steps: set[int] = set()
    hash_mismatch_steps: list[int] = []

    fixed_by_step = {step.step: step for step in fixed.steps}
    float_by_step = {step.step: step for step in floating.steps}
    for step in sorted(set(fixed_by_step) | set(float_by_step)):
        left = fixed_by_step.get(step)
        right = float_by_step.get(step)
        if left is None or right is None:
            deviations.append(
                {
                    "step": step,
                    "entity": "-",
                    "field": "step",
                    "fixed": left is not None,
                    "float": right is not None,
                    "abs_diff": "unbounded",
                    "tolerance": "0",
                    "exceeds": True,
                }
            )
            exceeded_steps.add(step)
            continue
        if left.state_hash != right.state_hash:
            hash_mismatch_steps.append(step)

        _append_deviation(
            deviations,
            exceeded_steps,
            step=step,
            entity="-",
            field="resource",
            left=left.room.resource,
            right=right.room.resource,
            field_path="resource",
            tolerances=tolerances,
        )
        for entity_id in sorted(set(left.entities) | set(right.entities)):
            left_entity = left.entities.get(entity_id)
            right_entity = right.entities.get(entity_id)
            if left_entity is None or right_entity is None:
                deviations.append(
                    {
                        "step": step,
                        "entity": entity_id,
                        "field": "entity",
                        "fixed": left_entity is not None,
                        "float": right_entity is not None,
                        "abs_diff": "unbounded",
                        "tolerance": "0",
                        "exceeds": True,
                    }
                )
                exceeded_steps.add(step)
                continue
            if left_entity.alive != right_entity.alive:
                deviations.append(
                    {
                        "step": step,
                        "entity": entity_id,
                        "field": "alive",
                        "fixed": left_entity.alive,
                        "float": right_entity.alive,
                        "abs_diff": "1",
                        "tolerance": "0",
                        "exceeds": True,
                    }
                )
                exceeded_steps.add(step)
            for field, left_value, right_value, tolerance_key in (
                ("position.x", left_entity.x, right_entity.x, "position"),
                ("position.y", left_entity.y, right_entity.y, "position"),
                ("velocity.x", left_entity.vx, right_entity.vx, "velocity"),
                ("velocity.y", left_entity.vy, right_entity.vy, "velocity"),
                ("heading", left_entity.heading, right_entity.heading, "heading"),
            ):
                _append_deviation(
                    deviations,
                    exceeded_steps,
                    step=step,
                    entity=entity_id,
                    field=field,
                    left=left_value,
                    right=right_value,
                    field_path=tolerance_key,
                    tolerances=tolerances,
                )

    rerun_fixed = run_recording(recording, "fixed", cache=VelocityCache())
    rerun_float = run_recording(recording, "float", cache=VelocityCache())
    semantics = hashlib.sha256(
        repr(recording.canonical_operations()).encode("ascii")
        + b"\n"
        + repr(recording.config.digest_payload()).encode("ascii")
    ).hexdigest()
    return {
        "ok": True,
        "match_id": recording.match_id,
        "source_version": recording.source_version,
        "state_model_version": recording.state_model_version,
        "derived_fields": recording.derived_fields,
        "input_semantics_sha256": semantics,
        "fixed_trajectory_sha256": fixed.trajectory_sha256(),
        "float_trajectory_sha256": floating.trajectory_sha256(),
        "deterministic_fixed": (
            fixed.trajectory_sha256() == rerun_fixed.trajectory_sha256()
            and fixed.final_state_hash == rerun_fixed.final_state_hash
        ),
        "deterministic_float": (
            floating.trajectory_sha256() == rerun_float.trajectory_sha256()
            and floating.final_state_hash == rerun_float.final_state_hash
        ),
        "fixed_final_state_hash": fixed.final_state_hash,
        "float_final_state_hash": floating.final_state_hash,
        "hash_mismatch_steps": hash_mismatch_steps,
        "tolerance_exceeded_steps": sorted(exceeded_steps),
        "first_divergence": deviations[0] if deviations else None,
        "deviations": deviations,
        "untrusted_steps": sorted(
            {entry["step"] for step in fixed.steps for entry in step.untrusted}
        ),
        "fixed_cache_stats": fixed.cache_stats,
        "float_cache_stats": floating.cache_stats,
    }


def _append_deviation(
    deviations: list[dict[str, Any]],
    exceeded_steps: set[int],
    *,
    step: int,
    entity: str,
    field: str,
    left: Any,
    right: Any,
    field_path: str,
    tolerances: dict[str, Decimal],
) -> None:
    left_decimal = _as_decimal(left)
    right_decimal = _as_decimal(right)
    difference = abs(left_decimal - right_decimal)
    if difference == 0:
        return
    tolerance = tolerances[field_path]
    exceeds = difference > tolerance
    if exceeds:
        exceeded_steps.add(step)
    deviations.append(
        {
            "step": step,
            "entity": entity,
            "field": field,
            "fixed": decimal_string(left_decimal),
            "float": decimal_string(right_decimal),
            "abs_diff": decimal_string(difference),
            "tolerance": decimal_string(tolerance),
            "exceeds": exceeds,
        }
    )
