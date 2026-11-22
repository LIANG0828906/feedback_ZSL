from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.compare import compare_snapshots
from terrain_pipeline.errors import E_CACHE, PipelineError
from terrain_pipeline.pipeline import inspect_cache
from terrain_pipeline.util import read_json


def test_failed_tile_is_retryable_and_does_not_publish_snapshot(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    first = Pipeline(project_dir, cache, params=params).run(fault_tiles={"T0002"})
    assert not first.success
    assert first.error is not None and first.error["code"] == "E_BAKE"
    assert not (cache / "snapshot.json").exists()
    pending = read_json(cache / "pending.json")
    assert "T0002" not in pending["committed"]
    assert pending["committed"]
    with pytest.raises(PipelineError) as caught:
        inspect_cache(cache)
    assert caught.value.code == E_CACHE

    recovered_pipeline = Pipeline(project_dir, cache, params=params)
    assert len(recovered_pipeline._startup_recovered) == 5
    retry = recovered_pipeline.run()
    assert retry.success, retry.error
    assert retry.recovered
    assert retry.rebuilt == ["T0002"]

    clean_cache = tmp_path / "clean"
    assert Pipeline(project_dir, clean_cache, params=params).run(mode="full").success
    assert compare_snapshots(cache, clean_cache, raise_on_diff=False)["equal"] is True


def test_global_content_hash_mismatch_invalidates_whole_snapshot(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    first = Pipeline(project_dir, cache, params=params).run(mode="full")
    assert first.success
    old_snapshot = read_json(cache / "snapshot.json")
    old_keys = {entry["cache_key"] for entry in old_snapshot["tiles"]}

    height_path = project_dir / "tiles" / "T0000" / "height.npy"
    height = np.load(height_path)
    height[1, 1] += 0.5
    np.save(height_path, height)

    reopened = Pipeline(project_dir, cache, params=params)
    assert reopened.snapshot is None
    assert reopened.startup_reason == "global_content_hash_mismatch"
    assert not (cache / "snapshot.json").exists()
    assert not any((cache / "blocks" / key).exists() for key in old_keys)
    rebuilt = reopened.run()
    assert rebuilt.success, rebuilt.error
    assert set(rebuilt.rebuilt) == set(reopened.project.tiles)


def test_same_process_retry_reuses_committed_blocks(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    pipeline = Pipeline(project_dir, cache, params=params)
    first = pipeline.run(fault_tiles={"T0001"})
    assert not first.success
    second = pipeline.run()
    assert second.success, second.error
    assert second.rebuilt == ["T0001"]
    assert second.recovered
    assert not (cache / "pending.json").exists()


def test_forced_process_termination_then_restart_recovers(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    child_code = f"""
from terrain_pipeline import Pipeline, BakeParams
pipeline = Pipeline(
    {str(project_dir)!r},
    {str(cache)!r},
    params=BakeParams(subdivisions=2, max_workers=2),
)
result = pipeline.run(mode="full", fault_tiles={{"T0002"}})
if result.success:
    raise SystemExit(1)
import os
os._exit(23)
"""
    child = subprocess.run(
        [sys.executable, "-c", child_code],
        cwd=Path(__file__).resolve().parents[1],
        capture_output=True,
        text=True,
        check=False,
    )
    assert child.returncode == 23, child.stderr
    assert not (cache / "snapshot.json").exists()
    pending = read_json(cache / "pending.json")
    assert len(pending["committed"]) == 5

    resumed_pipeline = Pipeline(project_dir, cache, params=params)
    assert len(resumed_pipeline._startup_recovered) == 5
    resumed = resumed_pipeline.run()
    assert resumed.success, resumed.error
    assert resumed.rebuilt == ["T0002"]

    clean_cache = tmp_path / "clean"
    assert Pipeline(project_dir, clean_cache, params=params).run(mode="full").success
    assert compare_snapshots(cache, clean_cache, raise_on_diff=False)["equal"] is True


def test_bake_version_mismatch_invalidates_whole_snapshot(
    project_dir: Path,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    assert (
        Pipeline(
            project_dir,
            cache,
            params=BakeParams(subdivisions=2, max_workers=2),
        )
        .run(mode="full")
        .success
    )
    reopened = Pipeline(
        project_dir,
        cache,
        params=BakeParams(subdivisions=3, max_workers=2),
    )
    assert reopened.snapshot is None
    assert reopened.startup_reason == "bake_version_mismatch"
    result = reopened.run()
    assert result.success, result.error
    assert set(result.rebuilt) == set(reopened.project.tiles)


def test_cancel_keeps_no_complete_snapshot_and_can_be_resumed(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    from terrain_pipeline.bake import CancellationToken

    cache = tmp_path / "cache"
    token = CancellationToken()
    token.cancel()
    result = Pipeline(project_dir, cache, params=params).run(cancel_token=token)
    assert not result.success
    assert result.error is not None and result.error["code"] == "E_CANCEL"
    assert not (cache / "snapshot.json").exists()

    resumed = Pipeline(project_dir, cache, params=params).run()
    assert resumed.success, resumed.error
