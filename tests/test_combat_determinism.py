from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

from balance_lab.budget import SimulationRequest
from balance_lab.combat import CandidateRuntime, simulate_match
from balance_lab.compare import compare_results
from balance_lab.scheduler import run_validation


def test_match_is_pure_for_seed_and_round(alpha) -> None:
    project, candidates = alpha
    runtime = CandidateRuntime.from_candidate(project, candidates.candidates[0])
    scenario = project.scenarios[0]
    first = simulate_match(project, runtime, scenario, sample_index=17)
    second = simulate_match(project, runtime, scenario, sample_index=17)
    assert first == second
    assert all(action.damage >= 0 for round_result in first.rounds for action in round_result.actions)
    assert all(
        round_result.damage_a_to_b
        == sum(
            action.damage
            for action in round_result.actions
            if action.actor_id.startswith("A:")
        )
        for round_result in first.rounds
    )
    assert all(
        round_result.damage_b_to_a
        == sum(
            action.damage
            for action in round_result.actions
            if action.actor_id.startswith("B:")
        )
        for round_result in first.rounds
    )


def test_sample_order_and_worker_thread_do_not_change_results(alpha) -> None:
    project, candidates = alpha
    runtime = CandidateRuntime.from_candidate(project, candidates.candidates[2])
    scenario = project.scenarios[4]
    indexes = [5, 1, 9, 0, 7, 3]
    expected = {
        index: simulate_match(project, runtime, scenario, sample_index=index)
        for index in indexes
    }

    def run(index: int):
        return index, simulate_match(project, runtime, scenario, sample_index=index)

    with ThreadPoolExecutor(max_workers=4) as executor:
        shuffled = list(executor.map(run, reversed(indexes)))
    for index, match in shuffled:
        assert match == expected[index]


def test_serial_and_parallel_runs_are_identical(alpha) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=10, min_samples=2, total_sample_budget=360)
    serial = run_validation(
        project,
        candidates,
        request,
        mode="full",
        parallel_slots_override=1,
    )
    parallel = run_validation(
        project,
        candidates,
        request,
        mode="full",
        parallel_slots_override=project.validation_budget.parallel_slots,
    )
    comparison = compare_results(serial, parallel, left_label="serial", right_label="parallel")
    assert comparison.equal
    assert serial.schedule.actual_peak_slots == 1
    assert parallel.schedule.actual_peak_slots <= project.validation_budget.parallel_slots
    assert [item.candidate_id for item in serial.candidates] == [
        item.candidate_id for item in parallel.candidates
    ]
    assert [(item.candidate_id, item.scenario_id) for item in serial.combos] == [
        (item.candidate_id, item.scenario_id) for item in parallel.combos
    ]

