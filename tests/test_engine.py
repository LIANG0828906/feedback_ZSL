import threading
import time

import pytest

from spectral_studio import samplegen
from spectral_studio.engine import MaterialLibrary
from spectral_studio.errors import ErrorCode, SpectralStudioError
from spectral_studio.versioning import diff_versions


def _library(tmp_path):
    library = MaterialLibrary(
        snapshot_path=str(tmp_path / "snapshot.json"))
    for doc in samplegen.generate_samples()[0]:
        library.add_material(doc)
    library.submit([], ui_state={"panel": "materials"})
    return library


def test_initial_publish_versions_monotonic(tmp_path):
    library = _library(tmp_path)
    release = library.current_release
    assert release.release_id == 1
    assert all(v.version == 1 for v in release.versions.values())
    assert len(release.versions) == 7


def test_whitepoint_change_affects_all(tmp_path):
    library = _library(tmp_path)
    release = library.submit([{"op": "set_env", "white_point": "D50"}])
    assert all(v.version == 2 for v in release.versions.values())
    entry = library.trace[-1]
    assert entry["recomputed_count"] == 7
    assert entry["reused_count"] == 0
    assert set(entry["affected_versions"]) == set(release.versions)


def test_single_material_edit_affects_only_that_material(tmp_path):
    library = _library(tmp_path)
    before = library.current_release
    doc = samplegen.generate_samples()[0][0]
    doc["spectral"]["reflectance"] = [min(1.0, v * 1.1)
                                      for v in doc["spectral"]["reflectance"]]
    release = library.submit([{"op": "set_material", "id": doc["id"],
                               "params": doc}])
    entry = library.trace[-1]
    assert entry["recomputed_count"] == 1
    assert entry["reused_count"] == 6
    assert release.versions[doc["id"]].version == 2
    for mid, version in release.versions.items():
        if mid == doc["id"]:
            continue
        # Reused versions keep identical cached hash and encoded values.
        assert version.content_hash == before.versions[mid].content_hash
        assert version.version == 1


def test_illuminant_change_affects_all(tmp_path):
    library = _library(tmp_path)
    release = library.submit([{"op": "set_env", "illuminant": "A"}])
    assert library.trace[-1]["recomputed_count"] == 7
    assert all("illuminant" in v.recompute_reason
               for v in release.versions.values())


def test_rollback_appends_new_version(tmp_path):
    library = _library(tmp_path)
    doc = samplegen.generate_samples()[0][0]
    edited = dict(doc["spectral"])
    edited["reflectance"] = [min(1.0, v * 1.1)
                             for v in edited["reflectance"]]
    library.submit([{"op": "set_material", "id": doc["id"],
                     "params": {"id": doc["id"], "kind": "spectral",
                                "spectral": edited}}])
    release = library.submit([{"op": "rollback", "id": doc["id"],
                               "version": 1}])
    version = release.versions[doc["id"]]
    assert version.version == 3
    assert version.rollback_from == 1
    history = library.history_of(doc["id"])
    assert [v.version for v in history] == [1, 2, 3]
    # Rolled-back params match the historical version 1 params.
    assert version.params == history[0].params
    # Subsequent edits build on the rolled-back version.
    release2 = library.submit([{"op": "set_env", "white_point": "D50"}])
    assert release2.versions[doc["id"]].prev_hash == version.content_hash
    assert release2.versions[doc["id"]].version == 4


def test_diff_versions(tmp_path):
    library = _library(tmp_path)
    library.submit([{"op": "set_env", "white_point": "D50"}])
    mid = sorted(library.current_release.versions)[0]
    history = library.history_of(mid)
    diff = diff_versions(history[0], history[1])
    assert diff["from_version"] == 1 and diff["to_version"] == 2
    assert "white_point" in diff["dep_factors"]
    assert "encoded" in diff


def test_busy_and_cancel(tmp_path):
    library = _library(tmp_path)
    started = threading.Event()
    finish = threading.Event()
    original = type(library)._apply

    def slow_apply(self, ops, ui_state):
        started.set()
        finish.wait(5.0)
        return original(self, ops, ui_state)

    type(library)._apply = slow_apply
    try:
        thread = threading.Thread(
            target=lambda: library.submit(
                [{"op": "set_env", "white_point": "D50"}]))
        thread.start()
        assert started.wait(2.0)
        with pytest.raises(SpectralStudioError) as exc:
            library.submit([{"op": "set_env", "white_point": "E"}])
        assert exc.value.code == ErrorCode.E_BUSY
    finally:
        finish.set()
        thread.join()
    assert library.current_release.release_id == 2


def test_cancel_keeps_last_published_release(tmp_path):
    library = _library(tmp_path)
    import spectral_studio.engine as engine_mod
    original_run = engine_mod.run_chain

    def cancelling_run(spec, env, caches):
        result = original_run(spec, env, caches)
        library.cancel()  # request cancel after the first version checkpoint
        return result

    engine_mod.run_chain = cancelling_run
    try:
        release = library.submit([{"op": "set_env", "white_point": "D50"}])
    finally:
        engine_mod.run_chain = original_run
    assert release.release_id == 1  # nothing new published
    assert library.trace[-1]["event"] == "cancelled"
    library._cancel_event.clear()
    release = library.submit([{"op": "set_env", "white_point": "D50"}])
    assert release.release_id == 2


def test_snapshot_handles_and_stale(tmp_path):
    library = _library(tmp_path)
    handle = library.acquire_snapshot()
    mid = sorted(library.current_release.versions)[0]
    data = library.read_material(handle, mid)
    assert data["version"] == 1
    library.release_snapshot(handle)
    with pytest.raises(SpectralStudioError) as exc:
        library.read_material(handle, mid)
    assert exc.value.code == ErrorCode.E_STALE_SNAPSHOT


def test_readers_observe_complete_releases_during_recompute(tmp_path):
    library = _library(tmp_path)
    gate = threading.Event()
    original_run = __import__("spectral_studio.engine",
                              fromlist=["run_chain"]).run_chain
    observed = []

    def gated_run(spec, env, caches):
        gate.wait(5.0)
        return original_run(spec, env, caches)

    import spectral_studio.engine as engine_mod
    engine_mod.run_chain = gated_run
    try:
        thread = threading.Thread(
            target=lambda: library.submit(
                [{"op": "set_env", "white_point": "D50"}]))
        thread.start()
        time.sleep(0.05)
        handle = library.acquire_snapshot()
        mid = sorted(library.current_release.versions)[0]
        data = library.read_material(handle, mid)
        observed.append((data["release_id"], data["version"]))
        library.release_snapshot(handle)
    finally:
        gate.set()
        thread.join()
        engine_mod.run_chain = original_run
    # Reader saw the complete previous release while recompute was running.
    assert observed == [(1, 1)]
    assert library.current_release.release_id == 2
