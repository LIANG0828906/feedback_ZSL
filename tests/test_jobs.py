from __future__ import annotations

import threading
import time

import pytest

from probe_baker.bake import BakeEngine
from probe_baker.errors import BakeCancelled, ErrorCode, ProbeError
from probe_baker.jobs import BakeJobManager
from probe_baker.publish import PublishedStore


class _SlowEngine(BakeEngine):
    def __init__(self) -> None:
        super().__init__()
        self.started = threading.Event()

    def bake(self, scene, config, *, previous=None, cancel_event=None, progress=None):
        self.started.set()
        while cancel_event is not None and not cancel_event.is_set():
            time.sleep(0.01)
        raise BakeCancelled()


def test_duplicate_start_returns_busy_and_cancel_preserves_publication(
    room_scene, small_config
) -> None:
    initial_outcome = BakeEngine().bake(room_scene, small_config)
    publisher = PublishedStore()
    initial = publisher.publish(initial_outcome.state, version=1)
    engine = _SlowEngine()
    manager = BakeJobManager(
        room_scene.scene_id,
        engine=engine,
        publisher=publisher,
    )
    second_manager = BakeJobManager(
        room_scene.scene_id,
        engine=engine,
        publisher=publisher,
    )
    manager.seed_committed_state(initial_outcome.state, initial)
    try:
        manager.start(room_scene, small_config)
        assert engine.started.wait(timeout=2.0)
        with pytest.raises(ProbeError) as busy:
            manager.start(room_scene, small_config)
        assert busy.value.code == ErrorCode.BUSY
        with pytest.raises(ProbeError) as global_busy:
            second_manager.start(room_scene, small_config)
        assert global_busy.value.code == ErrorCode.BUSY
        assert manager.cancel() is True
        manager.wait_until_idle(timeout=5.0)
        status = manager.status()
        assert status.running is False
        assert status.cancelled is True
        assert publisher.pin().version == 1
        assert publisher.pin().content_hash == initial.content_hash
    finally:
        manager.shutdown()
        second_manager.shutdown()

