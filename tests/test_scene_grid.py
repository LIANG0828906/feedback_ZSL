from __future__ import annotations

import copy

import pytest

from probe_baker.errors import ErrorCode, ProbeError
from probe_baker.grid import build_probe_grid
from probe_baker.samples import room_scene_payload
from probe_baker.scene import load_scene, parse_scene


def test_scene_parse_rejects_missing_and_nonpositive_spacing() -> None:
    payload = room_scene_payload()
    del payload["sky"]
    with pytest.raises(ProbeError) as missing:
        parse_scene(payload)
    assert missing.value.code == ErrorCode.SCENE_INVALID

    payload = room_scene_payload()
    payload["spacing"] = 0.0
    with pytest.raises(ProbeError) as spacing:
        parse_scene(payload)
    assert spacing.value.code == ErrorCode.SCENE_INVALID


def test_scene_parse_rejects_invalid_occluders_and_mask() -> None:
    payload = room_scene_payload()
    payload["occluders"][0]["max"] = payload["occluders"][0]["min"]
    with pytest.raises(ProbeError) as box:
        parse_scene(payload)
    assert box.value.code == ErrorCode.SCENE_INVALID

    payload = room_scene_payload()
    payload["dynamic_mask"] = ["not_an_occluder"]
    with pytest.raises(ProbeError) as mask:
        parse_scene(payload)
    assert mask.value.code == ErrorCode.SCENE_INVALID


def test_scene_load_uses_local_file(tmp_path) -> None:
    import json

    path = tmp_path / "scene.json"
    path.write_text(json.dumps(room_scene_payload()), encoding="utf-8")
    scene = load_scene(path)
    assert scene.scene_id == "street_corner_room"
    assert len(scene.occluders) == 3


def test_grid_rebuild_reports_new_metrics_and_hash(room_scene) -> None:
    first = build_probe_grid(room_scene, 1.0)
    assert first.old_probe_count is None
    assert first.new_probe_count == first.grid.count > 0
    assert first.new_edge_count == first.grid.edge_count > 0
    assert first.rebuilt is True

    second = build_probe_grid(room_scene, 1.25, first.grid)
    assert second.old_probe_count == first.grid.count
    assert second.new_probe_count < first.grid.count
    assert second.structure_hash != first.structure_hash
    assert second.rebuilt is True

    repeated = build_probe_grid(room_scene, 1.25, second.grid)
    assert repeated.structure_hash == second.structure_hash
    assert repeated.rebuilt is False
    assert repeated.old_probe_count == repeated.new_probe_count


def test_grid_invalid_spacing_never_reuses_old_grid(room_scene) -> None:
    previous = build_probe_grid(room_scene, 1.0).grid
    with pytest.raises(ProbeError) as exc:
        build_probe_grid(room_scene, -0.5, previous)
    assert exc.value.code == ErrorCode.SCENE_INVALID


def test_grid_neighbour_topology_is_symmetric(room_scene) -> None:
    grid = build_probe_grid(room_scene, 2.0).grid
    assert grid.edge_count == int(grid.neighbor_edges.shape[0])
    for source, target in grid.neighbor_edges:
        assert target in set(grid.adjacency[int(source)].tolist())
        assert source in set(grid.adjacency[int(target)].tolist())

