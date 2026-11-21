from __future__ import annotations

import threading
import time

import numpy as np
import pytest

from volrender.errors import AppError, ErrorCode
from volrender.incremental import (
    Change,
    VersionedRenderService,
    affected_tile_indices,
)

from tests.conftest import scene_from_header_payload


def _mutated_density_scene(sample_bundle):
    def mutate(payload):
        values = np.frombuffer(payload, dtype="<f4").copy()
        values[:100] = np.minimum(1.0, values[:100] + 0.2)
        payload[:] = values.tobytes()

    return scene_from_header_payload(
        sample_bundle["scene_path"], mutate_payload=mutate
    )


def test_incremental_reuses_unaffected_tiles(sample_bundle):
    loaded = sample_bundle["loaded"]
    changed_scene = _mutated_density_scene(sample_bundle)
    affected, reasons, conservative = affected_tile_indices(
        loaded.scene,
        changed_scene,
        loaded.config,
        loaded.config,
        Change("density", block_id=0),
    )
    assert affected == {0, 1, 3, 4}
    assert 2 not in affected and 5 not in affected
    assert conservative is False
    assert reasons

    service = VersionedRenderService()
    first = service.render_initial(loaded.scene, loaded.config)
    second = service.render_change(
        loaded.scene,
        changed_scene,
        loaded.config,
        loaded.config,
        Change("density", block_id=0, reason="smoke block update"),
    )
    assert second.version == first.version + 1
    assert second.publication["affected_tiles"] == [0, 1, 3, 4]
    assert second.publication["reused_tiles"] == [2, 5]
    for tile_index in second.publication["reused_tiles"]:
        assert (
            second.tile_hashes[tile_index]
            == first.tile_hashes[tile_index]
        )
    submitted = [
        event["tile_index"]
        for event in second.artifact.trace
        if event["event"] == "submit"
    ]
    assert submitted == [0, 1, 3, 4]
    published = service.query(loaded.scene.scene_id)
    assert published.version == 2
    assert published.artifact.color.shape == (
        loaded.scene.height,
        loaded.scene.width,
        3,
    )


def test_camera_or_light_change_is_conservatively_full(sample_bundle):
    loaded = sample_bundle["loaded"]
    scene = loaded.scene
    for change in (
        Change("camera"),
        Change("light"),
        Change("unknown"),
    ):
        affected, reasons, conservative = affected_tile_indices(
            scene, scene, loaded.config, loaded.config, change
        )
        assert affected == set(range(6))
        assert conservative is True
        assert reasons


def test_query_returns_old_or_new_complete_version(sample_bundle):
    loaded = sample_bundle["loaded"]
    changed_scene = _mutated_density_scene(sample_bundle)
    service = VersionedRenderService()
    first = service.render_initial(loaded.scene, loaded.config)
    errors: list[Exception] = []

    def worker():
        try:
            service.render_change(
                loaded.scene,
                changed_scene,
                loaded.config,
                loaded.config,
                Change("density", block_id=0),
            )
        except Exception as exc:
            errors.append(exc)

    thread = threading.Thread(target=worker)
    thread.start()
    deadline = time.time() + 5.0
    while time.time() < deadline:
        lock = service._locks.get(loaded.scene.scene_id)
        if lock is not None and lock.locked():
            break
        time.sleep(0.01)
    during = service.query(loaded.scene.scene_id)
    assert during.version == first.version
    assert during.artifact.color.shape == first.artifact.color.shape
    thread.join(timeout=30)
    assert not thread.is_alive()
    assert not errors
    after = service.query(loaded.scene.scene_id)
    assert after.version == 2
    assert after.artifact.color.shape == first.artifact.color.shape


def test_same_scene_concurrent_submit_returns_busy(sample_bundle):
    loaded = sample_bundle["loaded"]
    service = VersionedRenderService()
    lock = service._scene_lock(loaded.scene.scene_id)
    lock.acquire()
    try:
        with pytest.raises(AppError) as caught:
            service.render_initial(loaded.scene, loaded.config)
        assert caught.value.code == ErrorCode.E_BUSY
    finally:
        lock.release()
