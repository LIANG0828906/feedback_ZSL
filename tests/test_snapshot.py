"""Requirement 6: session snapshots and cross-session reproduction."""

import json
import os
import threading

import numpy as np
import pytest

from conftest import copy_volume
from volview.model import VolumeRegistry
from volview.session import ReadingSession
from volview.snapshot import SNAPSHOT_SCHEMA_VERSION, SnapshotStore


def _build_session(volume_dir, store, session_id):
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, session_id)
    session.set_window(1200.0, 400.0)
    session.set_plane("axial", 50.0)
    session.set_thickness(5.0)
    session.add_annotation([22.0, 32.0, 50.0], [42.0, 32.0, 50.0])
    session.add_annotation([32.0, 12.0, 50.0], [32.0, 52.0, 50.0])
    session.commit()
    return session


def test_cross_session_reproduction(volume_dir, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    first = _build_session(volume_dir, store, "sess-a")
    first_image = first.render()
    first_readings = [a.reading() for a in first.annotations]

    # independent session, fresh registry and store object
    reopened = ReadingSession.open(VolumeRegistry(), volume_dir,
                                   SnapshotStore(str(tmp_path / "snap")),
                                   "sess-a")
    assert reopened.restored
    assert reopened.window_width == first.window_width
    assert reopened.plane == first.plane
    assert reopened.thickness == first.thickness
    np.testing.assert_array_equal(reopened.render(), first_image)
    assert [a.reading() for a in reopened.annotations] == first_readings


def test_content_hash_mismatch_discards_snapshot(volume_dir, samples_dir,
                                                 tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _build_session(volume_dir, store, "sess-b")
    # tamper the volume: change calibration intercept -> new content hash
    mutated = copy_volume(samples_dir, str(tmp_path / "mutated"))
    manifest_path = os.path.join(mutated, "manifest.json")
    with open(manifest_path, encoding="utf-8") as fh:
        manifest = json.load(fh)
    manifest["intercept"] = -1000.0
    with open(manifest_path, "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
    # copy the snapshot under the mutated volume id is unnecessary: the hash
    # check happens against the loaded model, so open with the same id.
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "sess-b")
    assert session.restored  # sanity: untampered volume restores
    # now point a session at the mutated volume with a snapshot written for
    # the *mutated* id but stale hash
    store2 = SnapshotStore(str(tmp_path / "snap2"))
    stale = _build_session(volume_dir, store2, "sess-c")
    snapshot_path = os.path.join(store2.directory, "snapshot_sess-c.json")
    with open(snapshot_path, encoding="utf-8") as fh:
        snapshot = json.load(fh)
    snapshot["volume_id"] = "vol_a"
    with open(snapshot_path, "w", encoding="utf-8") as fh:
        json.dump(snapshot, fh, indent=2, sort_keys=True)
    # mutate the volume after the snapshot was taken
    _ = stale
    mutated_session = ReadingSession.open(VolumeRegistry(), mutated, store2,
                                          "sess-c")
    assert not mutated_session.restored  # hash mismatch -> default session
    assert mutated_session.thickness == 0.0
    assert mutated_session.annotations == []


def test_schema_version_mismatch_discards_snapshot(volume_dir, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _build_session(volume_dir, store, "sess-d")
    path = os.path.join(store.directory, "snapshot_sess-d.json")
    with open(path, encoding="utf-8") as fh:
        snapshot = json.load(fh)
    snapshot["schema_version"] = SNAPSHOT_SCHEMA_VERSION + 1
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(snapshot, fh, indent=2, sort_keys=True)
    session = ReadingSession.open(VolumeRegistry(), volume_dir,
                                  SnapshotStore(str(tmp_path / "snap")),
                                  "sess-d")
    assert not session.restored
    assert session.plane == "axial"  # rebuilt default
    assert session.thickness == 0.0


def test_interp_order_mismatch_discards_snapshot(volume_dir, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _build_session(volume_dir, store, "sess-e")  # default order 1
    session = ReadingSession.open(VolumeRegistry(), volume_dir,
                                  SnapshotStore(str(tmp_path / "snap")),
                                  "sess-e", interp_order=0)
    assert not session.restored
    assert session.interp_order == 0


def test_no_partial_reuse_of_stale_params(volume_dir, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    _build_session(volume_dir, store, "sess-f")
    path = os.path.join(store.directory, "snapshot_sess-f.json")
    with open(path, encoding="utf-8") as fh:
        snapshot = json.load(fh)
    snapshot["content_hash"] = "0" * 64  # corrupt exactly one field
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(snapshot, fh, indent=2, sort_keys=True)
    session = ReadingSession.open(VolumeRegistry(), volume_dir,
                                  SnapshotStore(str(tmp_path / "snap")),
                                  "sess-f")
    assert not session.restored
    # every parameter falls back to the default, none is reused
    assert session.window_width != 1200.0
    assert session.annotations == []


def test_snapshot_writes_are_serial_and_ordered(volume_dir, tmp_path):
    store = SnapshotStore(str(tmp_path / "snap"))
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "sess-g")
    sequences = [store.allocate_sequence() for _ in range(6)]
    images = [session.render() for _ in sequences]

    def commit(seq, image):
        store.save_ordered(seq, "sess-g", session.snapshot_dict(), image)

    threads = [threading.Thread(target=commit, args=(seq, img))
               for seq, img in reversed(list(zip(sequences, images)))]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    history = store.history()
    assert [h["sequence"] for h in history] == sorted(sequences)


def test_chunked_render_matches_full_render(volume_dir, store):
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "sess-h")
    session.set_thickness(5.0)
    full = session.render()
    chunked = session.render(max_task_pixels=512)
    np.testing.assert_array_equal(full, chunked)
