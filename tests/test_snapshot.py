from __future__ import annotations

import json

import numpy as np
import pytest

from dustrender.chunks import build_chunk_table
from dustrender.config import RenderConfig
from dustrender.errors import E_RECOVERY_INVALID, DustRenderError
from dustrender.scene import Scene
from dustrender.scheduler import RenderScheduler
from dustrender.snapshot import find_latest_valid_snapshot, load_snapshot, save_snapshot


def _config() -> RenderConfig:
    return RenderConfig.from_dict(
        {
            "chunk_width": 12,
            "chunk_height": 9,
            "parallelism": 2,
            "task_budget": 1000,
            "base_step": 0.12,
            "max_step": 0.5,
            "min_step": 0.04,
            "shadow_samples": 3,
            "seam_validation": False,
            "compare_precision": False,
        }
    )


def test_atomic_snapshot_resume_matches_full_render(smoke_path, tmp_path) -> None:
    scene = Scene.load(smoke_path)
    config = _config()
    chunks = build_chunk_table(scene.width, scene.height, 12, 9)
    path = tmp_path / "snapshot.json"
    scheduler = RenderScheduler()

    def progress(results, statuses, step) -> None:
        save_snapshot(path, scene, config, chunks, results, statuses, effective_step=step)
        if len(results) >= 2:
            scheduler.request_cancel()

    partial = scheduler.render(scene, config, on_progress=progress)
    assert partial.error is not None
    loaded = load_snapshot(path, scene, config)
    assert loaded.completed_indices == [0, 1]
    resumed = RenderScheduler().render(
        scene, config, preloaded=loaded.results, preloaded_status="recovered"
    )
    full = RenderScheduler().render(scene, config)
    assert resumed.error is None
    assert np.array_equal(resumed.image, full.image)
    assert resumed.statuses["c0000"] == "recovered"


def test_snapshot_chain_corruption_is_recovery_error(smoke_path, tmp_path) -> None:
    scene = Scene.load(smoke_path)
    config = _config()
    chunks = build_chunk_table(scene.width, scene.height, 12, 9)
    run = RenderScheduler().render(scene, config)
    path = tmp_path / "snapshot.json"
    save_snapshot(path, scene, config, chunks, run.results, run.statuses)
    document = json.loads(path.read_text(encoding="utf-8"))
    document["hash_chain"][0] = "00" * 32
    corrupt = tmp_path / "corrupt.json"
    corrupt.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(DustRenderError) as caught:
        load_snapshot(corrupt, scene, config)
    assert caught.value.code == E_RECOVERY_INVALID


def test_snapshot_missing_field_has_path(smoke_path, tmp_path) -> None:
    scene = Scene.load(smoke_path)
    config = _config()
    chunks = build_chunk_table(scene.width, scene.height, 12, 9)
    run = RenderScheduler().render(scene, config)
    path = tmp_path / "snapshot.json"
    save_snapshot(path, scene, config, chunks, run.results, run.statuses)
    document = json.loads(path.read_text(encoding="utf-8"))
    del document["chunk_table"]
    path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(DustRenderError) as caught:
        load_snapshot(path, scene, config)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.field_path.startswith("$")


def test_latest_valid_snapshot_skips_corrupt_candidate(smoke_path, tmp_path) -> None:
    scene = Scene.load(smoke_path)
    config = _config()
    chunks = build_chunk_table(scene.width, scene.height, 12, 9)
    run = RenderScheduler().render(scene, config)
    save_snapshot(tmp_path / "snapshot-a.json", scene, config, chunks, run.results, run.statuses)
    good = json.loads((tmp_path / "snapshot-a.json").read_text(encoding="utf-8"))
    (tmp_path / "snapshot-z.json").write_text(json.dumps(good | {"snapshot_hash": "00" * 32}))
    loaded = find_latest_valid_snapshot(tmp_path, scene, config)
    assert loaded.path.name == "snapshot-a.json"
