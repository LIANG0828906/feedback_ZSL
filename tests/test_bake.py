from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.compare import compare_snapshots
from terrain_pipeline.util import read_json


def test_full_bake_produces_expected_artifacts_and_truth(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    cache = tmp_path / "cache"
    result = Pipeline(project_dir, cache, params=params).run()
    assert result.success, result.error
    snapshot = read_json(cache / "snapshot.json")
    assert snapshot["tile_count"] == 6
    assert snapshot["artifact_order"] == [
        "positions.bin",
        "normals.bin",
        "indices.bin",
        "weights.bin",
        "instances.bin",
        "bounds.bin",
    ]
    for entry in snapshot["tiles"]:
        block_dir = cache / entry["block_dir"]
        block = read_json(block_dir / "block.json")
        mesh_side = 8 * 2 + 1
        vertices = mesh_side * mesh_side
        triangles = (mesh_side - 1) * (mesh_side - 1) * 2
        assert entry["vertex_count"] == vertices
        assert (block_dir / "positions.bin").stat().st_size == vertices * 3 * 4
        assert (block_dir / "normals.bin").stat().st_size == vertices * 3 * 4
        assert (block_dir / "indices.bin").stat().st_size == triangles * 3 * 4
        assert (block_dir / "weights.bin").stat().st_size == vertices * 4
        assert (block_dir / "bounds.bin").stat().st_size == 6 * 4
        assert block["truth_check"]["ok"] is True
        assert block["truth_check"]["height_max_abs_error"] <= 1e-4
        assert block["truth_check"]["coverage_max_abs_error"] <= 0.02
        assert block["truth_check"]["scatter_actual"] == entry["instance_count"]
        assert "timestamp" not in json.dumps(block).lower()
        assert "thread" not in json.dumps(block).lower()
        normals = np.frombuffer((block_dir / "normals.bin").read_bytes(), dtype="<f4").reshape(
            -1, 3
        )
        lengths = np.linalg.norm(normals.astype(np.float64), axis=1)
        assert np.allclose(lengths, 1.0, atol=2e-6)


def test_repeated_full_bakes_are_byte_identical(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    left = tmp_path / "left"
    right = tmp_path / "right"
    first = Pipeline(project_dir, left, params=params).run(mode="full")
    second = Pipeline(project_dir, right, params=params).run(mode="full")
    assert first.success and second.success
    assert compare_snapshots(left, right, raise_on_diff=False)["equal"] is True


def test_default_params_use_manifest_subdivisions(project_dir: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    result = Pipeline(project_dir, cache).run()
    assert result.success, result.error
    snapshot = read_json(cache / "snapshot.json")
    entry = snapshot["tiles"][0]
    block = read_json(cache / entry["block_dir"] / "block.json")
    assert block["mesh"]["subdivisions"] == 2


def test_vertex_limit_returns_resource_error(project_dir: Path, tmp_path: Path) -> None:
    params = BakeParams(subdivisions=2, max_vertices_per_thread=64, max_workers=2)
    result = Pipeline(project_dir, tmp_path / "cache", params=params).run()
    assert not result.success
    assert result.error is not None
    assert result.error["code"] == "E_RESOURCE"
    assert not (tmp_path / "cache" / "snapshot.json").exists()


def test_truth_mismatch_returns_truth_error(
    project_dir: Path, params: BakeParams, tmp_path: Path
) -> None:
    truth_path = project_dir / "tiles" / "T0003" / "truth.json"
    truth = json.loads(truth_path.read_text(encoding="utf-8"))
    truth["height_samples"][0]["height"] += 2.0
    truth_path.write_text(json.dumps(truth, indent=2) + "\n", encoding="utf-8")
    result = Pipeline(project_dir, tmp_path / "cache", params=params).run()
    assert not result.success
    assert result.error is not None
    assert result.error["code"] == "E_TRUTH"
    assert result.error["tile_id"] == "T0003"
