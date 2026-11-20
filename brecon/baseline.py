from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .canonical import BASELINE_VERSION, MODEL_VERSION, compact_json, read_json
from .engine import ReplayEngine
from .errors import BreconError


def _find_difference(
    expected: Any,
    actual: Any,
    path: str = "",
) -> tuple[str, Any, Any] | None:
    if type(expected) is not type(actual):
        return path or "$", expected, actual
    if isinstance(expected, dict):
        for key in expected:
            if key not in actual:
                return f"{path}.{key}".lstrip("."), expected[key], "<missing>"
            difference = _find_difference(
                expected[key], actual[key], f"{path}.{key}".lstrip(".")
            )
            if difference is not None:
                return difference
        for key in actual:
            if key not in expected:
                return f"{path}.{key}".lstrip("."), "<missing>", actual[key]
        return None
    if isinstance(expected, list):
        if len(expected) != len(actual):
            return f"{path}.length", len(expected), len(actual)
        for index, (left, right) in enumerate(zip(expected, actual)):
            difference = _find_difference(
                left, right, f"{path}[{index}]"
            )
            if difference is not None:
                return difference
        return None
    if expected != actual:
        return path or "$", expected, actual
    return None


def _load_baseline(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise BreconError(
            "E_BASELINE_INVALID",
            f"baseline file not found: {path}",
            step=1,
            field_path="baseline",
        )
    try:
        payload = read_json(path)
    except (OSError, ValueError) as exc:
        raise BreconError(
            "E_BASELINE_INVALID",
            f"baseline is not valid JSON: {exc}",
            step=1,
            field_path="baseline",
        ) from exc
    if not isinstance(payload, dict):
        raise BreconError(
            "E_BASELINE_INVALID",
            "baseline root must be an object",
            step=1,
            field_path="baseline",
        )
    return payload


def validate_baseline_metadata(
    engine: ReplayEngine, baseline: dict[str, Any]
) -> None:
    if "baseline_version" not in baseline:
        raise BreconError(
            "E_BASELINE_INVALID",
            "baseline_version is missing",
            step=1,
            field_path="baseline_version",
        )
    if "model_version" not in baseline:
        raise BreconError(
            "E_BASELINE_INVALID",
            "model_version is missing",
            step=1,
            field_path="model_version",
        )
    try:
        baseline_version = int(baseline["baseline_version"])
    except (TypeError, ValueError) as exc:
        raise BreconError(
            "E_BASELINE_INVALID",
            "baseline_version must be an integer",
            step=1,
            field_path="baseline_version",
        ) from exc
    if baseline_version != BASELINE_VERSION:
        raise BreconError(
            "E_BASELINE_VERSION",
            "baseline version does not match the state model",
            step=1,
            field_path="baseline_version",
            details={
                "expected": BASELINE_VERSION,
                "actual": baseline_version,
            },
        )
    if str(baseline["model_version"]) != MODEL_VERSION:
        raise BreconError(
            "E_BASELINE_VERSION",
            "baseline model version does not match the replay engine",
            step=1,
            field_path="model_version",
            details={"expected": MODEL_VERSION, "actual": baseline["model_version"]},
        )
    if str(baseline.get("room_id")) != engine.config.room_id:
        raise BreconError(
            "E_BASELINE_VERSION",
            "baseline belongs to a different room",
            step=1,
            field_path="room_id",
            details={"expected": engine.config.room_id, "actual": baseline.get("room_id")},
        )
    if str(baseline.get("config_digest")) != engine.config.digest:
        raise BreconError(
            "E_BASELINE_VERSION",
            "baseline configuration digest does not match",
            step=1,
            field_path="config_digest",
            details={
                "expected": engine.config.digest,
                "actual": baseline.get("config_digest"),
            },
        )
    steps = baseline.get("steps")
    if not isinstance(steps, list):
        raise BreconError(
            "E_BASELINE_INVALID",
            "baseline steps are missing",
            step=1,
            field_path="steps",
        )
    expected_steps = list(
        range(engine.config.observation.start_step, engine.config.observation.end_step + 1)
    )
    if len(steps) < len(expected_steps):
        missing = expected_steps[len(steps)] if len(steps) < len(expected_steps) else None
        raise BreconError(
            "E_BASELINE_INVALID",
            "baseline is truncated",
            step=missing,
            field_path=f"steps[{len(steps)}]",
            details={"expected_steps": len(expected_steps), "actual_steps": len(steps)},
        )
    for index, expected_step in enumerate(expected_steps):
        entry = steps[index]
        try:
            entry_step = int(entry.get("step", -1)) if isinstance(entry, dict) else -1
        except (TypeError, ValueError):
            entry_step = -1
        if not isinstance(entry, dict) or entry_step != expected_step:
            raise BreconError(
                "E_BASELINE_INVALID",
                "baseline step sequence is not contiguous",
                step=expected_step,
                field_path=f"steps[{index}].step",
            )
        if "state_hash" not in entry:
            raise BreconError(
                "E_BASELINE_INVALID",
                "baseline state hash is missing",
                step=expected_step,
                field_path=f"steps[{index}].state_hash",
            )


def check_baseline(engine: ReplayEngine, baseline_path: Path | None = None) -> dict[str, Any]:
    path = baseline_path or engine.config.baseline_path
    baseline = _load_baseline(path)
    validate_baseline_metadata(engine, baseline)
    baseline_steps = baseline["steps"]

    for index, trace in enumerate(engine.trace_entries):
        expected = baseline_steps[index]
        actual_state = engine.state_history[index]["state"]
        expected_hash = str(expected["state_hash"])
        actual_hash = str(trace["state_hash"])
        base = {
            "step": int(trace["step"]),
            "effective_ops": trace["effective_ops"],
            "buffer_depth": trace["buffer_depth"],
            "buffer_before": trace["buffer_before"],
            "buffer_after": trace["buffer_after"],
        }
        if expected_hash != actual_hash:
            field_path = "state_hash"
            expected_value: Any = expected_hash
            actual_value: Any = actual_hash
            if isinstance(expected.get("state"), dict):
                difference = _find_difference(expected["state"], actual_state)
                if difference is not None:
                    field_path, expected_value, actual_value = difference
            return {
                "ok": False,
                "code": "E_STATE_DIVERGENCE",
                "message": "first baseline divergence",
                **base,
                "field_path": field_path,
                "expected": expected_value,
                "actual": actual_value,
                "expected_state_hash": expected_hash,
                "actual_state_hash": actual_hash,
            }
        if isinstance(expected.get("state"), dict):
            state_difference = _find_difference(expected["state"], actual_state)
            if state_difference is not None:
                field_path, expected_value, actual_value = state_difference
                return {
                    "ok": False,
                    "code": "E_STATE_DIVERGENCE",
                    "message": "baseline state content diverged from its hash",
                    **base,
                    "field_path": field_path,
                    "expected": expected_value,
                    "actual": actual_value,
                    "expected_state_hash": expected_hash,
                    "actual_state_hash": actual_hash,
                }
        expected_chain = str(expected.get("chain_hash"))
        if expected_chain != str(trace["chain_hash"]):
            return {
                "ok": False,
                "code": "E_STATE_DIVERGENCE",
                "message": "hash chain diverged from baseline",
                **base,
                "field_path": "chain_hash",
                "expected": expected_chain,
                "actual": trace["chain_hash"],
                "expected_state_hash": expected_hash,
                "actual_state_hash": actual_hash,
            }
        for field in (
            "effective_ops",
            "reconciliations",
            "buffer_depth",
            "buffer_before",
            "buffer_after",
        ):
            expected_value = expected.get(field)
            actual_value = trace.get(field)
            if expected_value != actual_value:
                difference = _find_difference(expected_value, actual_value, field)
                field_path, left, right = (
                    difference
                    if difference is not None
                    else (field, expected_value, actual_value)
                )
                return {
                    "ok": False,
                    "code": "E_STATE_DIVERGENCE",
                    "message": "baseline event sequence diverged",
                    **base,
                    "field_path": field_path,
                    "expected": left,
                    "actual": right,
                    "expected_state_hash": expected_hash,
                    "actual_state_hash": actual_hash,
                }

    last = engine.trace_entries[-1] if engine.trace_entries else {}
    return {
        "ok": True,
        "steps_checked": len(engine.trace_entries),
        "head_state_hash": last.get("state_hash"),
        "head_chain_hash": last.get("chain_hash"),
    }


def baseline_sha256(baseline: dict[str, Any]) -> str:
    import hashlib

    return hashlib.sha256(compact_json(baseline).encode("utf-8")).hexdigest()


def load_baseline_for_inspection(path: Path) -> dict[str, Any]:
    try:
        return _load_baseline(path)
    except json.JSONDecodeError as exc:
        raise BreconError(
            "E_BASELINE_INVALID",
            f"baseline is not valid JSON: {exc}",
            step=1,
            field_path="baseline",
        ) from exc

