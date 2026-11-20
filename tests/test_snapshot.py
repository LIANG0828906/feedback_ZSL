from __future__ import annotations

from pathlib import Path

import pytest

from volrender.budget import BudgetConfig
from volrender.errors import SnapshotMismatchError
from volrender.renderer import RayCaster
from volrender.snapshot import SnapshotManager, load_snapshot, save_snapshot, stale_fields
from volrender.transfer import TransferFunction
from volrender.view import CameraView
from volrender.volume import load_volume


@pytest.fixture()
def rendered(volume_path: Path):
    volume = load_volume(volume_path)
    ray = RayCaster(volume, TransferFunction())
    result = ray.render(
        CameraView(),
        width=32,
        height=32,
        step_size=2.0,
        budget=BudgetConfig(max_samples=10**12, max_memory_bytes=10**12),
    )
    return result


def test_atomic_snapshot_round_trip(tmp_path: Path, rendered) -> None:
    path = tmp_path / "snapshot.json"
    save_snapshot(path, rendered)
    loaded = load_snapshot(path, expected_content_hash=rendered.content_hash)
    assert loaded["version"] == 1
    assert loaded["content_hash"] == rendered.content_hash
    assert loaded["view_matrix"] == rendered.view_matrix
    assert loaded["control_points"] == rendered.control_points
    assert loaded["budget"] == rendered.budget.to_dict()
    assert not list(tmp_path.glob("*.tmp"))


def test_stale_fields_identify_all_rerender_triggers(rendered) -> None:
    snapshot = rendered.snapshot_fields()
    current = rendered.snapshot_fields()
    current["view_matrix"] = CameraView(yaw_deg=50.0).matrix_list
    current["control_points"] = [[0, 0, 0, 0, 0], [1, 1, 1, 1, 1]]
    current["budget"] = {**current["budget"], "max_samples": 7}
    current["step_size"] = 9.0
    current["precision_mode"] = "fp64"
    current["degradation_stages"] = ["increase_step"]
    assert stale_fields(snapshot, current) == [
        "view",
        "lut",
        "budget",
        "step",
        "precision",
        "degradation",
    ]


def test_hash_mismatch_aborts_without_advancing_manager(tmp_path: Path, rendered) -> None:
    good = tmp_path / "good.json"
    manager = SnapshotManager()
    manager.commit(good, rendered)
    committed = manager.last_snapshot
    assert manager.current_result is rendered

    bad = tmp_path / "bad.json"
    payload = good.read_text(encoding="utf-8").replace(rendered.content_hash, "0" * 64)
    bad.write_text(payload, encoding="utf-8")
    with pytest.raises(SnapshotMismatchError) as raised:
        manager.load(bad)
    assert raised.value.code == "E_SNAPSHOT_MISMATCH"
    assert manager.last_snapshot == committed
    assert manager.current_result is rendered


def test_missing_version_is_a_snapshot_mismatch(tmp_path: Path, rendered) -> None:
    path = tmp_path / "missing.json"
    save_snapshot(path, rendered)
    text = path.read_text(encoding="utf-8")
    text = text.replace('"version": 1,', "")
    path.write_text(text, encoding="utf-8")
    with pytest.raises(SnapshotMismatchError) as raised:
        load_snapshot(path)
    assert raised.value.detail in ("MISSING_FIELD", "HASH_MISMATCH")
    assert raised.value.field in ("version", "snapshot_digest")


def test_version_mismatch_is_rejected(tmp_path: Path, rendered) -> None:
    path = tmp_path / "version.json"
    payload = save_snapshot(path, rendered)
    payload["version"] = 999
    # Leave the digest stale on purpose: version is checked first.
    path.write_text(__import__("json").dumps(payload), encoding="utf-8")
    with pytest.raises(SnapshotMismatchError) as raised:
        load_snapshot(path)
    assert raised.value.detail == "VERSION_MISMATCH"
    assert raised.value.field == "version"

