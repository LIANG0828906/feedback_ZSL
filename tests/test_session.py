from __future__ import annotations

import threading
from pathlib import Path

import pytest

from subtitle_localizer.errors import BusinessError, CancelledError, E_BUSY, E_TIMEBASE_INVALID
from subtitle_localizer.session import LayoutSession

from .helpers import make_case


def test_preview_reads_only_committed_snapshot(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir, truncate_trace=True) as session:
        baseline = session.commit()
        old_hash = baseline["commit_hash"]
        session.stage_params({"min_gap_ms": 450})
        before = session.refresh(0)
        assert before["commit_hash"] == old_hash
        assert session.has_pending
        committed = session.commit()
        after = session.refresh(0)
        assert committed["commit_hash"] != old_hash
        assert after["commit_hash"] == committed["commit_hash"]


def test_duplicate_session_returns_busy(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir):
        with pytest.raises(BusinessError) as caught:
            LayoutSession.open(case_dir)
        assert caught.value.code == E_BUSY
    with LayoutSession.open(case_dir):
        pass


def test_commit_exception_keeps_last_committed_version(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        report = session.commit()
        old_hash = report["commit_hash"]
        session.stage_sentence("S1", {"end": "00:01:00.000000"})
        with pytest.raises(BusinessError) as caught:
            session.commit()
        assert caught.value.code == E_TIMEBASE_INVALID
        assert session.commit_hash == old_hash
        assert session.refresh(0)["commit_hash"] == old_hash
    with LayoutSession.open(case_dir) as session:
        assert session.commit_hash == old_hash


def test_cancel_hook_keeps_last_committed_and_releases_lock(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        report = session.commit()
        old_hash = report["commit_hash"]
        session.stage_params({"default_cps": "11"})

        def cancel_at_checkpoint(point):
            del point
            raise CancelledError("test cancellation")

        with pytest.raises(CancelledError):
            session.commit(fault_hook=cancel_at_checkpoint)
        assert session.commit_hash == old_hash
    with LayoutSession.open(case_dir) as session:
        assert session.commit_hash == old_hash
        assert not session.has_pending


def test_online_layout_matches_offline_after_multiple_revisions(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        session.commit()
        session.stage_params({"min_gap_ms": 300})
        session.commit()
        session.stage_sentence("S3", {"speaker": "C"})
        online = session.commit()
        offline = session.compare_with_offline()
        assert online["revision"] == 3
        assert offline["commit_hash"] != online["commit_hash"]
        assert online["counts"]["reused"] >= 1


def test_channel_mapping_change_only_recomputes_mapped_speaker(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path, channels=6)
    with LayoutSession.open(case_dir) as session:
        session.commit()
        session.stage_params({"channel_map": {"A": 1}})
        report = session.commit()
        assert report["recomputed_sentence_ids"] == ["S1"]
        assert "S2" in report["reused_sentence_ids"]
        assert "S3" in report["reused_sentence_ids"]
        session.compare_with_offline()

