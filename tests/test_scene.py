from __future__ import annotations

import copy

import pytest

from dustrender.errors import E_SCENE_INVALID, DustRenderError
from dustrender.scene import Scene

from conftest import SAMPLES, pack_scene, unpack_scene


@pytest.mark.parametrize(
    "filename",
    ["smoke_small.dust", "nested_medium.dust", "triple_wide.dust"],
)
def test_binary_round_trip_is_byte_identical(filename: str) -> None:
    path = SAMPLES / filename
    scene = Scene.load(path)
    assert scene.to_bytes() == path.read_bytes()
    assert scene.scene_sha256 == Scene.from_bytes(path.read_bytes()).scene_sha256


@pytest.mark.parametrize(
    ("mutation", "expected_path"),
    [
        (lambda m: m["screen"].pop("width"), "$.screen.width"),
        (lambda m: m["camera"].__setitem__("world_to_camera", [[0] * 4 for _ in range(4)]), "$.camera.world_to_camera"),
        (lambda m: m["media"][0]["shape"].__setitem__("radius", 0), "$.media[0].shape.radius"),
        (lambda m: m["media"][0].__setitem__("scattering", [-1, 0, 0]), "$.media[0].scattering[0]"),
        (lambda m: m["media"][0].__setitem__("parent", "missing"), "$.media[0].parent"),
        (lambda m: m["density_fields"][0]["blocks"][0].__setitem__("offset", 999999), "$.density_fields[0].blocks[0].offset"),
        (lambda m: m["density_fields"][0]["blocks"][0]["index"].__setitem__(0, 99), "$.density_fields[0].blocks[0].index[0]"),
    ],
)
def test_scene_validation_reports_field_path(mutation, expected_path: str) -> None:
    metadata, payload = unpack_scene(SAMPLES / "smoke_small.dust")
    mutated = copy.deepcopy(metadata)
    mutation(mutated)
    with pytest.raises(DustRenderError) as caught:
        Scene.from_bytes(pack_scene(mutated, payload))
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.field_path == expected_path


def test_payload_hash_mismatch_is_rejected() -> None:
    metadata, payload = unpack_scene(SAMPLES / "smoke_small.dust")
    with pytest.raises(DustRenderError) as caught:
        Scene.from_bytes(pack_scene(metadata, payload, corrupt_hash=True))
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.field_path == "$.payload_sha256"


def test_nested_medium_containment_is_validated() -> None:
    metadata, payload = unpack_scene(SAMPLES / "nested_medium.dust")
    mutated = copy.deepcopy(metadata)
    mutated["media"][1]["shape"]["center"] = [8.0, 8.0, 8.0]
    with pytest.raises(DustRenderError) as caught:
        Scene.from_bytes(pack_scene(mutated, payload))
    assert caught.value.code == E_SCENE_INVALID
    assert caught.value.field_path == "$.media[1].parent"
