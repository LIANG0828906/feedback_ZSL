import json

from scoreforge.checkpoint import CheckpointStore, restore_session
from scoreforge.model import Score
from scoreforge.recompute import (
    _skip_committed_ops,
    compare_scores,
    make_edit_script,
    realtime_run,
    run_script,
)
from scoreforge.session import EditSession


def _committed_session(project_data, store, commits):
    session = EditSession(Score(project_data), store=store)
    run_script(session, make_edit_script(project_data),
               stop_after_commits=commits)
    return session


def test_resume_after_crash(project_data, tmp_path):
    script = make_edit_script(project_data)
    store = CheckpointStore(tmp_path, project_data["project_id"])
    crashed = _committed_session(project_data, store, 3)
    assert len(crashed.commit_log) == 3
    del crashed  # 模拟强杀

    restored = restore_session(project_data, store)
    assert len(restored.committed) == 3
    run_script(restored, _skip_committed_ops(restored, script))

    expected = realtime_run(project_data, script)
    compare_scores(expected, restored.score)


def test_commit_sequence_stable(project_data, tmp_path):
    store = CheckpointStore(tmp_path, project_data["project_id"])
    session = _committed_session(project_data, store, 4)
    seqs = [r.seq for r in session.commit_log]
    assert seqs == [0, 1, 2, 3]
    manifest = json.loads(store.manifest_path.read_text(encoding="utf-8"))
    assert [c["seq"] for c in manifest["commits"]] == [0, 1, 2, 3]


def test_param_version_mismatch_invalidates_all(project_data, tmp_path):
    store = CheckpointStore(tmp_path, project_data["project_id"])
    _committed_session(project_data, store, 2)
    manifest = json.loads(store.manifest_path.read_text(encoding="utf-8"))
    manifest["param_version"] = "0.0-old"
    store.manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    assert store.load_validated() is None
    assert not store.manifest_path.exists()
    assert not list(store.dir.glob("fragment_*.json"))


def test_hash_mismatch_invalidates_all(project_data, tmp_path):
    store = CheckpointStore(tmp_path, project_data["project_id"])
    _committed_session(project_data, store, 2)
    frag = sorted(store.dir.glob("fragment_*.json"))[0]
    payload = json.loads(frag.read_text(encoding="utf-8"))
    payload["fragment"]["measure"]["notes"][0]["duration_ticks"] += 1
    frag.write_text(json.dumps(payload), encoding="utf-8")
    assert store.load_validated() is None
    assert not list(store.dir.glob("fragment_*.json"))


def test_restore_falls_back_to_source(project_data, tmp_path):
    store = CheckpointStore(tmp_path, project_data["project_id"])
    _committed_session(project_data, store, 2)
    store.manifest_path.write_text("not json", encoding="utf-8")
    session = restore_session(project_data, store)
    assert len(session.committed) == 0  # 从源样本重新加载
