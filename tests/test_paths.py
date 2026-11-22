import copy

import pytest

from syncdemo import engine, runner
from syncdemo.checkpoint import CheckpointStore
from syncdemo.errors import DivergeError
from syncdemo.workers import WindowPool


def _store(tmp_path, sample, name):
    return CheckpointStore(
        tmp_path / name,
        seed=sample.meta["seed"],
        input_fingerprint=sample.meta["input_fingerprint"],
        algo_version=sample.meta["algo_version"],
    )


def test_three_paths_agree(sample, tmp_path):
    rcfg = runner.RunConfig(window_ticks=15)
    ecfg = engine.EngineConfig()

    pool = WindowPool(max_workers=2, queue_size=2, result_buffer=8)
    try:
        realtime = runner.run_realtime(sample, rcfg, ecfg,
                                       store=_store(tmp_path, sample, "rt"),
                                       pool=pool)
    finally:
        pool.close()

    store_rs = _store(tmp_path, sample, "rs")
    cut = sample.max_tick // 2
    partial = runner.run_realtime(sample, rcfg, ecfg, store=store_rs,
                                  cancel_after_tick=cut)
    assert partial.status == "cancelled"
    resumed = runner.run_resume(sample, rcfg, ecfg, store=store_rs)

    replay = runner.run_replay(sample, rcfg, ecfg)

    runner.compare(realtime, resumed)
    runner.compare(realtime, replay)
    assert realtime.settlement == sample.truth["settlement"]


def test_resume_with_mismatched_version_recomputes(sample, tmp_path):
    rcfg = runner.RunConfig(window_ticks=15)
    store = _store(tmp_path, sample, "mm")
    runner.run_realtime(sample, rcfg, store=store,
                        cancel_after_tick=sample.max_tick // 2)
    stale = CheckpointStore(
        tmp_path / "mm", seed=sample.meta["seed"],
        input_fingerprint=sample.meta["input_fingerprint"],
        algo_version="0.0.0-bogus",
    )
    assert stale.load() is None  # whole store discarded, no partial mixing


def test_compare_detects_divergence_with_location(sample):
    a = runner.run_replay(sample)
    b = runner.run_replay(sample)
    tick = 10
    uid = sorted(b.states[tick]["units"])[0]
    b.states[tick]["units"][uid]["hp"] += 1.0
    with pytest.raises(DivergeError) as exc:
        runner.compare(a, b)
    assert exc.value.code == "E_DIVERGE"
    assert exc.value.tick == tick
    assert exc.value.unit == uid
    assert exc.value.field == "hp"


def test_compare_detects_settlement_divergence(sample):
    a = runner.run_replay(sample)
    b = runner.run_replay(sample)
    b.settlement = copy.deepcopy(b.settlement)
    pid = sorted(b.settlement["players"])[0]
    b.settlement["players"][pid]["resource"] += 5.0
    with pytest.raises(DivergeError) as exc:
        runner.compare(a, b)
    assert exc.value.field == "settlement"
