"""Requirement 8: concurrent scheduling and resource boundaries."""

import numpy as np
import pytest

from volview.errors import ErrorCode, VolViewError
from volview.model import VolumeRegistry
from volview.resample import plane_shape, render_plane
from volview.scheduler import PlaneScheduler, PlaneTask
from volview.session import ReadingSession
from volview.snapshot import SnapshotStore


def _run_op_sequence(volume_dir, store, max_workers):
    """Same session op sequence; returns (images, history)."""
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "sched")
    session.set_window(1200.0, 400.0)
    ops = [("axial", 40.0), ("coronal", 30.0), ("sagittal", 32.0),
           ("axial", 50.0), ("coronal", 35.0)]
    images = []
    with PlaneScheduler(max_workers=max_workers,
                        max_inflight_pixels=1 << 16,
                        max_task_pixels=1 << 16) as scheduler:
        tasks = []
        for plane, pos in ops:
            rows, cols = plane_shape(session.model, plane)
            tasks.append(scheduler.submit(
                f"{plane}@{pos}", rows, cols,
                lambda p=plane, q=pos: render_plane(session.model, p, q,
                                                    1200.0, 400.0, 1)))
        for task, (plane, pos) in zip(tasks, ops):
            image = task.result()
            images.append(image)
            session.set_plane(plane, pos)
            session.commit()  # commits happen in op order
    return images, store.history()


def test_parallel_matches_serial(volume_dir, tmp_path):
    store_par = SnapshotStore(str(tmp_path / "par"))
    store_ser = SnapshotStore(str(tmp_path / "ser"))
    images_par, history_par = _run_op_sequence(volume_dir, store_par, 4)
    images_ser, history_ser = _run_op_sequence(volume_dir, store_ser, 1)
    for par, ser in zip(images_par, images_ser):
        np.testing.assert_array_equal(par, ser)
    # identical snapshot record order and content hashes
    assert [h["sequence"] for h in history_par] == \
           [h["sequence"] for h in history_ser]
    assert [h["plane_sha256"] for h in history_par] == \
           [h["plane_sha256"] for h in history_ser]


def test_failed_task_retry_and_isolation(model, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    session = ReadingSession.open(VolumeRegistry(), model.source_dir, store, "ok")
    session.commit()
    history_before = store.history()

    def bad_window():
        return render_plane(model, "axial", 50.0, 0.0, 300.0, 1)  # E_WIN

    def good():
        return render_plane(model, "axial", 50.0, 1500.0, 300.0, 1)

    with PlaneScheduler(max_workers=2) as scheduler:
        bad = scheduler.submit("bad", 64, 64, bad_window)
        ok = scheduler.submit("ok", 64, 64, good)
        with pytest.raises(VolViewError) as exc:
            bad.result()
        assert exc.value.code == ErrorCode.E_WIN
        assert bad.status == PlaneTask.FAILED
        ok.result()
        assert ok.status == PlaneTask.DONE
        # retry the failed task with a fixed function
        bad._fn = good
        bad.retry()
        np.testing.assert_array_equal(bad.result(), ok.result())
    # committed snapshots untouched by the failed task
    assert store.history() == history_before


def _tracked(model, completed, i):
    image = render_plane(model, "axial", 50.0, 1500.0, 300.0, 1)
    completed.append(i)
    return image


def test_pixel_budget_blocks_but_never_drops(model):
    rows, cols = plane_shape(model, "axial")
    plane_pixels = rows * cols
    completed = []
    # budget fits exactly one plane: tasks must serialise through the budget
    with PlaneScheduler(max_workers=4,
                        max_inflight_pixels=plane_pixels,
                        max_task_pixels=plane_pixels) as scheduler:
        tasks = [
            scheduler.submit(f"t{i}", rows, cols,
                             lambda i=i: _tracked(model, completed, i))
            for i in range(4)
        ]
        scheduler.wait_all()
        assert all(t.status == PlaneTask.DONE for t in tasks)
    assert sorted(completed) == [0, 1, 2, 3]
