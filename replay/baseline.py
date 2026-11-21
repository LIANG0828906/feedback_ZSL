"""Baseline generation, frame-by-frame comparison, and fork attribution."""

from __future__ import annotations

from typing import Any

from .engine import RunResult, StepResult
from .errors import (
    E_BASELINE_CORRUPT,
    E_BASELINE_VERSION,
    ReplayError,
    fail,
)
from .model import (
    BASELINE_VERSION,
    STATE_MODEL_VERSION,
)
from .validation import load_json_object


def generate_baseline(run: RunResult) -> dict[str, Any]:
    frames: list[dict[str, Any]] = []
    for step in run.steps:
        frames.append(
            {
                "step": step.step,
                "state_hash": step.state_hash,
                "chain_hash": step.chain_hash,
                "mode": step.mode,
                "state": step.state,
                "effective_ops": [
                    op["id"] for op in step.effective_ops
                ],
                "effective_ops_detail": step.effective_ops,
                "error_bounds": step.error_summary,
                "untrusted": step.untrusted,
            }
        )
    return {
        "baseline_version": BASELINE_VERSION,
        "state_model_version": STATE_MODEL_VERSION,
        "match_id": run.recording.match_id,
        "source_version": run.recording.source_version,
        "config_digest": run.config_digest,
        "mode": run.mode,
        "final_state_hash": run.final_state_hash,
        "frames": frames,
    }


def _baseline_state_index(frame: dict[str, Any]) -> tuple[str | None, dict[str, Any], dict[str, dict[str, Any]]]:
    state = frame.get("state")
    if not isinstance(state, dict):
        return None, {}, {}
    room = state.get("room") if isinstance(state.get("room"), dict) else {}
    entities: dict[str, dict[str, Any]] = {}
    raw_entities = state.get("entities")
    if isinstance(raw_entities, list):
        for item in raw_entities:
            if isinstance(item, dict) and "id" in item:
                entities[str(item["id"])] = item
    return state.get("state_model_version"), room, entities


def _compare_state_fields(
    local: StepResult,
    frame: dict[str, Any],
) -> list[dict[str, Any]]:
    mismatches: list[dict[str, Any]] = []
    _, baseline_room, baseline_entities = _baseline_state_index(frame)
    local_state = local.state
    local_room = local_state.get("room", {})
    baseline_resource = baseline_room.get("resource")
    local_resource = local_room.get("resource")
    if baseline_resource != local_resource:
        mismatches.append(
            {
                "field": "room.resource",
                "local": local_resource,
                "baseline": baseline_resource,
            }
        )
    local_entities = {
        str(item["id"]): item for item in local_state.get("entities", [])
    }
    for entity_id in sorted(local_entities):
        local_entity = local_entities[entity_id]
        baseline_entity = baseline_entities.get(entity_id)
        if baseline_entity is None:
            mismatches.append(
                {
                    "field": f"entities.{entity_id}",
                    "local": local_entity,
                    "baseline": None,
                }
            )
            continue
        if baseline_entity.get("alive") != local_entity.get("alive"):
            mismatches.append(
                {
                    "field": f"entities.{entity_id}.alive",
                    "local": local_entity.get("alive"),
                    "baseline": baseline_entity.get("alive"),
                }
            )
        for vector in ("position", "velocity"):
            local_vec = local_entity.get(vector, {})
            baseline_vec = baseline_entity.get(vector, {})
            for axis in ("x", "y"):
                if local_vec.get(axis) != baseline_vec.get(axis):
                    mismatches.append(
                        {
                            "field": f"entities.{entity_id}.{vector}.{axis}",
                            "local": local_vec.get(axis),
                            "baseline": baseline_vec.get(axis),
                        }
                    )
        if local_entity.get("heading") != baseline_entity.get("heading"):
            mismatches.append(
                {
                    "field": f"entities.{entity_id}.heading",
                    "local": local_entity.get("heading"),
                    "baseline": baseline_entity.get("heading"),
                }
            )
    for entity_id in sorted(set(baseline_entities) - set(local_entities)):
        mismatches.append(
            {
                "field": f"entities.{entity_id}",
                "local": None,
                "baseline": baseline_entities[entity_id],
            }
        )
    return mismatches


