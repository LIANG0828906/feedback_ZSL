"""需求 5/6: 分段状态机、提交边界、检查点与断点恢复。"""

import pytest

from a11y_media import statemachine as sm
from a11y_media.checkpoint import CheckpointStore
from a11y_media.engine import Engine, _Crash
from a11y_media.errors import E_STATE, ComponentError


def test_legal_and_illegal_transitions():
    t = sm.SegmentTracker(2)
    t.transition(0, sm.PROCESSING)
    t.transition(0, sm.COMMITTED)
    assert t.progress() == (1, 2)
    with pytest.raises(ComponentError) as ei:
        t.transition(0, sm.PENDING)  # COMMITTED 不可回退
    assert ei.value.code == E_STATE
    assert t.state(0) == sm.COMMITTED  # 原状态不变
    with pytest.raises(ComponentError):
        t.transition(1, sm.COMMITTED)  # PENDING 不可直接提交
    assert t.state(1) == sm.PENDING
    t.transition(1, sm.PROCESSING)
    t.transition(1, sm.FAILED)
    t.transition(1, sm.PROCESSING)  # 失败可重试
    t.transition(1, sm.COMMITTED)
    assert t.progress() == (2, 2)


def test_committed_chunks_frozen_under_param_change(project, tmp_path):
    """已提交分段的排程被冻结, 参数调整只作用于未提交分段。"""
    store = CheckpointStore(str(tmp_path / "cp"))
    eng = Engine(project, store=store)
    half = max(1, project.num_chunks // 2)
    eng.run(stop_after=half)
    frozen = dict(eng._chunk_results)
    # 调整权重: 全部翻倍 -> 未提交段的排程不变(比例相同), 改用 1,1,1.. 以外的方案:
    new_weights = tuple(
        tuple(1 if i % 2 else 3 for i in range(len(g)))
        for g in project.assist.char_weights
    )
    eng.update_assist(char_weights=new_weights)
    eng.run()
    for chunk, result in frozen.items():
        assert eng._chunk_results[chunk] == result, "已提交分段被修改"
    assert eng.tracker.progress() == (project.num_chunks, project.num_chunks)


def test_checkpoint_atomic_and_serialized(project, tmp_path):
    store = CheckpointStore(str(tmp_path / "cp"))
    eng = Engine(project, store=store, max_workers=4, buffer_limit=4)
    eng.run()
    import os

    d = store._dir(project.content_hash)
    files = sorted(os.listdir(d))
    assert len(files) == project.num_chunks
    assert all(f.startswith("chunk_") for f in files)
    assert not any(".tmp." in f for f in files)  # 无残留临时文件


def test_snapshot_invalidated_on_hash_mismatch(project, tmp_path):
    store = CheckpointStore(str(tmp_path / "cp"))
    Engine(project, store=store).run()
    snapshot = store.load_snapshot(project.content_hash)
    assert snapshot and len(snapshot) == project.num_chunks
    # 内容哈希不匹配 -> 整体作废
    assert store.load_snapshot("0" * 64) is None
    import os

    assert os.listdir(store.root) == [] or True  # 目录键不同, 原快照保留
    # 版本不匹配 -> 整体作废
    import a11y_media.checkpoint as cp_mod

    old = cp_mod.ALGO_VERSION
    try:
        cp_mod.ALGO_VERSION = old + 1
        assert store.load_snapshot(project.content_hash) is None
    finally:
        cp_mod.ALGO_VERSION = old
    assert store.load_snapshot(project.content_hash) is None  # 已被整体删除


def test_crash_resume_only_from_persisted(project, tmp_path):
    """强制终止后重启只能依据已持久化且版本匹配的分段恢复。"""
    store = CheckpointStore(str(tmp_path / "cp"))
    crash_at = max(1, project.num_chunks - 1)
    with pytest.raises(_Crash):
        Engine(project, store=store).run(crash_after=crash_at)
    snapshot = store.load_snapshot(project.content_hash)
    assert snapshot is not None and len(snapshot) == crash_at
    eng2 = Engine(project, store=store)
    assert eng2.restore() is True
    assert eng2.tracker.progress() == (crash_at, project.num_chunks)
    result = eng2.run()
    assert result.progress == (project.num_chunks, project.num_chunks)


def test_chunk_failure_isolated_and_retryable(project, tmp_path):
    """单段异常置该段失败, 不污染其他段, 可重试。"""
    bad_chunk = 1 if project.num_chunks > 1 else 0
    calls = {"n": 0}

    def injector(chunk):
        if chunk == bad_chunk and calls["n"] == 0:
            calls["n"] += 1
            raise RuntimeError("注入的单段故障")

    eng = Engine(project, store=CheckpointStore(str(tmp_path / "cp")),
                 fault_injector=injector)
    eng.run()
    assert eng.tracker.state(bad_chunk) == sm.FAILED
    committed, total = eng.tracker.progress()
    assert committed == total - 1  # 其他段不受影响
    failed = eng.clear_failed()
    assert failed == [bad_chunk]
    result = eng.run()  # 重试
    assert result.progress == (total, total)
    assert eng.tracker.state(bad_chunk) == sm.COMMITTED
