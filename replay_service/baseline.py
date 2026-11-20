"""基线生成与配额偏差/分叉归因报告。"""

from __future__ import annotations

from dataclasses import dataclass

from .errors import E_BASELINE_VERSION, ReplayError
from .models import (
    BASELINE_VERSION,
    RULE_VERSION,
    ExerciseConfig,
    StepResult,
    canonical_dumps,
)

_COMPARE_FIELDS = ("cumulative_used", "cumulative_penalty")


def build_baseline(config: ExerciseConfig, results: list[StepResult]) -> dict:
    return {
        "baseline_version": BASELINE_VERSION,
        "rule_version": RULE_VERSION,
        "exercise_id": config.exercise_id,
        "total_steps": config.total_steps,
        "steps": [_baseline_step(result) for result in results],
    }


def _baseline_step(result: StepResult) -> dict:
    return {
        "step": result.step,
        "cumulative_used": dict(result.cumulative_used),
        "cumulative_penalty": dict(result.cumulative_penalty),
        "penalties": [p.to_dict() for p in result.penalties],
        "op_client_seqs": list(result.op_client_seqs),
        "step_hash": result.step_hash,
    }


@dataclass
class Divergence:
    step: int
    player_id: str
    field: str
    baseline_value: float
    actual_value: float
    op_client_seqs: list

    def to_dict(self) -> dict:
        return {
            "step": self.step,
            "player_id": self.player_id,
            "field": self.field,
            "baseline_value": self.baseline_value,
            "actual_value": self.actual_value,
            "op_client_seqs": list(self.op_client_seqs),
        }


def compare_with_baseline(
    baseline: dict,
    config: ExerciseConfig,
    results: list[StepResult],
) -> Divergence | None:
    """逐条比对，返回首个不一致；完全一致返回 None。

    基线/规则版本不匹配时直接 E_BASELINE_VERSION，且不给分叉结论。
    """
    if baseline.get("baseline_version") != BASELINE_VERSION:
        raise ReplayError(
            E_BASELINE_VERSION,
            f"基线版本不符: 期望 {BASELINE_VERSION}，"
            f"实际 {baseline.get('baseline_version')!r}",
        )
    if baseline.get("rule_version") != RULE_VERSION:
        raise ReplayError(
            E_BASELINE_VERSION,
            f"规则版本不符: 期望 {RULE_VERSION}，实际 {baseline.get('rule_version')!r}",
        )
    if baseline.get("exercise_id") != config.exercise_id:
        raise ReplayError(
            E_BASELINE_VERSION,
            "基线 exercise_id 与当前演练不一致",
        )

    base_steps = {entry["step"]: entry for entry in baseline["steps"]}
    for result in results:
        entry = base_steps.get(result.step)
        if entry is None:
            return Divergence(
                step=result.step,
                player_id="*",
                field="step_presence",
                baseline_value=None,
                actual_value=result.step_hash,
                op_client_seqs=list(result.op_client_seqs),
            )
        for field_name in _COMPARE_FIELDS:
            actual_map = getattr(result, field_name)
            baseline_map = entry[field_name]
            for player_id in config.player_ids:
                if baseline_map.get(player_id) != actual_map.get(player_id):
                    return Divergence(
                        step=result.step,
                        player_id=player_id,
                        field=field_name,
                        baseline_value=baseline_map.get(player_id),
                        actual_value=actual_map.get(player_id),
                        op_client_seqs=list(result.op_client_seqs),
                    )
        if entry["step_hash"] != result.step_hash:
            return Divergence(
                step=result.step,
                player_id="*",
                field="step_hash",
                baseline_value=entry["step_hash"],
                actual_value=result.step_hash,
                op_client_seqs=list(result.op_client_seqs),
            )
    return None


def write_baseline(path: str, baseline: dict) -> None:
    import os

    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(canonical_dumps(baseline) + "\n")


def read_baseline(path: str) -> dict:
    import json

    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)