def compare_baseline(
    run: RunResult,
    baseline_path: str,
    *,
    inject: list[str] | None = None,
) -> dict[str, Any]:
    raw = load_json_object(baseline_path, code=E_BASELINE_CORRUPT)
    injection_context = None
    if inject:
        from .injection import apply_baseline_injection

        raw, injection_context = apply_baseline_injection(raw, inject)
    try:
        payload = _validate_baseline_header(raw, run)
        frames = payload["frames"]
        frame_by_step: dict[int, dict[str, Any]] = {}
        for index, frame in enumerate(frames):
            if not isinstance(frame, dict):
                fail(
                    E_BASELINE_CORRUPT,
                    "baseline frame must be an object",
                    f"frames[{index}]",
                )
            if "step" not in frame:
                fail(
                    E_BASELINE_CORRUPT,
                    "baseline frame is missing step",
                    f"frames[{index}].step",
                )
            try:
                step = int(frame["step"])
            except (TypeError, ValueError):
                fail(
                    E_BASELINE_CORRUPT,
                    "baseline frame step is not an integer",
                    f"frames[{index}].step",
                )
            if step in frame_by_step:
                fail(
                    E_BASELINE_CORRUPT,
                    "duplicate baseline frame step",
                    f"frames[{index}].step",
                    step=step,
                )
            if "state_hash" not in frame:
                fail(
                    E_BASELINE_CORRUPT,
                    "baseline frame is missing state_hash",
                    f"frames[{index}].state_hash",
                    step=step,
                )
            frame_by_step[step] = frame

        mismatches: list[dict[str, Any]] = []
        for local in run.steps:
            frame = frame_by_step.get(local.step)
            if frame is None:
                fail(
                    E_BASELINE_CORRUPT,
                    "baseline is missing a replay step",
                    "frames",
                    step=local.step,
                )
            field_mismatches = _compare_state_fields(local, frame)
            hash_mismatch = frame.get("state_hash") != local.state_hash
            if hash_mismatch or field_mismatches:
                effective_ops = [
                    {
                        "id": op["id"],
                        "entity_id": op["entity_id"],
                        "client_seq": op["client_seq"],
                        "arrival_seq": op["arrival_seq"],
                        "effective_step": op["effective_step"],
                    }
                    for op in local.effective_ops
                ]
                baseline_ops = frame.get("effective_ops")
                ops_available = isinstance(baseline_ops, list)
                detail = {
                    "step": local.step,
                    "field": (
                        field_mismatches[0]["field"]
                        if field_mismatches
                        else "state_hash"
                    ),
                    "local": (
                        field_mismatches[0]["local"]
                        if field_mismatches
                        else local.state_hash
                    ),
                    "baseline": (
                        field_mismatches[0]["baseline"]
                        if field_mismatches
                        else frame.get("state_hash")
                    ),
                    "local_state_hash": local.state_hash,
                    "baseline_state_hash": frame.get("state_hash"),
                    "mode": local.mode,
                    "baseline_mode": frame.get("mode"),
                    "effective_ops": effective_ops,
                    "baseline_effective_ops": baseline_ops if ops_available else None,
                    "attribution_available": ops_available,
                    "fork_causes": [],
                }
                if ops_available:
                    local_ids = [op["id"] for op in effective_ops]
                    if local_ids != baseline_ops:
                        detail["fork_causes"].append("effective_ops_mismatch")
                    if frame.get("mode") not in (None, local.mode):
                        detail["fork_causes"].append("numeric_mode_mismatch")
                    if not detail["fork_causes"]:
                        detail["fork_causes"].append("state_or_parameter_divergence")
                else:
                    detail["attribution_note"] = (
                        "baseline lacks effective ops; fork cause intentionally not inferred"
                    )
                mismatches.append(detail)

        return {
            "ok": not mismatches,
            "match_id": run.recording.match_id,
            "mode": run.mode,
            "baseline_version": payload["baseline_version"],
            "state_model_version": payload["state_model_version"],
            "steps_compared": len(run.steps),
            "first_mismatch": mismatches[0] if mismatches else None,
            "mismatches": mismatches,
            "injection": injection_context,
        }
    except ReplayError as exc:
        raise exc.with_injection(injection_context) from None


def _validate_baseline_header(raw: Any, run: RunResult) -> dict[str, Any]:
    if not isinstance(raw, dict):
        fail(E_BASELINE_CORRUPT, "baseline must be a JSON object", "baseline")
    if "baseline_version" not in raw:
        fail(E_BASELINE_CORRUPT, "baseline_version is missing", "baseline_version")
    if "state_model_version" not in raw:
        fail(
            E_BASELINE_CORRUPT,
            "state_model_version is missing",
            "state_model_version",
        )
    if raw["baseline_version"] != BASELINE_VERSION:
        fail(
            E_BASELINE_VERSION,
            "baseline version is not supported",
            "baseline_version",
            details={
                "expected": BASELINE_VERSION,
                "found": raw["baseline_version"],
            },
        )
    if raw["state_model_version"] != STATE_MODEL_VERSION:
        fail(
            E_BASELINE_VERSION,
            "baseline state model version does not match replay model",
            "state_model_version",
            details={
                "expected": STATE_MODEL_VERSION,
                "found": raw["state_model_version"],
            },
        )
    if raw.get("match_id") not in (None, run.recording.match_id):
        fail(
            E_BASELINE_CORRUPT,
            "baseline match_id does not match recording",
            "match_id",
        )
    if raw.get("config_digest") not in (None, run.config_digest):
        fail(
            E_BASELINE_VERSION,
            "baseline configuration digest does not match recording",
            "config_digest",
            details={
                "expected": run.config_digest,
                "found": raw.get("config_digest"),
            },
        )
    frames = raw.get("frames")
    if not isinstance(frames, list):
        fail(E_BASELINE_CORRUPT, "frames must be an array", "frames")
    return raw
