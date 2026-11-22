from __future__ import annotations

import copy

import numpy as np
import pytest

from transparent_pipeline.checkpoint import CheckpointStore
from transparent_pipeline.engine import RenderEngine, SimulatedCrash
from transparent_pipeline.renderer import RenderConfig
from transparent_pipeline.resources import ResourceLimits
from transparent_pipeline.scene import Scene
from transparent_pipeline.comparison import compare_results


def config() -> RenderConfig:
    return RenderConfig(step_size=0.05, block_width=16, block_height=12)


def test_full_render_and_commit_log(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    outcome = engine.render(mode="full")
    assert outcome.complete is True
    assert outcome.progress == {"committed": 9, "total": 9, "failed": 0}
    assert outcome.commit_order == list(range(9))
    assert all(status == "committed" for status in outcome.statuses.values())
    events = CheckpointStore(tmp_path / "cp").read_commit_log()
    assert [event["sequence"] for event in events] == list(range(1, 10))
    assert [event["block_id"] for event in events] == list(range(9))
    assert all(event["scene_hash"] == scene.content_hash for event in events)
    assert all(event["algorithm_version"] == outcome.algorithm_version for event in events)


def test_crash_resume_is_bitwise_identical(scene, tmp_path):
    full = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "full").render(mode="full")
    crashing = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "resume")
    with pytest.raises(SimulatedCrash):
        crashing.render(mode="full", crash_after_commits=4)
    assert crashing.progress()["committed"] == 4
    snapshot = CheckpointStore(tmp_path / "resume").load()
    assert snapshot is not None and snapshot.committed_count == 4 and snapshot.complete is False
    resumed_engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "resume")
    resumed = resumed_engine.render(mode="resume")
    report = compare_results(resumed, full)
    assert report.equal, report.to_dict()
    assert resumed.resume["action"] == "resume"
    assert resumed.commit_order == list(range(9))


def test_resume_discards_incompatible_snapshot(scene, tmp_path):
    first = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp").render(mode="full")
    changed = scene.copy()
    medium = changed.media_by_id["glass_sphere"]
    medium["absorption"] = [value * 1.5 for value in medium["absorption"]]
    second_engine = RenderEngine(changed, config(), checkpoint_directory=tmp_path / "cp")
    second = second_engine.render(mode="resume")
    assert second.resume["action"] == "discard_full_recompute"
    assert "scene_hash" in second.resume["reasons"]
    assert second.complete is True
    assert len(second_engine.committed_ids()) == 9
    assert not np.array_equal(first.transmission, second.transmission)
    assert (tmp_path / "cp" / "rejected").exists()
    decisions = (tmp_path / "cp" / "decision_log.jsonl").read_text(encoding="utf-8")
    assert "discard_full" in decisions


def test_corrupt_checkpoint_falls_back_to_full(scene, tmp_path):
    RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp").render(mode="full")
    (tmp_path / "cp" / "checkpoint.npz").write_bytes(b"not an npz")
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    outcome = engine.render(mode="resume")
    assert outcome.resume["action"] == "discard_full_recompute"
    assert "E_DECODE" in outcome.resume["reasons"]
    assert outcome.complete is True


def test_block_failure_is_isolated_and_retryable(scene, tmp_path):
    full = RenderEngine(scene, config(), checkpoint_directory=None).render(mode="full")
    engine = RenderEngine(
        scene,
        config(),
        checkpoint_directory=tmp_path / "cp",
        limits=ResourceLimits(
            max_parallelism=2,
            per_thread_sample_buffer=128,
            intersection_cache_budget=32,
            max_pending_blocks=4,
        ),
    )

    def inject(block_id: int) -> None:
        if block_id == 4:
            raise RuntimeError("injected block failure")

    failed = engine.render(mode="full", failure_injector=inject)
    assert failed.complete is False
    assert failed.statuses[4] == "failed"
    assert all(failed.statuses[block] == "committed" for block in range(9) if block != 4)
    assert "RuntimeError" in failed.failed_blocks[4]
    for block_id, window in enumerate(full.block_windows):
        if block_id == 4:
            continue
        assert np.array_equal(
            failed.transmission[window.y0 : window.y1, window.x0 : window.x1],
            full.transmission[window.y0 : window.y1, window.x0 : window.x1],
        )
    retried = engine.render(mode="incremental")
    assert retried.complete is True
    assert compare_results(retried, full).equal
    assert retried.resource_stats["peak_cache_units"] <= 32
    assert retried.resource_stats["peak_sample_units"] <= 128
    assert retried.resource_stats["cache_hits"] > 0
    assert retried.resource_stats["cache_size"] <= 32


