"""State-model version upgrade/downgrade helpers."""

from __future__ import annotations

import copy
from collections import OrderedDict
from typing import Any

from .engine import RunResult
from .errors import E_INPUT_INVALID, E_VERSION_LOSS, fail
from .model import STATE_MODEL_VERSION


def downgrade_state(
    state: dict[str, Any],
    target_version: str = "v1",
) -> dict[str, Any]:
    """Return a downgraded state document or raise E_VERSION_LOSS.

    v1 has no alive field.  ``alive == true`` is representable via the declared
    v1 default, while ``alive == false`` cannot be expressed and is a loss.
    """

    if target_version not in ("v1", "v2"):
        fail(
            E_INPUT_INVALID,
            "target state version must be v1 or v2",
            "target_version",
        )
    payload = copy.deepcopy(state)
    if target_version == "v2":
        return payload
    step = int(payload.get("step", 0))
    losses: list[str] = []
    entities = payload.get("entities", [])
    if isinstance(entities, list):
        for item in entities:
            if not isinstance(item, dict):
                continue
            alive = item.pop("alive", True)
            if alive is False:
                losses.append(f"entities.{item.get('id', '?')}.alive")
    payload["state_model_version"] = STATE_MODEL_VERSION.replace("v2", "v1")
    # Rebuild the top-level order so a downgraded document obeys the protocol
    # field order rather than relying on the input dict's insertion order.
    ordered: OrderedDict[str, Any] = OrderedDict()
    ordered["state_model_version"] = "state-v1"
    ordered["step"] = payload.get("step", step)
    ordered["room"] = payload.get("room", {})
    ordered["entities"] = payload.get("entities", [])
    if losses:
        fail(
            E_VERSION_LOSS,
            "state contains fields that v1 cannot express",
            losses[0],
            step=step,
            details={
                "target_version": "v1",
                "affected_steps": [{"step": step, "fields": losses}],
            },
        )
    return ordered


def downgrade_run(run: RunResult, target_version: str = "v1") -> dict[str, Any]:
    if target_version not in ("v1", "v2"):
        fail(
            E_INPUT_INVALID,
            "target state version must be v1 or v2",
            "target_version",
        )
    if target_version == "v2":
        frames = [
            {
                "step": step.step,
                "state": step.state,
                "state_hash": step.state_hash,
            }
            for step in run.steps
        ]
        return {
            "export_version": "export-v1",
            "state_model_version": STATE_MODEL_VERSION,
            "match_id": run.recording.match_id,
            "frames": frames,
        }

    affected: list[dict[str, Any]] = []
    frames: list[dict[str, Any]] = []
    for step in run.steps:
        fields: list[str] = []
        state = copy.deepcopy(step.state)
        for item in state.get("entities", []):
            alive = item.pop("alive", True)
            if alive is False:
                fields.append(f"entities.{item.get('id', '?')}.alive")
        state["state_model_version"] = "state-v1"
        frames.append({"step": step.step, "state": state})
        if fields:
            affected.append({"step": step.step, "fields": fields})
    if affected:
        fail(
            E_VERSION_LOSS,
            "v1 export cannot represent one or more state fields",
            f"steps.{affected[0]['step']}.{affected[0]['fields'][0]}",
            step=affected[0]["step"],
            details={
                "target_version": "v1",
                "affected_steps": affected,
                "affected_step_numbers": [item["step"] for item in affected],
                "affected_fields": sorted(
                    {field for item in affected for field in item["fields"]}
                ),
            },
        )
    return {
        "export_version": "export-v1",
        "state_model_version": "state-v1",
        "match_id": run.recording.match_id,
        "frames": frames,
    }
