from __future__ import annotations

from pathlib import Path

from mocap_workbench.experiment import run_experiment, write_trajectory
from mocap_workbench.pipeline import SolvePipeline
from mocap_workbench.samples import SampleSpec, generate_sample
from mocap_workbench.solver import SolverConfig


def test_same_seed_generates_byte_identical_files(tmp_path):
    spec = SampleSpec(
        name="same",
        seed=912345,
        frames=40,
        joints=18,
        missing_rate=0.12,
        conflict="none",
    )
    left = generate_sample(tmp_path / "left", spec)
    right = generate_sample(tmp_path / "right", spec)
    for key in ("bvh", "constraints", "baseline"):
        left_path = Path(left[key])
        right_path = Path(right[key])
        assert left_path.name == right_path.name
        assert left_path.read_bytes() == right_path.read_bytes()


def test_trajectory_is_byte_identical_for_same_actions(compact_samples, tmp_path):
    sample = compact_samples["small"]
    config = SolverConfig(workers=1, strategy="window", window_size=20, window_overlap=4)
    hashes = []
    for run in ("a", "b"):
        out = tmp_path / run
        report = SolvePipeline(
            sample["bvh"],
            sample["constraints"],
            out / "snapshots",
            config,
        ).run(resume=False)
        trajectory = out / "trajectory.jsonl"
        hashes.append(
            write_trajectory(
                trajectory,
                report,
                bvh_path=sample["bvh"],
                constraints_path=sample["constraints"],
                config=config,
            )
        )
        assert trajectory.read_text(encoding="utf-8").count("\n") == report.frame_count + 1
    assert hashes[0] == hashes[1]
    assert (tmp_path / "a" / "trajectory.jsonl").read_bytes() == (
        tmp_path / "b" / "trajectory.jsonl"
    ).read_bytes()
    for left in sorted((tmp_path / "a" / "snapshots").iterdir()):
        right = tmp_path / "b" / "snapshots" / left.name
        assert right.exists()
        assert left.read_bytes() == right.read_bytes()


def test_scripted_two_scale_experiment(compact_samples, tmp_path):
    result = run_experiment(
        compact_samples["_root"]["path"], tmp_path / "output", workers=1
    )
    for name in ("small", "large"):
        comparison = result["comparisons"][name]
        assert comparison["complete"]
        assert comparison["pose_hashes_equal"]
        assert comparison["within_tolerance"]
        assert comparison["frame_count"] == compact_samples[name]["baseline_data"]["frame_count"]
    assert result["cache"]["small"]["only_affected_or_reusable_frames_recomputed"]
    assert result["determinism"]["small"]["trajectory_bytes_equal"]
    assert result["conflicts"]["conflict"]["conflicts"] > 0