def test_pause_then_incremental_resume(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")

    def pause_on_first(block_id: int) -> None:
        if block_id == 0:
            engine.pause()

    paused = engine.render(mode="full", failure_injector=pause_on_first)
    assert paused.paused is True
    assert paused.complete is False
    assert paused.progress["committed"] < paused.progress["total"]
    resumed = engine.render(mode="incremental")
    assert resumed.complete is True


def test_edit_invalidation_reuses_unaffected_buffers(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp", sample_index=0)
    before = engine.render(mode="full")
    before_transmission = before.transmission.copy()
    before_scattering = before.scattering.copy()
    before_statuses = dict(before.statuses)
    entries = engine.edit_media("glass_sphere", {"absorption": [0.06, 0.05, 0.04]})
    assert entries
    assert any(entry["block_id"] == 4 for entry in entries)
    assert all(entry["block_id"] in {4, 5} for entry in entries)
    touched = {entry["block_id"] for entry in entries}
    assert len(touched) < 9
    assert all(entry["resource_kind"] == "medium" for entry in entries)
    assert all(entry["resource_id"] == "glass_sphere" for entry in entries)
    for block_id, window in enumerate(before.block_windows):
        if block_id in touched:
            assert engine.statuses()[block_id] == "pending_intersect"
        else:
            assert engine.statuses()[block_id] == before_statuses[block_id]
            assert np.array_equal(
                engine._transmission[window.y0 : window.y1, window.x0 : window.x1],
                before_transmission[window.y0 : window.y1, window.x0 : window.x1],
            )
            assert np.array_equal(
                engine._scattering[window.y0 : window.y1, window.x0 : window.x1],
                before_scattering[window.y0 : window.y1, window.x0 : window.x1],
            )
    after = engine.render(mode="incremental")
    reference = RenderEngine(engine.scene.copy(), config(), checkpoint_directory=None).render(mode="full")
    assert compare_results(after, reference).equal
    assert after.media_versions["glass_sphere"] == 2


def test_edit_validation_does_not_bump_version(scene):
    engine = RenderEngine(scene, config(), checkpoint_directory=None)
    with pytest.raises(Exception):
        engine.edit_media("glass_sphere", {"ior": [1.0, 1.5, 1.5]})
    assert engine.media_versions["glass_sphere"] == 1


def test_edit_restart_resumes_with_persisted_versions(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    original = engine.render(mode="full")
    engine.edit_media("glass_sphere", {"absorption": [0.061, 0.051, 0.041]})
    edited = engine.render(mode="incremental")
    reloaded_scene = Scene.from_dict(copy.deepcopy(engine.scene.data))
    resumed_engine = RenderEngine(reloaded_scene, config(), checkpoint_directory=tmp_path / "cp")
    assert resumed_engine.media_versions["glass_sphere"] == 2
    resumed = resumed_engine.render(mode="resume")
    assert resumed.resume["action"] == "resume"
    assert resumed.complete is True
    assert compare_results(resumed, edited).equal
    assert not compare_results(original, resumed).equal


def test_boundary_edit_includes_projection_candidates(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    engine.render(mode="full")
    old_boundary = scene.media_by_id["glass_sphere"]["boundary"]
    entries = engine.edit_media(
        "glass_sphere",
        {
            "boundary": {
                "type": "sphere",
                "center": old_boundary["center"],
                "radius": 1.15,
            }
        },
    )
    assert any(entry["reason"] == "geometry_projection_overlap" for entry in entries)
    assert len({entry["block_id"] for entry in entries}) < 9
    incremental = engine.render(mode="incremental")
    reference = RenderEngine(
        engine.scene.copy(),
        config(),
        checkpoint_directory=None,
    ).render(mode="full")
    assert compare_results(incremental, reference).equal


def test_light_edit_preserves_transmission_stage(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    before = engine.render(mode="full")
    before_transmission = before.transmission.copy()
    before_scattering = before.scattering.copy()
    entries = engine.edit_light("key_volume", {"radiance": [1.5, 1.4, 1.3]})
    assert entries and all(entry["stage"] == "scattering" for entry in entries)
    affected = {entry["block_id"] for entry in entries}
    for block_id, window in enumerate(before.block_windows):
        if block_id not in affected:
            continue
        assert engine.statuses()[block_id] == "pending_intersect"
        assert np.array_equal(
            engine._transmission[window.y0 : window.y1, window.x0 : window.x1],
            before_transmission[window.y0 : window.y1, window.x0 : window.x1],
        )
        assert np.all(engine._scattering[window.y0 : window.y1, window.x0 : window.x1] == 0.0)
    after = engine.render(mode="incremental")
    assert np.array_equal(after.transmission, before_transmission)
    assert not np.array_equal(after.scattering, before_scattering)


def test_restart_after_edit_reuses_unaffected_persisted_blocks(scene, tmp_path):
    engine = RenderEngine(scene, config(), checkpoint_directory=tmp_path / "cp")
    before = engine.render(mode="full")
    entries = engine.edit_media("glass_sphere", {"absorption": [0.061, 0.051, 0.041]})
    invalidated = {entry["block_id"] for entry in entries}
    snapshot = CheckpointStore(tmp_path / "cp").load()
    assert snapshot is not None
    assert snapshot.committed_count == 9 - len(invalidated)
    assert snapshot.media_versions["glass_sphere"] == 2
    reloaded = Scene.from_dict(copy.deepcopy(engine.scene.data))
    resumed_engine = RenderEngine(reloaded, config(), checkpoint_directory=tmp_path / "cp")
    resumed = resumed_engine.render(mode="resume")
    assert resumed.resume["action"] == "resume"
    assert resumed.complete is True
    for block_id, window in enumerate(before.block_windows):
        if block_id in invalidated:
            continue
        assert np.array_equal(
            resumed.transmission[window.y0 : window.y1, window.x0 : window.x1],
            before.transmission[window.y0 : window.y1, window.x0 : window.x1],
        )
    reference = RenderEngine(
        reloaded,
        config(),
        checkpoint_directory=None,
    ).render(mode="full")
    assert compare_results(resumed, reference).equal

