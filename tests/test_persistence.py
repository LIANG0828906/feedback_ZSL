from __future__ import annotations

import json
from pathlib import Path

import pytest

from subtitle_localizer.errors import BusinessError, E_RECOVERY_INVALID
from subtitle_localizer.persistence import load_state, verify_state
from subtitle_localizer.session import LayoutSession

from .helpers import make_case, load_json


def test_state_hash_chain_and_atomic_state_file(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        first = session.commit()
        session.stage_params({"min_gap_ms": 300})
        second = session.commit()
    state = load_state(case_dir / "layout_state.json")
    assert state is not None
    assert state["revision"] == 2
    assert state["pending"] is None
    assert [item["revision"] for item in state["history"]] == [1, 2]
    assert state["history"][1]["parent_hash"] == first["commit_hash"]
    assert state["report"]["commit_hash"] == second["commit_hash"]
    verify_state(state)
    assert not list(case_dir.glob("*.tmp"))
    assert not (case_dir / "layout_state.json.lock").exists()


@pytest.mark.parametrize("mutation", ["entry_hash", "missing_parent", "history_hash"])
def test_corrupt_hash_chain_or_missing_field_is_recovery_invalid(tmp_path: Path, mutation: str) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        session.commit()
    state = load_json(case_dir / "layout_state.json")
    if mutation == "entry_hash":
        state["report"]["sentences"][0]["entry_hash"] = "f" * 64
    elif mutation == "missing_parent":
        state["history"][0].pop("parent_hash")
    else:
        state["history_hash"] = "f" * 64
    path = case_dir / "corrupt_state.json"
    path.write_text(json.dumps(state, ensure_ascii=False), encoding="utf-8")
    with pytest.raises(BusinessError) as caught:
        load_state(path)
    assert caught.value.code == E_RECOVERY_INVALID


def test_stale_lock_from_dead_process_is_reclaimed(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    lock_path = case_dir / "layout_state.json.lock"
    lock_path.write_text('{"pid":99999999,"token":"dead"}', encoding="utf-8")
    with LayoutSession.open(case_dir) as session:
        session.commit()
        assert session.revision == 1
    assert not lock_path.exists()

