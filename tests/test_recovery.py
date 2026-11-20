from __future__ import annotations

import json

import pytest

from atlas_pipeline.errors import AtlasError, ErrorCode
from atlas_pipeline.pipeline import AtlasPipeline


def test_pending_snapshot_resumes_only_missing_tiles(pipeline, tmp_path):
    job = pipeline.begin_submission(
        config={
            "target_color_space": "display-p3",
            "target_transfer": "linear",
        }
    )
    assert job.step_tile()
    completed = tuple(job.completed_tiles)
    assert completed
    snapshot = tmp_path / "pending.snapshot.json"
    pipeline.save_snapshot(snapshot)

    recovered = AtlasPipeline.from_snapshot(snapshot)
    report = recovered.recovery_report
    assert report["resumed_pending"] is True
    assert recovered.current_version_number == 2
    assert report["reused_completed_tiles"] == list(completed)
    assert len(report["recomputed_tiles"]) == 4 - len(completed)
    assert recovered.recompute_full_check().exact_hash_match


def test_automatic_checkpoint_tracks_and_clears_cancelled_pending_job(
    pipeline,
    tmp_path,
):
    checkpoint = tmp_path / "auto.snapshot.json"
    pipeline.set_checkpoint_path(checkpoint)
    job = pipeline.begin_submission(
        config={
            "target_color_space": "display-p3",
            "target_transfer": "linear",
        }
    )
    assert checkpoint.is_file()
    assert job.step_tile()
    document = json.loads(checkpoint.read_text(encoding="ascii"))
    assert document["pending"] is not None
    assert document["pending"]["completed_tiles"]
    job.cancel()
    with pytest.raises(AtlasError) as caught:
        job.run_to_completion()
    assert caught.value.code == ErrorCode.E_CANCELLED
    document = json.loads(checkpoint.read_text(encoding="ascii"))
    assert document["pending"] is None
    assert document["current_version"] == 1


def test_broken_hash_chain_is_rejected(pipeline, tmp_path):
    pipeline.submit(tile_updates={1: {"exposure_ev": 0.2, "tone_curve": "reinhard"}})
    snapshot = tmp_path / "chain.snapshot.json"
    pipeline.save_snapshot(snapshot)
    document = json.loads(snapshot.read_text(encoding="ascii"))
    document["versions"][1]["chain_hash"] = "0" * 64
    snapshot.write_text(json.dumps(document, sort_keys=True), encoding="ascii")
    with pytest.raises(AtlasError) as caught:
        AtlasPipeline.from_snapshot(snapshot)
    assert caught.value.code == ErrorCode.E_RECOVERY_INVALID
    assert "chain_hash" in (caught.value.field or "")


def test_missing_version_field_is_rejected(pipeline, tmp_path):
    snapshot = tmp_path / "missing.snapshot.json"
    pipeline.save_snapshot(snapshot)
    document = json.loads(snapshot.read_text(encoding="ascii"))
    del document["versions"][0]["content_hash"]
    snapshot.write_text(json.dumps(document, sort_keys=True), encoding="ascii")
    with pytest.raises(AtlasError) as caught:
        AtlasPipeline.from_snapshot(snapshot)
    assert caught.value.code == ErrorCode.E_RECOVERY_INVALID
    assert caught.value.field is not None
    assert "content_hash" in caught.value.field


def test_corrupt_cached_tile_bytes_are_rejected(pipeline, tmp_path):
    snapshot = tmp_path / "tile.snapshot.json"
    pipeline.save_snapshot(snapshot)
    document = json.loads(snapshot.read_text(encoding="ascii"))
    key = sorted(document["cache"])[0]
    document["cache"][key]["data"] = "eJxjYGBgAAAABQAB"
    snapshot.write_text(json.dumps(document, sort_keys=True), encoding="ascii")
    with pytest.raises(AtlasError) as caught:
        AtlasPipeline.from_snapshot(snapshot)
    assert caught.value.code == ErrorCode.E_RECOVERY_INVALID
    assert (caught.value.field or "").startswith("cache.")
