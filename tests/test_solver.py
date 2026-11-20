from __future__ import annotations

from dataclasses import replace

import numpy as np

from mocap_workbench.bvh import parse_bvh
from mocap_workbench.constraints import ConstraintSet, load_constraints
from mocap_workbench.solver import (
    SolveCache,
    SolverConfig,
    select_strategy,
    solve_frames,
)


def _load(record):
    document = parse_bvh(record["bvh"])
    constraints = load_constraints(record["constraints"], document)
    return document, constraints


def test_small_sample_solves_within_tolerance(samples):
    document, constraints = _load(samples["small"])
    config = SolverConfig(workers=1, strategy="batch")
    results = solve_frames(
        document,
        constraints,
        range(document.frame_count),
        config,
        cache=SolveCache(),
    )
    assert len(results) == document.frame_count
    assert all(result.satisfied for result in results)
    assert all(not result.conflicts for result in results)
    assert max(result.total_residual for result in results) < 0.05


def test_batch_and_window_are_frame_complete_and_hash_identical(compact_samples):
    document, constraints = _load(compact_samples["large"])
    frames = list(range(document.frame_count))
    config = SolverConfig(
        workers=1,
        max_iterations=24,
        window_size=64,
        window_overlap=8,
    )
    batch = solve_frames(
        document,
        constraints,
        frames,
        config,
        strategy="batch",
        cache=SolveCache(),
    )
    window = solve_frames(
        document,
        constraints,
        frames,
        config,
        strategy="window",
        cache=SolveCache(),
    )
    assert [item.frame for item in batch] == frames
    assert [item.frame for item in window] == frames
    assert all(left.pose_hash == right.pose_hash for left, right in zip(batch, window))
    assert max(
        abs(left.total_residual - right.total_residual)
        for left, right in zip(batch, window)
    ) <= 1.0e-12
    assert max(
        float(np.max(np.abs(left.pose.world - right.pose.world)))
        for left, right in zip(batch, window)
    ) <= 1.0e-12


def test_tolerance_edit_reuses_all_cached_poses(samples):
    document, constraints = _load(samples["small"])
    config = SolverConfig(workers=1)
    cache = SolveCache()
    frames = list(range(document.frame_count))
    first = solve_frames(document, constraints, frames, config, cache=cache)
    first_stats = cache.stats()
    constraint_id = constraints.constraints[0].id
    changed = constraints.with_parameter(
        constraint_id,
        tolerance=constraints.get(constraint_id).tolerance + 0.001,
    )
    second = solve_frames(document, changed, frames, config, cache=cache)
    second_stats = cache.stats()
    assert second_stats["hits"] - first_stats["hits"] == document.frame_count
    assert all(left.pose_hash == right.pose_hash for left, right in zip(first, second))


def test_frame_specific_target_edit_only_recomputes_that_frame(samples):
    document, constraints = _load(samples["small"])
    config = SolverConfig(workers=1)
    cache = SolveCache()
    frames = list(range(document.frame_count))
    solve_frames(document, constraints, frames, config, cache=cache)
    before = cache.stats()
    effector = next(item for item in constraints.constraints if item.kind == "effector")
    targets = dict(effector.frame_targets)
    target_frame = next(
        frame for frame, target in sorted(targets.items()) if target is not None
    )
    targets[target_frame] = targets[target_frame] + np.array([0.001, -0.001, 0.0])
    changed_spec = replace(
        effector,
        frame_targets=tuple(sorted(targets.items())),
    )
    changed = ConstraintSet(
        constraints=tuple(
            changed_spec if item.id == effector.id else item
            for item in constraints.constraints
        ),
        revision=constraints.revision + 1,
        name=constraints.name,
    )
    solve_frames(document, changed, frames, config, cache=cache)
    after = cache.stats()
    assert after["hits"] - before["hits"] == document.frame_count - 1
    assert after["misses"] - before["misses"] == 1


def test_bone_length_conflict_report_is_locatable(compact_samples):
    document, constraints = _load(compact_samples["conflict"])
    result = solve_frames(
        document,
        constraints,
        [0],
        SolverConfig(workers=1),
        cache=SolveCache(),
    )[0]
    assert not result.satisfied
    assert result.diagnostic is not None
    report = result.diagnostic
    assert report["frame"] == 0
    assert report["conflict_constraints"]
    assert report["worst_constraint_id"] in report["conflict_constraints"]
    assert report["joints"]
    assert report["worst_residual"] > report["worst_tolerance"]
    assert "frame 0" in report["evidence"]
    assert report["evidence"]


def test_parallel_worker_count_does_not_change_hashes(compact_samples):
    document, constraints = _load(compact_samples["small"])
    frames = list(range(document.frame_count))
    sequential = solve_frames(
        document,
        constraints,
        frames,
        SolverConfig(workers=1),
        strategy="batch",
        cache=SolveCache(),
    )
    parallel = solve_frames(
        document,
        constraints,
        frames,
        SolverConfig(workers=2),
        strategy="batch",
        cache=SolveCache(),
    )
    assert [item.pose_hash for item in sequential] == [
        item.pose_hash for item in parallel
    ]


def test_auto_strategy_switches_with_complexity(compact_samples):
    small_document, small_constraints = _load(compact_samples["small"])
    large_document, large_constraints = _load(compact_samples["large"])
    config = SolverConfig(
        workers=1,
        batch_frame_limit=100,
        batch_complexity_limit=1_000_000,
        window_size=32,
        window_overlap=4,
    )
    assert select_strategy(small_document, small_constraints, config) == "batch"
    assert select_strategy(large_document, large_constraints, config) == "window"

