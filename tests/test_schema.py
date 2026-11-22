from __future__ import annotations

import json
from pathlib import Path

import pytest

from diffrender.errors import AppError
from diffrender.schema import decode_target, load_dataset


def _scene_path(root: Path) -> Path:
    return root / "single" / "scene.json"


def test_schema_loads_generated_dataset(dataset):
    assert set(dataset.groups) == {"single", "multi"}
    task = dataset.task("single")
    assert len(task.targets) == 2
    assert "sphere_0.radius" in task.truth.values
    assert len(task.scene.cameras) == 2


def test_missing_field_reports_exact_path(dataset, dataset_copy):
    scene_path = _scene_path(dataset_copy)
    payload = json.loads(scene_path.read_text(encoding="utf-8"))
    del payload["cameras"][0]["intrinsics"]
    scene_path.write_text(json.dumps(payload), encoding="utf-8")
    copied = load_dataset(dataset_copy)
    with pytest.raises(AppError) as caught:
        copied.task("single")
    assert caught.value.code == "E_SCHEMA"
    assert caught.value.field_path == "cameras[0].intrinsics"


def test_non_finite_and_singular_camera_are_rejected(dataset, dataset_copy):
    scene_path = _scene_path(dataset_copy)
    payload = json.loads(scene_path.read_text(encoding="utf-8"))
    payload["geometries"][1]["radius"] = float("inf")
    scene_path.write_text(json.dumps(payload), encoding="utf-8")
    copied = load_dataset(dataset_copy)
    with pytest.raises(AppError) as caught:
        copied.task("single")
    assert caught.value.code == "E_SCHEMA"
    assert caught.value.field_path.endswith(".radius")

    payload = json.loads(
        _scene_path(dataset.root).read_text(encoding="utf-8")
    )
    payload["cameras"][0]["intrinsics"] = [
        [0.0, 0.0, 0.0],
        [0.0, 0.0, 0.0],
        [0.0, 0.0, 1.0],
    ]
    scene_path.write_text(json.dumps(payload), encoding="utf-8")
    copied = load_dataset(dataset_copy)
    with pytest.raises(AppError) as caught:
        copied.task("single")
    assert caught.value.code == "E_SCHEMA"
    assert caught.value.field_path == "cameras[0].intrinsics"


def test_truth_label_mismatch_is_rejected(dataset, dataset_copy):
    targets_path = dataset_copy / "single" / "targets.json"
    payload = json.loads(targets_path.read_text(encoding="utf-8"))
    del payload["truth"]["geometries"]["sphere_0"]["radius"]
    targets_path.write_text(json.dumps(payload), encoding="utf-8")
    copied = load_dataset(dataset_copy)
    with pytest.raises(AppError) as caught:
        copied.task("single")
    assert caught.value.code == "E_SCHEMA"
    assert caught.value.field_path == "truth"


def test_corrupt_target_decode_returns_decode_error(
    dataset: object, dataset_copy: Path
) -> None:
    task = dataset.task("single")
    target = task.targets[0]
    (dataset_copy / target.path).write_bytes(b"not-an-npy")
    copied = load_dataset(dataset_copy)
    copied_task = copied.task("single")
    with pytest.raises(AppError) as caught:
        decode_target(copied_task.dataset_root / target.path, target)
    assert caught.value.code == "E_DECODE"
    assert caught.value.field_path == f"targets.{target.id}.path"

