"""Authoritative baseline creation and first-divergence diagnosis."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .config import ReplayConfig
from .engine import ReplayEngine, ReplayRun
from .errors import (
    BaselineDivergenceError,
    BaselineVersionError,
    StrategyDivergenceError,
)
from .model import ReplayRecord
from .util import atomic_write_json, first_difference, load_json, sha256_obj

BASELINE_SCHEMA_VERSION = "cover-replay-baseline-v1"


def build_baseline(run: ReplayRun) -> dict[str, Any]:
    return {
        "schema_version": BASELINE_SCHEMA_VERSION,
        "baseline_version": run.baseline_version,
        "model_version": run.model_version,
        "match_id": run.match_id,
        "physics_steps": run.physics_steps,
        "fractional_digits": run.fractional_digits,
        "strategy": run.strategy,
        "frames": list(run.frames),
        "final_state_hash": run.final_state_hash,
        "baseline_digest": sha256_obj(list(run.frames)),
    }


def write_baseline(path: str | Path, run: ReplayRun) -> dict[str, Any]:
    baseline = build_baseline(run)
    atomic_write_json(path, baseline)
    return baseline


def _version_error(
    run: ReplayRun,
    baseline_version: Any,
    model_version: Any,
) -> BaselineVersionError:
    return BaselineVersionError(
        baseline_version=(
            baseline_version if isinstance(baseline_version, str) else None
        ),
        expected_baseline_version=run.baseline_version,
        model_version=model_version if isinstance(model_version, str) else None,
        expected_model_version=run.model_version,
    )


def _attribution(field_path: str) -> str:
    if "scheduling" in field_path or "budget_usage" in field_path:
        return "budget_or_worker_scheduling"
    if "credit" in field_path:
        return "prediction_or_withdrawal_reconciliation"
    if "crack" in field_path:
        return "crack_propagation"
    if "covers" in field_path or "state_hash" in field_path:
        return "fixed_point_damage_or_reduction_order"
    return "frame_metadata_or_settlement_order"


def compare_baseline(
    run: ReplayRun,
    baseline: dict[str, Any],
) -> dict[str, Any]:
    baseline_version = baseline.get("baseline_version")
    model_version = baseline.get("model_version")
    if (
        baseline_version != run.baseline_version
        or model_version != run.model_version
    ):
        raise _version_error(run, baseline_version, model_version)
    if baseline.get("schema_version") != BASELINE_SCHEMA_VERSION:
        raise _version_error(run, baseline_version, model_version)
    baseline_frames = baseline.get("frames")
    if not isinstance(baseline_frames, list):
        raise BaselineDivergenceError(
            step=1,
            field_path="frames",
            baseline_value=baseline_frames,
            replay_value="<expected-array>",
            effective_client_sequences=[],
            queue_watermark=None,
            memory_budget=None,
        )
    for index in range(max(len(baseline_frames), len(run.frames))):
        replay_frame = run.frames[index] if index < len(run.frames) else None
        baseline_frame = (
            baseline_frames[index] if index < len(baseline_frames) else None
        )
        step = (
            replay_frame.get("step")
            if isinstance(replay_frame, dict)
            else (index + 1)
        )
        difference = first_difference(
            baseline_frame,
            replay_frame,
            path=f"frames[{index}]",
        )
        if difference is None:
            continue
        field_path, baseline_value, replay_value = difference
        frame = replay_frame if isinstance(replay_frame, dict) else {}
        budget = (
            frame.get("budget_usage", {})
            if isinstance(frame.get("budget_usage"), dict)
            else {}
        )
        raise BaselineDivergenceError(
            step=int(step),
            field_path=field_path,
            baseline_value=baseline_value,
            replay_value=replay_value,
            effective_client_sequences=list(
                frame.get("effective_client_sequences", [])
            ),
            queue_watermark=budget.get("queue_watermark"),
            memory_budget=budget.get("memory_budget"),
            attribution=_attribution(field_path),
        )
    expected_final = baseline.get("final_state_hash")
    if expected_final != run.final_state_hash:
        raise BaselineDivergenceError(
            step=len(run.frames),
            field_path="final_state_hash",
            baseline_value=expected_final,
            replay_value=run.final_state_hash,
            effective_client_sequences=(
                list(run.frames[-1].get("effective_client_sequences", []))
                if run.frames
                else []
            ),
            queue_watermark=(
                run.frames[-1].get("budget_usage", {}).get("queue_watermark")
                if run.frames
                else None
            ),
            memory_budget=(
                run.frames[-1].get("budget_usage", {}).get("memory_budget")
                if run.frames
                else None
            ),
            attribution="final_state_hash",
        )
    return {
        "ok": True,
        "steps_compared": len(run.frames),
        "final_state_hash": run.final_state_hash,
        "baseline_digest": baseline.get("baseline_digest"),
    }


def compare_with_baseline_file(
    run: ReplayRun, baseline_path: str | Path
) -> dict[str, Any]:
    try:
        baseline = load_json(baseline_path)
    except (OSError, ValueError) as error:
        raise BaselineDivergenceError(
            step=1,
            field_path="$",
            baseline_value=f"<unreadable: {error}>",
            replay_value="<replay>",
            effective_client_sequences=[],
            queue_watermark=None,
            memory_budget=None,
        ) from None
    return compare_baseline(run, baseline)


def compare_strategies(
    record: ReplayRecord,
    config: ReplayConfig,
    *,
    allow_conflicts: bool = False,
) -> dict[str, Any]:
    full_run = ReplayEngine(
        record,
        config.with_strategy("full"),
        allow_conflicts=allow_conflicts,
    ).run()
    window_run = ReplayEngine(
        record,
        config.with_strategy("window"),
        allow_conflicts=allow_conflicts,
    ).run()
    difference = first_difference(
        {"frames": list(full_run.frames), "final": full_run.final_state_hash},
        {"frames": list(window_run.frames), "final": window_run.final_state_hash},
        path="$",
    )
    if difference is not None:
        field_path, full_value, window_value = difference
        step = 1
        if field_path.startswith("$.frames["):
            try:
                step = int(field_path.split("[", 1)[1].split("]", 1)[0]) + 1
            except ValueError:
                step = 1
        raise StrategyDivergenceError(
            {
                "step": step,
                "field_path": field_path,
                "full_scan_value": full_value,
                "window_value": window_value,
                "attribution": _attribution(field_path),
            }
        )
    return {
        "ok": True,
        "steps_compared": len(full_run.frames),
        "final_state_hash": full_run.final_state_hash,
        "full_scan_digest": sha256_obj(list(full_run.frames)),
        "window_digest": sha256_obj(list(window_run.frames)),
    }
