from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from inverse_imaging.errors import E_SCENE_INVALID, AppError
from inverse_imaging.scene import load_scene


def _base_scene(target_shape=(6, 8, 3)) -> dict:
    return {
        "resolution": {"width": 8, "height": 6},
        "camera": {
            "position": [0.0, 0.0, 5.0],
            "rotation": [0.0, 0.0, 0.0],
            "intrinsics": {"fx": 80.0, "fy": 80.0, "cx": 4.0, "cy": 3.0},
        },
        "geometry": {
            "spheres": [{"center": [0.0, 0.0, 0.0], "radius": 1.0}],
            "plane": {"point": [0.0, 0.0, -2.0], "normal": [0.0, 0.0, 1.0]},
        },
        "material": {
            "sphere_albedo": [[0.7, 0.4, 0.3]],
            "plane_albedo": [0.2, 0.3, 0.4],
            "ambient": 0.05,
        },
        "light": {"direction": [0.2, 0.5, 0.8], "intensity": 1.2},
        "target": "target.npy",
    }


def _write_scene(tmp_path: Path, payload: dict, target_shape=(6, 8, 3)) -> Path:
    np.save(tmp_path / "target.npy", np.zeros(target_shape, dtype=np.float64))
    scene_path = tmp_path / "scene.json"
    scene_path.write_text(json.dumps(payload), encoding="utf-8")
    return scene_path


def test_scene_load_valid(tmp_path: Path) -> None:
    scene = load_scene(_write_scene(tmp_path, _base_scene()))
    assert scene.width == 8
    assert scene.height == 6
    assert scene.params.sphere_count == 1
    assert scene.scene_id


def test_scene_invalid_radius_has_field(tmp_path: Path) -> None:
    payload = _base_scene()
    payload["geometry"]["spheres"][0]["radius"] = 0.0
    path = _write_scene(tmp_path, payload)
    with pytest.raises(AppError) as error:
        load_scene(path)
    assert error.value.code == E_SCENE_INVALID
    assert error.value.field == "geometry.spheres.0.radius"


def test_scene_singular_matrix_has_field(tmp_path: Path) -> None:
    payload = _base_scene()
    payload["camera"].pop("rotation")
    payload["camera"]["rotation_matrix"] = [
        [1.0, 0.0, 0.0],
        [0.0, 0.0, 0.0],
        [0.0, 0.0, 1.0],
    ]
    path = _write_scene(tmp_path, payload)
    with pytest.raises(AppError) as error:
        load_scene(path)
    assert error.value.code == E_SCENE_INVALID
    assert error.value.field == "camera.rotation_matrix"


def test_scene_target_shape_mismatch_has_field(tmp_path: Path) -> None:
    path = _write_scene(tmp_path, _base_scene(), target_shape=(5, 8, 3))
    with pytest.raises(AppError) as error:
        load_scene(path)
    assert error.value.code == E_SCENE_INVALID
    assert error.value.field == "target"
    assert error.value.context["expected_shape"] == [6, 8, 3]


def test_scene_missing_light_direction_has_field(tmp_path: Path) -> None:
    payload = _base_scene()
    payload["light"]["direction"] = [0.0, 0.0, 0.0]
    path = _write_scene(tmp_path, payload)
    with pytest.raises(AppError) as error:
        load_scene(path)
    assert error.value.field == "light.direction"


def test_world_to_camera_extrinsics_are_used(tmp_path: Path) -> None:
    payload = _base_scene()
    payload["camera"].pop("position")
    payload["camera"].pop("rotation")
    payload["camera"]["extrinsics"] = {
        "world_to_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, 0.0],
            [0.0, 0.0, 1.0, -5.0],
            [0.0, 0.0, 0.0, 1.0],
        ]
    }
    scene = load_scene(_write_scene(tmp_path, payload))
    assert scene.params.values["camera"]["position.2"] == pytest.approx(5.0)
    assert scene.params.values["camera"]["rotation.0"] == pytest.approx(0.0)

