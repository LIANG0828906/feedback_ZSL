from __future__ import annotations

import json
import threading
from dataclasses import replace

from formation_workbench.checkpoint import CheckpointStore
from formation_workbench.engine import FormationEngine


def test_checkpoint_roundtrip_restores_all_caches(sample_scene, tmp_path):
    path = tmp_path / "cache.json"
    engine = FormationEngine(sample_scene, checkpoint_path=path)
    build = engine.build_all(save_checkpoint=True)
    assert build["checkpoint"]["saved"]
    payload = json.loads(path.read_text(encoding="utf-8"))["payload"]
    assert payload["commit_order"] == ["alpha", "bravo", "charlie", "delta"]
    assert payload["versions"]["algorithm"]
    assert payload["versions"]["fixed_point"]

    restored = FormationEngine(sample_scene, checkpoint_path=path)
    result = restored.try_restore_checkpoint()
    assert result.restored
    assert all(
        state["path_cache_hit"] and state["simulation_cache_hit"]
        for state in restored.member_states().values()
    )
    assert restored.compare_with_full().ok


def test_version_mismatch_invalidates_whole_snapshot(sample_scene, tmp_path):
    path = tmp_path / "cache.json"
    engine = FormationEngine(sample_scene, checkpoint_path=path)
    engine.build_all(save_checkpoint=True)
    changed_formation = replace(sample_scene.formation, version="slots-v2")
    changed_scene = replace(sample_scene, formation=changed_formation)
    restored = FormationEngine(changed_scene, checkpoint_path=path)
    result = restored.try_restore_checkpoint()
    assert not result.restored
    assert result.full_rebuild_required
    assert "mismatch" in result.reason
    assert restored.paths() == {}


def test_edited_map_mismatch_rebuilds_full(sample_scene, tmp_path):
    path = tmp_path / "cache.json"
    engine = FormationEngine(sample_scene, checkpoint_path=path)
    engine.build_all(save_checkpoint=True)
    engine.apply_cell_cost((11, 7), 4)
    restored = FormationEngine(sample_scene, checkpoint_path=path)
    result = restored.try_restore_checkpoint()
    assert not result.restored
    assert "map content hash mismatch" in result.reason


def test_corrupt_checkpoint_is_rejected(sample_scene, tmp_path):
    path = tmp_path / "cache.json"
    path.write_text('{"digest":"bad","payload":{}}', encoding="utf-8")
    store = CheckpointStore(path)
    result = store.read()
    assert not result.ok
    assert "digest mismatch" in (result.reason or "")
    engine = FormationEngine(sample_scene, checkpoint_path=path)
    restore = engine.try_restore_checkpoint()
    assert not restore.restored
    assert restore.full_rebuild_required


def test_concurrent_checkpoint_writes_remain_atomic(tmp_path):
    path = tmp_path / "concurrent.json"
    store = CheckpointStore(path)
    errors: list[BaseException] = []

    def writer(index: int) -> None:
        try:
            store.save({"writer": index, "members": ["alpha", "bravo", "charlie", "delta"]})
        except BaseException as exc:
            errors.append(exc)

    threads = [threading.Thread(target=writer, args=(index,)) for index in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert not errors
    result = store.read()
    assert result.ok
    assert result.payload is not None
    assert result.payload["members"] == ["alpha", "bravo", "charlie", "delta"]
