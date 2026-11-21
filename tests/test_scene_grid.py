from __future__ import annotations

import pytest

from lightprobe.errors import ProbeError
from lightprobe.grid import build_grid, grid_change
from lightprobe.samples import room_scene
from lightprobe.scene import load_scene_dict


def test_missing_scene_field_reports_path():
    payload = room_scene()
    del payload["sky"]["zenith_radiance"]
    with pytest.raises(ProbeError) as caught:
        load_scene_dict(payload)
    assert caught.value.code == "E_SCENE_INVALID"
    assert caught.value.details["path"] == "$.sky.zenith_radiance"


@pytest.mark.parametrize(
    ("occluder", "object_id"),
    [
        (
            {
                "id": "flat_box",
                "type": "aabb",
                "min": [1, 1, 1],
                "max": [1, 2, 2],
                "material": "plaster",
            },
            "flat_box",
        ),
        (
            {
                "id": "zero_sphere",
                "type": "sphere",
                "center": [1, 1, 1],
                "radius": 0,
                "material": "plaster",
            },
            "zero_sphere",
        ),
    ],
)
def test_degenerate_occluder_located(occluder, object_id):
    payload = room_scene()
    payload["occluders"].append(occluder)
    with pytest.raises(ProbeError) as caught:
        load_scene_dict(payload)
    assert caught.value.code == "E_SCENE_INVALID"
    assert caught.value.details["object_id"] == object_id


def test_grid_coordinates_hashes_and_neighbors(room):
    grid = build_grid(room, 2.0)
    assert grid.counts == (5, 3, 5)
    assert grid.probe_count == 75
    assert len(set(grid.coordinate_hashes)) == grid.probe_count
    center_id = grid.probe_id_at(2, 1, 2)
    assert len(grid.neighbors[center_id]) == 6
    for probe_id in grid.probe_ids:
        index = grid.index_by_id[probe_id]
        assert grid.coordinate_hashes[index] in grid.coordinate_hashes


def test_spacing_change_rebuilds_grid_and_reports_delta(room):
    old = build_grid(room, 2.0)
    new = build_grid(room, 1.0)
    report = grid_change(old, new)
    assert report.rebuilt
    assert report.old_count == 75
    assert report.new_count == 9 * 5 * 9
    assert report.count_delta == report.new_count - report.old_count
    assert report.added_coordinate_hashes
    assert report.removed_coordinate_hashes == ()

