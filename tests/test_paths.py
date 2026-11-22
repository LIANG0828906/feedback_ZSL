"""Three-path consistency, parallel-vs-serial equality, E_DIFF链路."""
import numpy as np
import pytest

from jitter_workbench.compare import compare_results, compare_three
from jitter_workbench.errors import DiffError
from jitter_workbench.paths import run_offline, run_realtime, run_resume


def test_three_paths_identical(sample, params, ckpt_dir):
    r_off = run_offline(sample, params)
    r_rt = run_realtime(sample, params, ckpt_dir)
    r_re = run_resume(sample, params, ckpt_dir, stop_after_windows=137)
    report = compare_three(r_rt, r_re, r_off)
    assert report["match"]
    assert r_off.stats["output_samples"] == sample.duration_samples


def test_parallel_equals_serial(sample, params, ckpt_dir):
    r_off = run_offline(sample, params)
    for workers in (1, 2, 4, 8):
        r_rt = run_realtime(sample, params, ckpt_dir, n_workers=workers,
                            max_pending_frames=8)
        assert compare_results("offline", r_off, "rt%d" % workers,
                               r_rt)["match"]


def test_resume_from_many_points(sample, params, ckpt_dir):
    r_off = run_offline(sample, params)
    for stop in (1, 50, 150, 299):
        r_re = run_resume(sample, params, ckpt_dir, stop_after_windows=stop)
        assert compare_results("offline", r_off, "resume", r_re)["match"]


def test_e_diff_sample_mismatch(sample, params):
    r_a = run_offline(sample, params)
    r_b = run_offline(sample, params)
    r_b.output[12345] += 1.0
    with pytest.raises(DiffError) as ei:
        compare_results("a", r_a, "b", r_b)
    assert ei.value.code == "E_DIFF"
    assert ei.value.detail["sample_index"] == 12345
    assert "context_a" in ei.value.detail


def test_e_diff_decision_mismatch(sample, params):
    from jitter_workbench.controller import ControllerParams
    r_a = run_offline(sample, params)
    other = ControllerParams(max_delay_ms=200)
    r_b = run_offline(sample, other)
    ka = [d.key() for d in r_a.decisions]
    kb = [d.key() for d in r_b.decisions]
    if ka != kb:
        with pytest.raises(DiffError) as ei:
            compare_results("a", r_a, "b", r_b)
        assert ei.value.field in ("decisions", "output", "energy")


def test_gap_and_energy_stats_identical(sample, params, ckpt_dir):
    r_rt = run_realtime(sample, params, ckpt_dir)
    r_off = run_offline(sample, params)
    assert r_rt.gap_flags == r_off.gap_flags
    assert np.array_equal(r_rt.energy, r_off.energy)
    assert r_rt.dup_records == r_off.dup_records
