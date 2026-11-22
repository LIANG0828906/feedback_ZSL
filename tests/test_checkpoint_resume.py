from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace

from balance_lab.budget import SimulationRequest
from balance_lab.checkpoint import CheckpointStore, SnapshotContext
from balance_lab.compare import compare_results
from balance_lab.models import ComboResult
from balance_lab.scheduler import run_validation


def test_resume_reuses_atomic_snapshot_and_matches_full(alpha, tmp_path) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=8, min_samples=2, total_sample_budget=300)
    checkpoint = tmp_path / "resume.sqlite3"
    partial_request = replace(request, interrupt_after_commits=9)
    partial = run_validation(
        project,
        candidates,
        partial_request,
        checkpoint_path=checkpoint,
        mode="resume",
    )
    assert partial.interrupted
    assert partial.schedule.committed_combos >= 9
    resumed = run_validation(
        project,
        candidates,
        request,
        checkpoint_path=checkpoint,
        mode="resume",
    )
    full = run_validation(project, candidates, request, mode="full")
    assert resumed.schedule.restored_combos == partial.schedule.committed_combos
    assert resumed.schedule.simulated_combos > 0
    assert compare_results(resumed, full, left_label="resumed", right_label="full").equal
    assert resumed.state_snapshot["checkpoint_load"]["valid"] is True


def test_version_mismatch_invalidates_entire_snapshot(alpha, tmp_path) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=4, min_samples=2, total_sample_budget=180)
    checkpoint = tmp_path / "versions.sqlite3"
    run_validation(
        project,
        candidates,
        request,
        checkpoint_path=checkpoint,
        mode="realtime",
    )
    context = SnapshotContext.create(project, candidates, request)
    store = CheckpointStore(checkpoint)
    assert store.count() > 0
    bad_budget = replace(context, budget_version="different")
    loaded = store.load(bad_budget)
    assert loaded.invalidated is True
    assert loaded.valid is False
    assert "" in loaded.reason or "mismatch" in loaded.reason
    assert store.count() == 0
    store.close()


def test_changed_project_hash_invalidates_entire_snapshot(alpha, tmp_path) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=3, min_samples=2, total_sample_budget=150)
    checkpoint = tmp_path / "project_hash.sqlite3"
    run_validation(
        project,
        candidates,
        request,
        checkpoint_path=checkpoint,
        mode="realtime",
    )
    context = SnapshotContext.create(project, candidates, request)
    store = CheckpointStore(checkpoint)
    loaded = store.load(replace(context, project_hash="not-the-project"))
    assert loaded.invalidated
    assert store.count() == 0
    store.close()


def test_checkpoint_writes_are_serialized_with_stable_sequence(alpha, tmp_path) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=2, min_samples=1, total_sample_budget=120)
    checkpoint = tmp_path / "serialized.sqlite3"
    store = CheckpointStore(checkpoint)
    context = SnapshotContext.create(project, candidates, request)
    source = _minimal_result(project, candidates)

    def commit(index: int) -> int:
        clone = ComboResult.from_dict(source.to_dict())
        clone = replace(
            clone,
            scenario_id=f"scene.synthetic.{index}",
            content_hash_value="",
        )
        return store.commit_combo(context, clone)

    with ThreadPoolExecutor(max_workers=6) as executor:
        sequences = list(executor.map(commit, range(12)))
    assert sorted(sequences) == list(range(1, 13))
    assert store.count() == 12
    assert store.load(context).last_sequence == 12
    store.close()


def _minimal_result(project, candidates) -> ComboResult:
    from balance_lab.combat import CandidateRuntime, simulate_combo
    from balance_lab.budget import build_budget_plan

    request = SimulationRequest(target_samples=1, min_samples=1, total_sample_budget=100)
    plan = build_budget_plan(project, (candidates.candidates[0],), request)
    combo = next(item for item in plan.combos if item.samples == 1)
    scenario = project.scenario_map()[combo.scenario_id]
    return simulate_combo(
        project,
        CandidateRuntime.from_candidate(project, candidates.candidates[0]),
        scenario,
        combo,
        per_thread_buffer_kb=plan.per_thread_buffer_kb,
    )

