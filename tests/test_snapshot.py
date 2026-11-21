import json

import pytest

from loudmeter.errors import ReportMismatchError
from loudmeter.snapshot import SnapshotStore, unfinished_blocks


def _state(tag):
    return {"params": {"block_samples": 19200},
            "committed_blocks": [{"index": 0, "tag": tag}],
            "gating": None}


def test_roundtrip_and_chain(tmp_path):
    store = SnapshotStore(tmp_path)
    store.save(_state("a"))
    store.save(_state("b"))
    state = store.load_latest()
    assert state["committed_blocks"][0]["tag"] == "b"
    assert state["prev_hash"] != "0" * 64


def test_broken_chain_aborts(tmp_path):
    store = SnapshotStore(tmp_path)
    store.save(_state("a"))
    p2 = store.save(_state("b"))
    data = json.loads(p2.read_text())
    data["prev_hash"] = "f" * 64
    p2.write_text(json.dumps(data))
    with pytest.raises(ReportMismatchError):
        store.load_latest()


def test_tampered_content_aborts(tmp_path):
    store = SnapshotStore(tmp_path)
    p1 = store.save(_state("a"))
    data = json.loads(p1.read_text())
    data["committed_blocks"][0]["tag"] = "evil"
    p1.write_text(json.dumps(data))
    with pytest.raises(ReportMismatchError):
        store.load_latest()


def test_missing_field_aborts(tmp_path):
    store = SnapshotStore(tmp_path)
    p1 = store.save(_state("a"))
    data = json.loads(p1.read_text())
    del data["params"]
    p1.write_text(json.dumps(data))
    with pytest.raises(ReportMismatchError):
        store.load_latest()


def test_unfinished_blocks_skip_committed():
    state = {"committed_blocks": [{"index": 0}, {"index": 2}]}
    assert unfinished_blocks(state, 4) == [1, 3]


def test_atomic_rename_leaves_no_tmp(tmp_path):
    store = SnapshotStore(tmp_path)
    store.save(_state("a"))
    assert not list(tmp_path.glob("*.tmp"))
    assert (tmp_path / "snap_000000.json").exists()
