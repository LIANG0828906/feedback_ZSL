import json
import threading

import pytest

from syncdemo.checkpoint import CheckpointStore
from syncdemo.errors import SyncError

META = {"seed": 7, "input_fingerprint": "fp", "algo_version": "1.0.0"}


def _store(path, **overrides):
    meta = {**META, **overrides}
    return CheckpointStore(path, **meta)


def test_commit_load_roundtrip(tmp_path):
    store = _store(tmp_path / "cp")
    store.commit(1, {"tick": 30, "state": {"a": 1}})
    store.commit(2, {"tick": 60, "state": {"a": 2}})
    snaps = _store(tmp_path / "cp").load()
    assert [s["tick"] for s in snaps] == [30, 60]


def test_metadata_mismatch_discards_everything(tmp_path):
    store = _store(tmp_path / "cp")
    store.commit(1, {"tick": 30})
    for key, value in (("seed", 8), ("input_fingerprint", "other"),
                       ("algo_version", "2.0.0")):
        assert _store(tmp_path / "cp", **{key: value}).load() is None


def test_tampered_snapshot_invalidates_store(tmp_path):
    store = _store(tmp_path / "cp")
    store.commit(1, {"tick": 30})
    final = tmp_path / "cp" / "window_1.json"
    record = json.loads(final.read_text())
    record["snapshot"]["tick"] = 999  # tamper without fixing the hash
    final.write_text(json.dumps(record))
    assert _store(tmp_path / "cp").load() is None


def test_committed_window_not_silently_rewritten(tmp_path):
    store = _store(tmp_path / "cp")
    store.commit(1, {"tick": 30})
    store.commit(1, {"tick": 30})  # idempotent identical recommit
    with pytest.raises(SyncError):
        store.commit(1, {"tick": 31})


def test_concurrent_commits_serialised(tmp_path):
    store = _store(tmp_path / "cp")

    def work(window):
        store.commit(window, {"tick": window * 30})

    threads = [threading.Thread(target=work, args=(w,)) for w in range(1, 9)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    snaps = _store(tmp_path / "cp").load()
    assert [s["tick"] for s in snaps] == [w * 30 for w in range(1, 9)]
    leftovers = list((tmp_path / "cp").glob("*.tmp"))
    assert leftovers == []
