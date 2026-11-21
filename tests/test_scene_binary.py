from __future__ import annotations

import hashlib
import struct

import numpy as np
import pytest

from tactical_scene.errors import ErrorCode, TacticalError
from tactical_scene.scene import (
    AABBShape,
    CapsuleShape,
    Scene,
    SceneUnit,
    load_scene,
    write_scene,
)


def _transform(x: float, y: float, z: float) -> tuple[float, ...]:
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 3] = (x, y, z)
    return tuple(float(value) for value in matrix.reshape(-1))


def _example_scene() -> Scene:
    units = (
        SceneUnit(2, "capsule", CapsuleShape((0, 0, -2), (0, 0, 2), 0.75), _transform(4, 0, 10)),
        SceneUnit(1, "box", AABBShape((-1, -2, -3), (1, 2, 3)), _transform(0, 0, 10)),
    )
    return Scene(version=7, units=units)


def test_binary_roundtrip_is_deterministic(tmp_path):
    scene = _example_scene()
    first = tmp_path / "first.bfscene"
    second = tmp_path / "second.bfscene"
    write_scene(first, scene)
    write_scene(second, scene)
    assert first.read_bytes() == second.read_bytes()
    loaded = load_scene(first)
    assert loaded.version == 7
    assert loaded.content_hash == scene.content_hash
    assert [unit.unit_id for unit in loaded.units] == [1, 2]
    assert isinstance(loaded.units[0].shape, AABBShape)
    assert isinstance(loaded.units[1].shape, CapsuleShape)


def test_content_hash_changes_for_visibility_update():
    scene = _example_scene()
    unit = scene.units[0]
    hidden = SceneUnit(unit.unit_id, unit.name, unit.shape, unit.transform, False)
    updated = scene.with_updates([hidden])
    assert updated.version == scene.version + 1
    assert updated.content_hash != scene.content_hash


def _rewrite_checksum(path):
    data = bytearray(path.read_bytes())
    body = bytes(data[:-32])
    data[-32:] = hashlib.sha256(body).digest()
    path.write_bytes(bytes(data))


def test_degenerate_aabb_reports_unit_id(tmp_path):
    path = tmp_path / "bad.bfscene"
    write_scene(path, _example_scene())
    data = bytearray(path.read_bytes())
    geometry_offset = 28 + 8 + 16 * 8
    # First unit in the file is unit id 1 (AABB).  Force max.x == min.x.
    struct.pack_into("<d", data, geometry_offset + 24, struct.unpack_from("<d", data, geometry_offset)[0])
    path.write_bytes(bytes(data))
    _rewrite_checksum(path)
    with pytest.raises(TacticalError) as exc_info:
        load_scene(path)
    assert exc_info.value.code == ErrorCode.SCENE_INVALID
    assert exc_info.value.details["unit_id"] == 1


def test_non_invertible_transform_reports_unit_id(tmp_path):
    path = tmp_path / "bad_transform.bfscene"
    write_scene(path, _example_scene())
    data = bytearray(path.read_bytes())
    transform_offset = 28 + 8
    for index in range(16):
        struct.pack_into("<d", data, transform_offset + index * 8, 0.0)
    path.write_bytes(bytes(data))
    _rewrite_checksum(path)
    with pytest.raises(TacticalError) as exc_info:
        load_scene(path)
    assert exc_info.value.code == ErrorCode.SCENE_INVALID
    assert exc_info.value.details["unit_id"] == 1


def test_missing_version_is_invalid(tmp_path):
    path = tmp_path / "bad_version.bfscene"
    write_scene(path, _example_scene())
    data = bytearray(path.read_bytes())
    struct.pack_into("<Q", data, 12, 0)
    path.write_bytes(bytes(data))
    _rewrite_checksum(path)
    with pytest.raises(TacticalError) as exc_info:
        load_scene(path)
    assert exc_info.value.code == ErrorCode.SCENE_INVALID
    assert "version" in exc_info.value.message


def test_checksum_mismatch_is_invalid(tmp_path):
    path = tmp_path / "bad_checksum.bfscene"
    write_scene(path, _example_scene())
    data = bytearray(path.read_bytes())
    data[-1] ^= 0xFF
    path.write_bytes(bytes(data))
    with pytest.raises(TacticalError) as exc_info:
        load_scene(path)
    assert exc_info.value.code == ErrorCode.SCENE_INVALID
