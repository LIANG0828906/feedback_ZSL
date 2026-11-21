from __future__ import annotations

import copy
import json

import pytest

from npr_pipeline.canonical import canonical_json_bytes
from npr_pipeline.errors import E_SCENE_INVALID, PipelineError
from npr_pipeline.scene import parse_scene


def rectangle_scene(
    first_box=(10.0, 10.0, 90.0, 90.0),
    second_box=(120.0, 10.0, 200.0, 90.0),
):
    def path(path_id: str, box) -> dict:
        x0, y0, x1, y1 = box
        return {
            "id": path_id,
            "closed": True,
            "start": [x0, y0],
            "segments": [
                {"type": "line", "to": [x1, y0]},
                {"type": "line", "to": [x1, y1]},
                {"type": "line", "to": [x0, y1]},
                {"type": "line", "to": [x0, y0]},
            ],
        }

    return {
        "schema": "npr-scene-v1",
        "canvas": {"width": 240.0, "height": 120.0},
        "draw_order": ["zone_a", "zone_b"],
        "paths": [
            path("path_a", first_box),
            path("path_b", second_box),
        ],
        "zones": [
            {"id": "zone_a", "label": "A", "fill": "#aa5533", "path": "path_a"},
            {"id": "zone_b", "label": "B", "fill": "#3355aa", "path": "path_b"},
        ],
    }


def test_scene_geometry_is_deterministic(balanced_scene, sample_root) -> None:
    first = balanced_scene.geometry_document()
    raw = json.loads(
        (sample_root / "balanced" / "scene.json").read_text(encoding="utf-8")
    )
    second = parse_scene(copy.deepcopy(raw)).geometry_document()
    assert first["geometry_hash"] == balanced_scene.geometry_hash
    assert canonical_json_bytes(first) == canonical_json_bytes(second)


def test_missing_field_locates_zone() -> None:
    raw = rectangle_scene()
    del raw["zones"][1]["fill"]
    with pytest.raises(PipelineError) as caught:
        parse_scene(raw)
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.location == "zone:zone_b"
    assert caught.value.detail == "missing_field"


def test_unclosed_path_locates_path() -> None:
    raw = rectangle_scene()
    raw["paths"][0]["segments"][-1]["to"] = [10.0, 20.0]
    with pytest.raises(PipelineError) as caught:
        parse_scene(raw)
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.location == "path:path_a"
    assert caught.value.detail == "path_not_closed"


def test_non_finite_coordinate_rejected() -> None:
    raw = rectangle_scene()
    raw["paths"][0]["start"][0] = float("nan")
    with pytest.raises(PipelineError) as caught:
        parse_scene(raw)
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.location == "path:path_a"
    assert caught.value.detail == "coordinate_not_finite"


def test_overlapping_partitions_rejected() -> None:
    raw = rectangle_scene(
        first_box=(10.0, 10.0, 100.0, 90.0),
        second_box=(80.0, 20.0, 180.0, 100.0),
    )
    with pytest.raises(PipelineError) as caught:
        parse_scene(raw)
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.location == "zone:zone_a/zone_b"
    assert caught.value.detail == "zone_overlap"


def test_shared_boundary_is_not_overlap() -> None:
    raw = rectangle_scene(
        first_box=(10.0, 10.0, 100.0, 90.0),
        second_box=(100.0, 10.0, 190.0, 90.0),
    )
    scene = parse_scene(raw)
    assert scene.geometry_hash

