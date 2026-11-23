"""要求 5：版本快照与提交边界。"""
import numpy as np
import pytest

from cadrender.errors import E_STATE, E_VER
from cadrender.versioning import Material, VersionStore


def _store():
    vs = VersionStore()
    vs.initial({"a": 1}, Material())
    return vs


def test_snapshot_immutable_and_incrementing():
    vs = _store()
    s1 = vs.latest()
    vs.commit({"a": 2}, Material())
    assert vs.latest_version == 2
    assert vs.get(1).scene_content == {"a": 1}  # 旧快照不被改写
    assert s1.version == 1


def test_preview_reads_latest_precise_locks():
    vs = _store()
    locked = vs.lock_for_precise(1)
    vs.commit({"a": 9}, Material())
    # 精确任务持有的锁定版本仍是旧快照
    assert locked.version == 1
    assert vs.latest_version == 2


def test_frozen_tile_query_old_version_e_ver():
    vs = _store()
    vs.freeze_tile(1, 7)
    vs.commit({"a": 2}, Material())
    assert 7 in vs.frozen_tiles(1)
    with pytest.raises(Exception) as ei:
        vs.frozen_tiles(99)
    assert ei.value.code == E_VER
    ctx = ei.value.to_dict()["chain"][0]["context"]
    assert ctx["requested"] == 99


def test_edit_frozen_tile_rejected():
    vs = _store()
    vs.freeze_tile(1, 3)
    with pytest.raises(Exception) as ei:
        vs.assert_editable(1, 3)
    assert ei.value.code == E_VER


def test_illegal_migration_e_state():
    vs = _store()
    vs.commit({"a": 2}, Material())
    vs.commit({"a": 3}, Material())
    vs.migrate(1, 3)  # 同一条链，合法
    other = VersionStore()
    other.initial({"b": 1}, Material())
    with pytest.raises(Exception) as ei:
        vs.migrate(1, 99)
    assert ei.value.code == E_STATE


def test_material_invalid_e_para():
    from cadrender.errors import E_PARA
    with pytest.raises(Exception) as ei:
        Material(ambient=-1).validate()
    assert ei.value.code == E_PARA
