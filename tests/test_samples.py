from __future__ import annotations

import json

from inverse_imaging.samples import generate_matrix, generate_sample


def test_same_seed_and_directory_is_byte_deterministic(tmp_path) -> None:
    directory = tmp_path / "sample"
    first = generate_sample(directory, seed=41, width=14, height=10, sphere_count=2)
    first_scene = (directory / "scene.json").read_bytes()
    first_target = (directory / "target.npy").read_bytes()
    first_partition = (directory / "groups.json").read_bytes()
    second = generate_sample(directory, seed=41, width=14, height=10, sphere_count=2)
    assert (directory / "scene.json").read_bytes() == first_scene
    assert (directory / "target.npy").read_bytes() == first_target
    assert (directory / "groups.json").read_bytes() == first_partition
    assert first["scene_id"] == second["scene_id"]
    assert first["scene_sha256"] == second["scene_sha256"]
    assert first["target_sha256"] == second["target_sha256"]


def test_sample_matrix_is_deterministic(tmp_path) -> None:
    first = generate_matrix(tmp_path / "first", seed=77)
    second = generate_matrix(tmp_path / "second", seed=77)
    assert first["manifest_hash"] == second["manifest_hash"]
    assert [item["scene_id"] for item in first["samples"]] == [
        item["scene_id"] for item in second["samples"]
    ]
    manifest = json.loads((tmp_path / "first" / "manifest.json").read_text())
    assert manifest["manifest_hash"] == first["manifest_hash"]
    assert len(manifest["samples"]) == 3

