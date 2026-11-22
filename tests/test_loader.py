from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from terrain_pipeline.errors import E_DECODE, E_SCHEMA, PipelineError
from terrain_pipeline.loader import load_project

from .conftest import read_manifest, write_manifest


def test_loads_project_and_builds_content_hashes(project_dir: Path) -> None:
    project = load_project(project_dir)
    assert len(project.tiles) == 6
    assert len(project.global_content_hash) == 64
    hashes = [tile.content_hash for tile in project.tiles.values()]
    assert all(len(value) == 64 for value in hashes)
    assert len(set(hashes)) == len(hashes)
    first = project.tiles["T0000"]
    assert first.height.shape == (9, 9)
    assert first.weights.shape == (9, 9, 4)
    assert first.truth.height_samples
    assert project.global_content_hash == load_project(project_dir).global_content_hash


def test_missing_tile_field_returns_e_schema_with_tile_and_field(
    project_dir: Path,
) -> None:
    manifest = read_manifest(project_dir)
    del manifest["tiles"][0]["height_file"]
    write_manifest(project_dir, manifest)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    error = caught.value
    assert error.code == E_SCHEMA
    assert error.tile_id == "T0000"
    assert error.field and "height_file" in error.field


def test_declared_tile_size_mismatch_returns_e_schema(project_dir: Path) -> None:
    manifest = read_manifest(project_dir)
    manifest["tiles"][0]["height_shape"] = [8, 8]
    write_manifest(project_dir, manifest)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "height_shape"


def test_actual_tile_size_mismatch_returns_e_schema(project_dir: Path) -> None:
    path = project_dir / "tiles" / "T0000" / "height.npy"
    height = np.load(path)
    np.save(path, height[:-1, :-1])
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "height.shape"


def test_malformed_shape_field_keeps_tile_context(project_dir: Path) -> None:
    manifest = read_manifest(project_dir)
    manifest["tiles"][2]["height_shape"] = "bad"
    write_manifest(project_dir, manifest)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.tile_id == "T0002"
    assert caught.value.field == "tiles[2].height_shape"


def test_material_layer_count_mismatch_returns_e_schema(project_dir: Path) -> None:
    manifest = read_manifest(project_dir)
    manifest["materials"] = manifest["materials"][:3]
    write_manifest(project_dir, manifest)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "weights.shape"


def test_weight_out_of_range_returns_e_schema(project_dir: Path) -> None:
    path = project_dir / "tiles" / "T0000" / "weights.npy"
    weights = np.load(path).astype(np.int16)
    weights[1, 1, 0] = 300
    np.save(path, weights)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "weights"
    assert caught.value.details["max"] == 300


def test_corrupt_numpy_returns_e_decode(project_dir: Path) -> None:
    (project_dir / "tiles" / "T0000" / "height.npy").write_bytes(b"not-a-npy-file")
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_DECODE
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "height.file"


def test_corrupt_json_returns_e_decode(project_dir: Path) -> None:
    (project_dir / "tiles" / "T0000" / "truth.json").write_text("{broken", encoding="utf-8")
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_DECODE
    assert caught.value.tile_id == "T0000"
    assert caught.value.field == "truth"


def test_edits_change_hashes_and_can_be_reloaded(project_dir: Path, tmp_path: Path) -> None:
    from terrain_pipeline.loader import save_edits

    project = load_project(project_dir)
    before = project.global_content_hash
    project.apply_height_edit("T0004", 4, 4, 20.25)
    project.apply_material_edit("T0001", "rock", 2.0)
    after = project.global_content_hash
    assert after != before
    path = tmp_path / "edits.json"
    save_edits(project, path)
    reloaded = load_project(project_dir, path)
    assert reloaded.global_content_hash == after
    assert reloaded.tiles["T0004"].height[4, 4] == pytest.approx(20.25)
    assert reloaded.tiles["T0001"].material_params["rock"] == pytest.approx(2.0)


def test_missing_required_manifest_field_is_schema(project_dir: Path) -> None:
    manifest = read_manifest(project_dir)
    del manifest["seed"]
    write_manifest(project_dir, manifest)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.field == "seed"


def test_nan_height_is_schema(project_dir: Path) -> None:
    path = project_dir / "tiles" / "T0000" / "height.npy"
    height = np.load(path)
    height[0, 0] = np.nan
    np.save(path, height)
    with pytest.raises(PipelineError) as caught:
        load_project(project_dir)
    assert caught.value.code == E_SCHEMA
    assert caught.value.field == "height"
