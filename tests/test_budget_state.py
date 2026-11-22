from __future__ import annotations

import pytest

from balance_lab.budget import (
    SimulationRequest,
    build_budget_plan,
    decision_hash,
    replay_decision_sequence,
)
from balance_lab.combat import ComboResult
from balance_lab.errors import BalanceError, ErrorCode
from balance_lab.state import CandidateState, ComboState, StateMachine
from balance_lab.util import wilson_interval


def test_budget_reduces_sampling_without_dropping_combos(alpha) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=64, min_samples=2, total_sample_budget=1200)
    plan = build_budget_plan(project, candidates.candidates, request)
    assert len(plan.combos) == len(candidates.candidates) * len(project.scenarios)
    assert sum(combo.samples for combo in plan.combos) <= 1200
    assert all(1 <= combo.samples <= combo.requested_samples for combo in plan.combos)
    assert any(combo.samples < combo.requested_samples for combo in plan.combos)
    assert any(combo.reduction_reason for combo in plan.combos)
    assert any(decision.action == "EXPAND_SAMPLING" for decision in plan.decisions)
    assert any(decision.action == "REDUCE_MATCHES" for decision in plan.decisions)
    assert plan.decisions[-1].reason == "RESOURCE_SLOTS_DO_NOT_DROP_COMBINATIONS"
    assert plan.per_thread_buffer_kb * project.validation_budget.parallel_slots <= project.validation_budget.memory_buffer_kb


def test_budget_decision_sequence_replays_exactly(alpha) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=48, min_samples=2, total_sample_budget=900)
    first = build_budget_plan(project, candidates.candidates, request)
    second = build_budget_plan(project, candidates.candidates, request)
    replayed = replay_decision_sequence(project, candidates.candidates, request)
    assert first.decisions == second.decisions == replayed
    assert decision_hash(first) == decision_hash(second)


def test_wilson_width_shrinks_with_more_samples() -> None:
    low_a, high_a, width_a = wilson_interval(12, 24)
    low_b, high_b, width_b = wilson_interval(120, 240)
    assert width_b < width_a
    assert high_b - low_b == width_b
    assert low_b >= low_a and high_b <= high_a


@pytest.mark.parametrize(
    "budget_request",
    [
        SimulationRequest(target_samples=0, min_samples=1),
        SimulationRequest(target_samples=10, min_samples=11),
        SimulationRequest(target_samples=20, min_samples=2, total_sample_budget=10),
    ],
)
def test_out_of_range_budget_is_e_para(alpha, budget_request: SimulationRequest) -> None:
    project, candidates = alpha
    combo_count = len(candidates.candidates) * len(project.scenarios)
    with pytest.raises(BalanceError) as caught:
        budget_request.validate(combo_count)
    assert caught.value.code == ErrorCode.PARA


def _dummy_result(candidate_id: str, scenario_id: str) -> ComboResult:
    return ComboResult(
        candidate_id=candidate_id,
        scenario_id=scenario_id,
        samples=1,
        requested_samples=1,
        reduction_reason="",
        wins_a=1,
        wins_b=0,
        draws=0,
        win_rate_bp=10000,
        ci_low_bp=10000,
        ci_high_bp=10000,
        ci_width_bp=0,
        damage_min_total=1,
        damage_max_total=1,
        damage_mean_total=1,
        round_curve_a=(1,),
        round_curve_b=(0,),
        matches=(),
    )


def test_state_machine_rejects_illegal_moves_and_freezes_results(alpha) -> None:
    project, candidates = alpha
    candidate_id = candidates.candidates[0].id
    scenario_id = project.scenarios[0].id
    key = StateMachine.combo_key(candidate_id, scenario_id)
    state = StateMachine()
    state.register([candidate_id], [key])
    state.ensure_evaluating(candidate_id)
    with pytest.raises(BalanceError) as caught:
        state.transition_combo(key, ComboState.SUBMITTED)
    assert caught.value.code == ErrorCode.STATE
    assert state.combo_state(key) == ComboState.PENDING

    state.transition_combo(key, ComboState.SIMULATING)
    result = _dummy_result(candidate_id, scenario_id)
    state.submit_combo(result)
    assert state.combo_state(key) == ComboState.SUBMITTED
    assert state.submitted_combo_count == 1
    with pytest.raises(BalanceError) as caught:
        state.submit_combo(result)
    assert caught.value.code == ErrorCode.STATE
    assert state.combo_state(key) == ComboState.SUBMITTED
    with pytest.raises(BalanceError) as caught:
        state.assert_combo_mutable(candidate_id, scenario_id)
    assert caught.value.code == ErrorCode.STATE
    assert state.get_combo_result(key).content_hash() == result.content_hash()
    state.submit_candidate(candidate_id)
    assert state.candidate_state(candidate_id) == CandidateState.SUBMITTED
    with pytest.raises(BalanceError) as caught:
        state.transition_candidate(candidate_id, CandidateState.EVALUATING)
    assert caught.value.code == ErrorCode.STATE
    assert state.candidate_state(candidate_id) == CandidateState.SUBMITTED

