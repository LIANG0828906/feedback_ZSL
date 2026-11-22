"""Checkpoint atomicity, version/hash gating, force-kill resume boundary."""
import json
import os

import numpy as np

from jitter_workbench.checkpoint import CheckpointManager
from jitter_workbench.controller import ControllerParams
from jitter_workbench.compare import compare_results
from jitter_workbench.paths import run_offline, run_realtime, run_resume


def test_atomic_write_no_tmp_left(sample, params, ckpt_dir):
    run_realtime(sample, params, ckpt_dir, stop_after_windows=20)
    assert os.path.exists(os.path.join(ckpt_dir, "state.json"))
    assert not os.path.exists(os.path.join(ckpt_dir, "state.json.tmp"))
    with open(os.path.join(ckpt_dir, "state.json")) as fh:
        state = json.load(fh)
    assert state["next_seq"] == 20
    assert state["content_hash"] == sample.content_hash
    assert state["policy_version"] == params.version


def test_hash_mismatch_discards_snapshot(sample, params, ckpt_dir):
    run_realtime(sample, params, ckpt_dir, stop_after_windows=20)
    mgr = CheckpointManager(ckpt_dir)
    assert mgr.load(content_hash="0" * 64,
                    policy_version=params.version) is None
    assert not mgr.exists()  # snapshot discarded, no partial reuse


def test_policy_version_mismatch_discards(sample, params, ckpt_dir):
    run_realtime(sample, params, ckpt_dir, stop_after_windows=20)
    mgr = CheckpointManager(ckpt_dir)
    assert mgr.load(content_hash=sample.content_hash,
                    policy_version="deadbeef") is None
    assert not mgr.exists()


def test_corrupt_snapshot_ignored(sample, params, ckpt_dir):
    run_realtime(sample, params, ckpt_dir, stop_after_windows=20)
    with open(os.path.join(ckpt_dir, "state.json"), "w") as fh:
        fh.write("{not json")
    mgr = CheckpointManager(ckpt_dir)
    assert mgr.load(content_hash=sample.content_hash,
                    policy_version=params.version) is None


def test_forced_kill_resume(sample, params, ckpt_dir):
    """Simulate kill -9: run stops mid-stream; only persisted windows
    may be used on restart, and the resumed run must equal the full
    offline replay."""
    r_part = run_realtime(sample, params, ckpt_dir, stop_after_windows=88)
    assert r_part.stats["committed_windows"] == 88
    # "restart": brand-new engine continues from the persisted snapshot
    r_done = run_realtime(sample, params, ckpt_dir)
    r_off = run_offline(sample, params)
    assert compare_results("resumed", r_done, "offline", r_off)["match"]


def test_changed_params_restart_from_zero(sample, ckpt_dir):
    p1 = ControllerParams()
    run_realtime(sample, p1, ckpt_dir, stop_after_windows=40)
    # different policy params -> snapshot invalid -> full replay
    p2 = ControllerParams(init_delay_ms=200)
    r = run_realtime(sample, p2, ckpt_dir)
    assert r.stats["committed_windows"] == sample.num_frames
    r_off = run_offline(sample, p2)
    assert np.array_equal(r.output, r_off.output)
