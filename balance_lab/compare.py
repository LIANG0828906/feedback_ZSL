from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .errors import BalanceError, ErrorCode
from .models import ComboResult, MatchResult, RunResult


@dataclass(frozen=True)
class DiffLocation:
    candidate_id: str
    scenario_id: str
    round_index: int
    sample_index: int
    category: str
    detail: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "candidate_id": self.candidate_id,
            "scenario_id": self.scenario_id,
            "round_index": self.round_index,
            "sample_index": self.sample_index,
            "category": self.category,
            "detail": self.detail,
        }


@dataclass(frozen=True)
class ComparisonResult:
    equal: bool
    code: str
    message: str
    location: DiffLocation | None = None
    left_label: str = ""
    right_label: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "code": self.code,
            "message": self.message,
            "location": self.location.to_dict() if self.location else None,
            "left_label": self.left_label,
            "right_label": self.right_label,
        }

    def raise_if_diff(self) -> None:
        if self.equal:
            return
        location = self.location
        object_id = (
            f"{location.candidate_id}::{location.scenario_id}" if location else "run"
        )
        raise BalanceError(
            ErrorCode.DIFF,
            self.message,
            object_id,
            "rounds" if location else "result",
            detail={
                "location": location.to_dict() if location else None,
                "left": self.left_label,
                "right": self.right_label,
            },
        )


def _first_match_diff(
    left: ComboResult,
    right: ComboResult,
) -> tuple[int, int, str, str] | None:
    if left.samples != right.samples:
        return (
            1,
            min(left.samples, right.samples),
            "sample_count",
            f"{left.samples} != {right.samples}",
        )
    for sample_index, (left_match, right_match) in enumerate(zip(left.matches, right.matches)):
        diff = _match_diff(left_match, right_match)
        if diff:
            round_index, category, detail = diff
            return round_index, sample_index, category, detail
    if left.wins_a != right.wins_a or left.wins_b != right.wins_b or left.draws != right.draws:
        return 1, 0, "aggregate", "winner aggregates differ"
    if left.ci_low_bp != right.ci_low_bp or left.ci_high_bp != right.ci_high_bp:
        return 1, 0, "confidence_interval", "confidence intervals differ"
    return None


def _match_diff(left: MatchResult, right: MatchResult) -> tuple[int, str, str] | None:
    if left.winner != right.winner:
        return 1, "winner", f"{left.winner} != {right.winner}"
    max_rounds = max(len(left.rounds), len(right.rounds))
    for index in range(max_rounds):
        if index >= len(left.rounds) or index >= len(right.rounds):
            return index + 1, "round_count", f"{len(left.rounds)} != {len(right.rounds)}"
        left_round = left.rounds[index]
        right_round = right.rounds[index]
        if left_round.damage_a_to_b != right_round.damage_a_to_b:
            return (
                left_round.round_index,
                "damage_a_to_b",
                f"{left_round.damage_a_to_b} != {right_round.damage_a_to_b}",
            )
        if left_round.damage_b_to_a != right_round.damage_b_to_a:
            return (
                left_round.round_index,
                "damage_b_to_a",
                f"{left_round.damage_b_to_a} != {right_round.damage_b_to_a}",
            )
        if left_round.actions != right_round.actions:
            return left_round.round_index, "actions", "action sequence differs"
    return None


def compare_results(
    left: RunResult,
    right: RunResult,
    *,
    left_label: str = "realtime",
    right_label: str = "full",
) -> ComparisonResult:
    if left.project_id != right.project_id:
        location = DiffLocation("", "", 1, 0, "project", f"{left.project_id} != {right.project_id}")
        return ComparisonResult(
            False,
            ErrorCode.DIFF.value,
            "project identifiers differ",
            location,
            left_label,
            right_label,
        )
    if left.plan.decisions != right.plan.decisions:
        location = DiffLocation("", "", 1, 0, "budget_decisions", "budget decision sequences differ")
        return ComparisonResult(
            False,
            ErrorCode.DIFF.value,
            "budget allocation decision sequence differs",
            location,
            left_label,
            right_label,
        )
    left_map = left.combo_map()
    right_map = right.combo_map()
    ordered = [combo for combo in left.plan.combos] + [
        combo for combo in right.plan.combos if combo.key not in left_map
    ]
    for plan_combo in ordered:
        key = (plan_combo.candidate_id, plan_combo.scenario_id)
        left_combo = left_map.get(key)
        right_combo = right_map.get(key)
        if left_combo is None or right_combo is None:
            location = DiffLocation(
                plan_combo.candidate_id,
                plan_combo.scenario_id,
                1,
                0,
                "missing_combo",
                f"left={'present' if left_combo else 'missing'}, right={'present' if right_combo else 'missing'}",
            )
            return ComparisonResult(
                False,
                ErrorCode.DIFF.value,
                "one path is missing a candidate/scenario combination",
                location,
                left_label,
                right_label,
            )
        diff = _first_match_diff(left_combo, right_combo)
        if diff:
            round_index, sample_index, category, detail = diff
            location = DiffLocation(
                plan_combo.candidate_id,
                plan_combo.scenario_id,
                round_index,
                sample_index,
                category,
                detail,
            )
            return ComparisonResult(
                False,
                ErrorCode.DIFF.value,
                f"first difference at {plan_combo.candidate_id}/{plan_combo.scenario_id} "
                f"sample {sample_index} round {round_index}",
                location,
                left_label,
                right_label,
            )
    return ComparisonResult(
        True,
        "OK",
        "all candidate/scenario match sequences and budget decisions are identical",
        None,
        left_label,
        right_label,
    )


def compare_three_paths(
    realtime: RunResult,
    resumed: RunResult,
    full: RunResult,
) -> tuple[ComparisonResult, ComparisonResult]:
    first = compare_results(realtime, full, left_label="realtime", right_label="full")
    if not first.equal:
        return first, ComparisonResult(False, ErrorCode.DIFF.value, "not evaluated")
    second = compare_results(resumed, full, left_label="resumed", right_label="full")
    return first, second
