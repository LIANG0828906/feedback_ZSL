"""要求 4/5/6/8：引擎增量重算、回退失效、跨档校验、任务锁与取消。"""

from __future__ import annotations

import json
import threading
from pathlib import Path

import pytest

from mediaaudit.engine import AuditEngine
from mediaaudit.errors import AuditError, ErrorCode
from mediaaudit.parameters import TierParams
from mediaaudit.slicing import CancelledError
from mediaaudit.util import content_hash
from tests.test_slicing import _source


def _engine(tmp_path: Path) -> AuditEngine:
    return AuditEngine(_source(), tmp_path / "audit")


def test_incremental_recompute_lists_reuse_and_recompute(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    p1 = TierParams(16000, 256, 1000, 250_000)
    engine.submit_params("tier", p1)
    first = engine.recompute("tier")
    assert len(first.recomputed) == 4 and first.reused == []

    # 完全相同内容去重，不产生新版本。
    record = engine.submit_params("tier", p1)
    assert record.version == 1

    p2 = TierParams(16000, 320, 1000, 250_000)
    engine.submit_params("tier", p2)
    second = engine.recompute("tier")
    assert len(second.recomputed) == 4
    assert all("gain" in item["reason"] for item in second.recomputed)


def test_rollback_hashes_match_historical_version(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    p1 = TierParams(16000, 256, 1000, 250_000)
    p2 = TierParams(24000, 300, 1000, 250_000)
    engine.submit_params("tier", p1)
    engine.recompute("tier")
    engine.submit_params("tier", p2)
    engine.recompute("tier")

    v1_hashes = [
        content_hash(json.loads(engine.store.read_slice_bytes("tier", 1, i)))
        for i in range(4)
    ]
    rolled = engine.rollback_params("tier", 1)
    assert rolled.origin == "rollback" and rolled.version == 3
    outcome = engine.recompute("tier")
    rb_hashes = [a.content_hash_value for a in outcome.artifacts]
    assert rb_hashes == v1_hashes


def test_stale_cache_rejected_and_recomputed(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    p1 = TierParams(16000, 256, 1000, 250_000)
    p2 = TierParams(24000, 300, 1000, 250_000)
    engine.submit_params("tier", p1)
    engine.recompute("tier")
    engine.submit_params("tier", p2)
    engine.recompute("tier")

    # 篡改 v1 的缓存切片，制造回退时的陈旧内容。
    target = engine.store.slice_path("tier", 1, 0)
    payload = json.loads(target.read_text())
    payload["audio_hex"] = "ff"
    target.write_text(json.dumps(payload))

    engine.rollback_params("tier", 1)
    outcome = engine.recompute("tier")
    assert outcome.stale_rejections
    first = outcome.stale_rejections[0]
    assert first["slice_index"] == 0
    assert first["cached_hash"] != first["expected_hash"]
    # 降级重算后产物哈希仍与历史期望一致。
    assert outcome.artifacts[0].content_hash_value == first["expected_hash"]


def test_busy_when_two_jobs_same_tier(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    engine.submit_params("tier", TierParams(16000, 256, 1000, 250_000))
    entered = threading.Event()
    errors: list[Exception] = []

    def hold_lock() -> None:
        event = engine._acquire("tier")
        entered.set()
        event.wait(0.5)
        engine._release("tier")

    worker = threading.Thread(target=hold_lock)
    worker.start()
    entered.wait(1.0)
    try:
        with pytest.raises(AuditError) as exc:
            engine.recompute("tier")
        assert exc.value.code == ErrorCode.E_BUSY
    finally:
        worker.join()


def test_cooperative_cancel_keeps_committed_state(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    engine.submit_params("tier", TierParams(16000, 256, 1000, 250_000))
    event = engine._acquire("tier")
    event.set()  # 预置取消信号
    try:
        with pytest.raises(CancelledError):
            engine._recompute_locked(
                "tier", engine.params.latest("tier"), event
            )
    finally:
        engine._release("tier")
    # 取消后锁已释放，可再次启动。
    outcome = engine.recompute("tier")
    assert len(outcome.recomputed) == 4


def test_span_mismatch_reports_offset(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    engine.submit_params("low", TierParams(16000, 256, 1000, 500_000))
    engine.submit_params("high", TierParams(48000, 256, 750, 375_000))
    mismatches = engine.check_spans("low", "high")
    assert mismatches
    first = mismatches[0]
    assert first.involved_tiers == ("low", "high")
    records = first.records
    assert records[0].source_end_sample != records[1].source_end_sample


def test_recovery_resumes_without_duplicate_writes(tmp_path: Path) -> None:
    work = tmp_path / "audit"
    engine = AuditEngine(_source(), work)
    engine.submit_params("tier", TierParams(16000, 256, 1000, 250_000))
    engine.recompute("tier")
    manifest_size = (work / "manifest.jsonl").stat().st_size

    recovered = AuditEngine(_source(), work)
    rows = recovered.store.load_manifest()
    assert len(rows) == 4
    # 已提交切片再次走提交流程不会追加清单。
    assert (work / "manifest.jsonl").stat().st_size == manifest_size
    assert recovered.committed["tier"][1] == 4


def test_trajectory_contains_required_fields(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    engine.submit_params("tier", TierParams(16000, 256, 1000, 250_000))
    engine.recompute("tier")
    events = engine.load_trajectory()
    done = [e for e in events if e["event"] == "recompute_done"][0]
    assert done["version"] == 1
    assert "content_hashes" in done
    assert done["recompute_count"] == 4
    assert "graph_version" in done
