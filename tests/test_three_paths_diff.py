from __future__ import annotations

from dataclasses import replace

from balance_lab.budget import SimulationRequest
from balance_lab.compare import compare_results, compare_three_paths
from balance_lab.errors import BalanceError, ErrorCode
from balance_lab.models import ComboResult
from balance_lab.scheduler import run_validation


def test_three_paths_are_identical(alpha, tmp_path) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=8, min_samples=2, total_sample_budget=300)
    realtime_cp = tmp_path / "realtime.sqlite3"
    resume_cp = tmp_path / "resume.sqlite3"
    realtime = run_validation(
        project,
        candidates,
        request,
        checkpoint_path=realtime_cp,
        mode="realtime",
    )
    partial = run_validation(
        project,
        candidates,
        replace(request, interrupt_after_commits=8),
        checkpoint_path=resume_cp,
        mode="resume",
    )
    resumed = run_validation(
        project,
        candidates,
        request,
        checkpoint_path=resume_cp,
        mode="resume",
    )
    full = run_validation(project, candidates, request, mode="full")
    assert partial.interrupted
    assert resumed.schedule.restored_combos == partial.schedule.committed_combos
    first, second = compare_three_paths(realtime, resumed, full)
    assert first.equal and second.equal
    assert realtime.schedule.committed_combos == full.schedule.committed_combos


def test_diff_locates_first_round_and_returns_e_diff(alpha) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=3, min_samples=1, total_sample_budget=200)
    reference = run_validation(project, candidates, request, mode="full")
    original = reference.combos[0]
    payload = original.to_dict()
    payload.pop("content_hash", None)
    target_round = payload["matches"][0]["rounds"][2]
    target_round["damage_a_to_b"] += 7
    mutated_combo = ComboResult.from_dict(payload)
    mutated = replace(
        reference,
        combos=[mutated_combo] + reference.combos[1:],
    )
    comparison = compare_results(reference, mutated, left_label="expected", right_label="mutated")
    assert not comparison.equal
    assert comparison.code == "E_DIFF"
    assert comparison.location is not None
    assert comparison.location.candidate_id == original.candidate_id
    assert comparison.location.scenario_id == original.scenario_id
    assert comparison.location.round_index == 3
    with pytest_raises_e_diff():
        comparison.raise_if_diff()


class pytest_raises_e_diff:
    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        assert exc_type is BalanceError
        assert exc.code == ErrorCode.DIFF
        assert exc.object_id.startswith("candidate.")
        assert exc.field == "rounds"
        assert exc.detail["location"]["round_index"] == 3
        return True

