from __future__ import annotations

import threading
import time
from dataclasses import replace

import pytest

from spectral_editor.color import WHITE_POINTS
from spectral_editor.errors import (
    BusinessError,
    E_BUSY,
    E_CANCELLED,
    E_SNAPSHOT_STALE,
)
from spectral_editor.models import TransitionRequest
from spectral_editor.runtime import RecomputeManager


def _new_config(library, name: str):
    return replace(
        library.current_snapshot.pipeline,
        white_point_name=name,
        white_point_xyz=WHITE_POINTS[name],
    )


def test_old_handle_remains_complete_after_publication(library):
    old_handle = library.acquire_snapshot()
    old_snapshot = old_handle.snapshot
    library.run_transition_sync(
        TransitionRequest("white-point", pipeline=_new_config(library, "D50"))
    )
    try:
        materials = old_handle.current_materials()
        assert old_snapshot.generation == 1
        assert len(materials) == 8
        assert all(item["version"] == 1 for item in materials.values())
    finally:
        old_handle.release()
    with pytest.raises(BusinessError) as caught:
        old_handle.current_materials()
    assert caught.value.code == E_SNAPSHOT_STALE


def test_fake_handle_is_rejected(library):
    with library.acquire_snapshot() as handle:
        snapshot = handle.snapshot
    # The released handle is stale even if a caller retains its snapshot object.
    with pytest.raises(BusinessError) as caught:
        handle.ensure_valid()
    assert caught.value.code == E_SNAPSHOT_STALE
    assert snapshot.generation == 1


def test_readers_never_observe_mixed_generations(library):
    errors: list[BaseException] = []
    stop = threading.Event()
    observations: list[tuple[int, str, int]] = []

    def reader() -> None:
        try:
            while not stop.is_set():
                with library.acquire_snapshot() as handle:
                    snapshot = handle.snapshot
                    materials = handle.current_materials()
                    assert len(materials) == 8
                    assert len({item["version"] for item in materials.values()}) >= 1
                    observations.append(
                        (
                            snapshot.generation,
                            snapshot.publication_hash,
                            len(materials),
                        )
                    )
                time.sleep(0)
        except BaseException as exc:  # pragma: no cover - surfaced below
            errors.append(exc)

    thread = threading.Thread(target=reader)
    thread.start()
    try:
        for index in range(8):
            name = "D50" if index % 2 == 0 else "D65"
            library.run_transition_sync(
                TransitionRequest(f"toggle-{index}", pipeline=_new_config(library, name))
            )
    finally:
        stop.set()
        thread.join(5)
    assert not errors
    assert observations
    assert library.max_observed_pointer_wait_ms < 25.0


def test_pre_cancelled_transition_keeps_published_snapshot(library):
    before = library.current_snapshot
    cancel = threading.Event()
    cancel.set()
    with pytest.raises(BusinessError) as caught:
        library.run_transition_sync(
            TransitionRequest("cancel", pipeline=_new_config(library, "D50")),
            cancel_event=cancel,
        )
    assert caught.value.code == E_CANCELLED
    after = library.current_snapshot
    assert after.generation == before.generation
    assert after.publication_hash == before.publication_hash


def test_recompute_manager_returns_busy_and_cancel_preserves_snapshot(
    library, monkeypatch
):
    entered = threading.Event()
    release = threading.Event()
    original = library.run_transition_sync

    def blocked_run(request, *, cancel_event=None, write_pending=True):
        entered.set()
        while not release.is_set():
            if cancel_event is not None and cancel_event.is_set():
                from spectral_editor.errors import cancelled

                raise cancelled()
            time.sleep(0.005)
        return original(request, cancel_event=cancel_event, write_pending=write_pending)

    monkeypatch.setattr(library, "run_transition_sync", blocked_run)
    manager = RecomputeManager(library)
    before = library.current_snapshot
    request = TransitionRequest("managed", pipeline=_new_config(library, "D50"))
    manager.submit(request)
    assert entered.wait(2)
    with pytest.raises(BusinessError) as caught:
        manager.submit(request)
    assert caught.value.code == E_BUSY
    assert manager.cancel()
    release.set()
    final = manager.wait(3)
    assert final.state == "cancelled"
    assert library.current_snapshot.generation == before.generation
    assert library.current_snapshot.publication_hash == before.publication_hash
