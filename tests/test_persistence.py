from __future__ import annotations

import copy

import pytest

import filter_studio.pipeline as pipeline_module
from filter_studio.pipeline import PipelineSession
from filter_studio.snapshot import SnapshotStore
from filter_studio.stages import DEFAULT_PARAMS, STAGE_VERSIONS, params_fingerprint


def test_complete_snapshot_restores_and_reuses_every_stage(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["LA"]
    snapshot = tmp_path / "restore.snapshot"
    first = PipelineSession(snapshot)
    first.set_image(sample_dir / sample["file"])
    first_run = first.run()
    assert first_run.status == "complete"
    assert snapshot.is_file()
    assert not list(tmp_path.glob(".*.tmp"))

    manifest = SnapshotStore(snapshot).peek_manifest()
    assert manifest is not None
    assert manifest["chain_complete"] is True
    assert manifest["completed_stages"] == ["denoise", "sharpen", "local_contrast"]

    restored = PipelineSession(snapshot)
    restore = restored.set_image(sample_dir / sample["file"])
    assert restore.status == "restored"
    assert len(restored.cache) == 3
    resumed = restored.run()
    assert resumed.status == "complete"
    assert resumed.diagnostics.recompute_count == 0
    assert resumed.diagnostics.cache_hit_count == 3
    assert resumed.diagnostics.path_type == "incremental"
    assert restored.compare_with_full().equal


def test_parameter_mismatch_discards_entire_snapshot(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["RGB"]
    snapshot = tmp_path / "params.snapshot"
    first = PipelineSession(snapshot)
    first.set_image(sample_dir / sample["file"])
    first.run()

    changed = copy.deepcopy(DEFAULT_PARAMS)
    changed["local_contrast"]["blend"] = 0.61
    restored = PipelineSession(snapshot, params=changed)
    restore = restored.set_image(sample_dir / sample["file"])
    assert restore.status == "discarded"
    assert "参数版本" in restore.reason
    assert len(restored.cache) == 0
    assert not snapshot.exists()
    rerun = restored.run()
    assert rerun.status == "complete"
    assert rerun.diagnostics.path_type == "full"
    assert rerun.diagnostics.recompute_count == 3


def test_stage_version_mismatch_discards_entire_snapshot(
    sample_bundle, sample_by_mode, tmp_path, monkeypatch
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["L"]
    snapshot = tmp_path / "version.snapshot"
    first = PipelineSession(snapshot)
    first.set_image(sample_dir / sample["file"])
    first.run()

    monkeypatch.setitem(pipeline_module.STAGE_VERSIONS, "denoise", 999)
    restored = PipelineSession(snapshot)
    restore = restored.set_image(sample_dir / sample["file"])
    assert restore.status == "discarded"
    assert "版本" in restore.reason
    assert len(restored.cache) == 0


def test_input_change_discards_old_snapshot_and_cache(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    snapshot = tmp_path / "input.snapshot"
    session = PipelineSession(snapshot)
    session.set_image(sample_dir / sample_by_mode["L"]["file"])
    session.run()
    old_hash = session.image.content_hash
    assert len(session.cache) == 3

    restore = session.set_image(sample_dir / sample_by_mode["RGBA"]["file"])
    assert restore.status == "none"
    assert session.image.content_hash != old_hash
    assert len(session.cache) == 0
    assert not snapshot.exists()


def test_unreadable_snapshot_is_discarded_without_partial_cache(
    sample_bundle, sample_by_mode, tmp_path
):
    sample_dir, _ = sample_bundle
    snapshot = tmp_path / "broken.snapshot"
    snapshot.write_bytes(b"not a zip snapshot")
    session = PipelineSession(snapshot)
    restore = session.set_image(sample_dir / sample_by_mode["L"]["file"])
    assert restore.status == "discarded"
    assert len(session.cache) == 0
    assert not snapshot.exists()


def test_snapshot_atomic_replace_preserves_previous_file(
    sample_bundle, sample_by_mode, tmp_path, monkeypatch
):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["L"]
    snapshot_path = tmp_path / "atomic.snapshot"
    session = PipelineSession(snapshot_path)
    session.set_image(sample_dir / sample["file"])
    session.run()
    before = snapshot_path.read_bytes()

    image = session.image
    entries = session.cache.valid_prefix()

    def fail_replace(source, destination):
        del source, destination
        raise OSError("simulated power loss before atomic replace")

    monkeypatch.setattr("filter_studio.snapshot.os.replace", fail_replace)
    store = SnapshotStore(snapshot_path)
    with pytest.raises(OSError):
        store.save(
            image_metadata=image.metadata(),
            params=session.params,
            params_fingerprint=params_fingerprint(session.params),
            stage_versions=STAGE_VERSIONS,
            entries=entries,
            chain_complete=True,
        )
    assert snapshot_path.read_bytes() == before
    assert not [path for path in tmp_path.iterdir() if path.name.endswith(".tmp")]


def test_store_removes_stale_temporary_snapshot(tmp_path):
    stale = tmp_path / ".workbench.snapshot.abandoned.tmp"
    stale.write_bytes(b"half-written")
    SnapshotStore(tmp_path / "workbench.snapshot")
    assert not stale.exists()

