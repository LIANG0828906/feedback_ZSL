from __future__ import annotations

import time

import pytest

from inverse_imaging.engine import FittingConfig, FittingSession
from inverse_imaging.errors import E_BUSY, E_STALE_HANDLE, AppError
from inverse_imaging.params import GROUPS


def _config(directory, steps=2):
    return FittingConfig(
        steps=steps,
        learning_rate=0.01,
        active_groups=GROUPS,
        chunk_size=3,
        parallel=1,
        verify_incremental=False,
        checkpoint_dir=str(directory),
    )


def test_duplicate_start_returns_busy(scene, tmp_path) -> None:
    session = FittingSession(scene, _config(tmp_path, steps=30))
    session.start()
    try:
        with pytest.raises(AppError) as error:
            session.start()
        assert error.value.code == E_BUSY
    finally:
        session.cancel()
        session.join(timeout=30)


def test_scene_lease_returns_busy(scene, tmp_path) -> None:
    first = FittingSession(scene, _config(tmp_path / "a", steps=20))
    second = FittingSession(scene, _config(tmp_path / "b", steps=1))
    first.start()
    try:
        with pytest.raises(AppError) as error:
            second.start()
        assert error.value.code == E_BUSY
    finally:
        first.cancel()
        first.join(timeout=30)


def test_old_snapshot_handle_is_stale(scene, tmp_path) -> None:
    checkpoint_dir = tmp_path / "snapshots"
    first_session = FittingSession(scene, _config(checkpoint_dir, steps=1))
    first_session.run_sync()
    old_handle = first_session.query().handle
    second_session = FittingSession(scene, _config(checkpoint_dir, steps=2))
    second_session.run_sync()
    with pytest.raises(AppError) as error:
        second_session.query(old_handle)
    assert error.value.code == E_STALE_HANDLE
    current = second_session.query()
    assert current.step == 1


def test_cancel_is_cooperative_and_publishes_only_committed_checkpoints(scene, tmp_path) -> None:
    checkpoint_dir = tmp_path / "cancel"
    session = FittingSession(scene, _config(checkpoint_dir, steps=50))
    session.start()
    deadline = time.monotonic() + 15.0
    while session.latest_snapshot is None and time.monotonic() < deadline:
        time.sleep(0.02)
    session.cancel()
    outcome = session.join(timeout=30)
    assert outcome is not None
    assert outcome.status in ("cancelled", "completed")
    latest = session.latest_snapshot
    assert latest is not None
    assert (checkpoint_dir / "checkpoints" / f"checkpoint-{latest.step:08d}.json").exists()

