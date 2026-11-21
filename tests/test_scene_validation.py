from __future__ import annotations

import json
from pathlib import Path

import pytest

from volrender.binary import decode_scene, encode_scene
from volrender.errors import AppError, ErrorCode
from volrender.injection import _read_scene_container

from tests.conftest import rewrite_header, scene_from_header_payload


def test_generated_scene_has_valid_geometry(sample_bundle):
    loaded = sample_bundle["loaded"]
    scene = loaded.scene
    assert scene.width == 48
    assert scene.height == 32
    assert len(scene.media) == 2
    assert {medium.medium_id for medium in scene.media} == {
        "glass_shell",
        "smoke_plume",
    }
    assert np_all_finite(scene.camera.intrinsics)
    assert np_all_finite(scene.camera.world_to_camera)
    assert abs(float(__import__("numpy").linalg.det(scene.camera.intrinsics))) > 1
    assert scene.density_version
    assert scene.fingerprint


def np_all_finite(array) -> bool:
    import numpy as np

    return bool(np.all(np.isfinite(array)))


def test_singular_camera_returns_scene_invalid(
    sample_bundle, tmp_path: Path
):
    def mutate(header):
        header["camera"]["K"][1] = list(header["camera"]["K"][0])

    path = rewrite_header(
        sample_bundle["scene_path"], tmp_path / "singular.volbin", mutate
    )
    with pytest.raises(AppError) as caught:
        decode_scene(path.read_bytes())
    error = caught.value
    assert error.code == ErrorCode.E_SCENE_INVALID
    assert error.field == "camera.K"
    assert "singular" in error.message


def test_density_index_out_of_range_is_located(
    sample_bundle, tmp_path: Path
):
    def mutate(header):
        header["media"][1]["density_block"] = 99

    path = rewrite_header(
        sample_bundle["scene_path"], tmp_path / "badindex.volbin", mutate
    )
    with pytest.raises(AppError) as caught:
        decode_scene(path.read_bytes())
    error = caught.value
    assert error.code == ErrorCode.E_SCENE_INVALID
    assert error.field == "media[1].density_block"
    assert error.medium_id == "smoke_plume"


def test_partial_medium_overlap_is_rejected(
    sample_bundle, tmp_path: Path
):
    with pytest.raises(AppError) as caught:
        scene_from_header_payload(
            sample_bundle["scene_path"],
            mutate_header=lambda header: header["media"][1].__setitem__(
                "max", [1.9, 0.55, 3.18]
            ),
        )
    error = caught.value
    assert error.code == ErrorCode.E_SCENE_INVALID
    assert error.field == "media.intervals"
    assert error.medium_id == "smoke_plume"


def test_missing_field_reports_first_failing_field(
    sample_bundle, tmp_path: Path
):
    path = rewrite_header(
        sample_bundle["scene_path"],
        tmp_path / "missing.volbin",
        lambda header: header["light"].pop("intensity"),
    )
    with pytest.raises(AppError) as caught:
        decode_scene(path.read_bytes())
    error = caught.value
    assert error.code == ErrorCode.E_SCENE_INVALID
    assert error.field == "light.intensity"


def test_checksum_and_truncation_are_rejected(sample_bundle):
    raw = sample_bundle["scene_path"].read_bytes()
    corrupted = bytearray(raw)
    corrupted[20] ^= 0x01
    with pytest.raises(AppError) as caught:
        decode_scene(bytes(corrupted))
    assert caught.value.code == ErrorCode.E_SCENE_INVALID
    assert caught.value.field == "binary.checksum"

    with pytest.raises(AppError) as caught:
        decode_scene(raw[:-5])
    assert caught.value.code == ErrorCode.E_SCENE_INVALID
    assert caught.value.field == "binary.length"


def test_nonfinite_density_is_rejected(sample_bundle):
    header, payload = _read_scene_container(
        sample_bundle["scene_path"].read_bytes()
    )
    import numpy as np

    values = np.frombuffer(payload, dtype="<f4").copy()
    values[0] = np.nan
    with pytest.raises(AppError) as caught:
        decode_scene(encode_scene(header, values.tobytes()))
    assert caught.value.code == ErrorCode.E_SCENE_INVALID
    assert caught.value.field == "density_blocks[0].values"
    assert caught.value.medium_id == "smoke_plume"
