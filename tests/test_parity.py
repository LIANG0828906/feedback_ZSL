from __future__ import annotations

from pathlib import Path

import pytest

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.compare import compare_snapshots
from terrain_pipeline.errors import E_DIFF, PipelineError
from terrain_pipeline.util import read_json


def test_incremental_and_full_paths_are_byte_identical(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    incremental_cache = tmp_path / "incremental"
    pipeline = Pipeline(project_dir, incremental_cache, params=params)
    baseline = pipeline.run(mode="full")
    assert baseline.success

    pipeline.apply_height_edit("T0004", 4, 4, 17.0)
    material_report = pipeline.apply_material_edit("T0001", "rock", 1.9)
    assert material_report.affected
    edits_path = tmp_path / "edits.json"
    pipeline.save_edits(edits_path)

    incremental = pipeline.run(mode="incremental")
    assert incremental.success, incremental.error
    assert incremental.reused
    assert incremental.rebuilt
    assert set(incremental.rebuilt) == set(incremental.invalidations.affected)

    full_cache = tmp_path / "full"
    full = Pipeline(
        project_dir,
        full_cache,
        params=BakeParams(subdivisions=2, max_workers=1),
        edits_file=edits_path,
    ).run(mode="full")
    assert full.success, full.error
    comparison = compare_snapshots(incremental_cache, full_cache, raise_on_diff=False)
    assert comparison["equal"] is True

    cache_hit = pipeline.run()
    assert cache_hit.success
    assert not cache_hit.rebuilt
    assert set(cache_hit.reused) == set(pipeline.project.tiles)
    assert cache_hit.invalidations.trigger[0]["kind"] == "cache_hit"


def test_compare_reports_first_differing_tile_and_artifact(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    left = tmp_path / "left"
    right = tmp_path / "right"
    assert Pipeline(project_dir, left, params=params).run(mode="full").success
    assert Pipeline(project_dir, right, params=params).run(mode="full").success
    snapshot = read_json(right / "snapshot.json")
    entry = snapshot["tiles"][0]
    artifact = right / entry["block_dir"] / "positions.bin"
    data = bytearray(artifact.read_bytes())
    data[0] ^= 0x01
    artifact.write_bytes(bytes(data))
    with pytest.raises(PipelineError) as caught:
        compare_snapshots(left, right)
    error = caught.value
    assert error.code == E_DIFF
    assert error.tile_id == entry["tile_id"]
    assert error.field == "positions.bin"


def test_incremental_edit_without_base_cache_falls_back_to_full(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    pipeline = Pipeline(project_dir, tmp_path / "cache", params=params)
    pipeline.apply_material_edit("T0004", "rock", 1.5)
    result = pipeline.run(mode="incremental")
    assert result.success, result.error
    assert set(result.rebuilt) == set(pipeline.project.tiles)
    assert result.invalidations.mode == "full"
    assert result.invalidations.trigger[0]["reason"] == "incomplete_incremental_base"
