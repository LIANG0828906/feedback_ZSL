import json
import os
import subprocess
import sys

import numpy as np
import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO_ROOT)

from terrain_bake.chunks import build_grid, chunk_origin, iter_chunks, neighbors
from terrain_bake.heightfield import generate_heights
from terrain_bake.normals import (chunk_normals_local,
                                  chunk_normals_with_halo, global_normals,
                                  repair_seams)
from terrain_bake.pipeline import default_params, run_bake
from terrain_bake.texture import bake_texture
from terrain_bake.verify import compare_bakes

PARAMS = default_params(seed=20261118, chunk_verts=9, chunks_x=3, chunks_y=2)


def _grid():
    return build_grid(PARAMS["chunk_verts"], PARAMS["chunks_x"],
                      PARAMS["chunks_y"])


def _heights():
    grid = _grid()
    return generate_heights(PARAMS["seed"], grid.verts_x, grid.verts_y,
                            PARAMS["feature"], PARAMS["octaves"])


def test_heightfield_is_deterministic_and_seed_sensitive():
    grid = _grid()
    a = generate_heights(7, grid.verts_x, grid.verts_y, 8, 3)
    b = generate_heights(7, grid.verts_x, grid.verts_y, 8, 3)
    assert a.tobytes() == b.tobytes()
    c = generate_heights(8, grid.verts_x, grid.verts_y, 8, 3)
    assert c.tobytes() != a.tobytes()
    assert np.all((a >= 0.0) & (a < 1.0))


def test_chunk_tiling_covers_grid_without_gaps_or_overlap():
    grid = _grid()
    n = grid.chunk_verts
    counts = np.zeros((grid.verts_y, grid.verts_x), dtype=np.int64)
    for cx, cy in iter_chunks(grid):
        ox, oy = chunk_origin(grid, cx, cy)
        counts[oy:oy + n, ox:ox + n] += 1
    assert counts.min() >= 1  # no vertex omitted
    assert counts.max() <= 4  # shared edges/corners only, never duplicated data
    assert grid.chunk_count == 6
    assert grid.verts_x == 3 * (n - 1) + 1
    assert grid.verts_y == 2 * (n - 1) + 1
    corner = neighbors(grid, 0, 0)
    assert corner["west"] is None and corner["south"] is None
    assert corner["east"] == [1, 0] and corner["north"] == [0, 1]


def test_normals_are_unit_and_finite():
    grid = _grid()
    heights = _heights()
    step = PARAMS["world_size_x"] / (grid.verts_x - 1)
    normals = global_normals(heights, step, step)
    lengths = np.linalg.norm(normals, axis=-1)
    assert np.all(np.isfinite(normals))
    assert np.all(lengths > 0.0)
    np.testing.assert_allclose(lengths, 1.0, atol=1e-12)


def test_seam_repair_matches_global_and_shares_boundary_normals():
    grid = _grid()
    heights = _heights()
    n = grid.chunk_verts
    step = PARAMS["world_size_x"] / (grid.verts_x - 1)
    reference = global_normals(heights, step, step)
    total_repaired = 0
    repaired_chunks = {}
    for cx, cy in iter_chunks(grid):
        ox, oy = chunk_origin(grid, cx, cy)
        chunk_h = heights[oy:oy + n, ox:ox + n]
        local = chunk_normals_local(chunk_h, step, step)
        halo = chunk_normals_with_halo(heights, ox, oy, n, step, step)
        repaired, count = repair_seams(local, halo)
        total_repaired += count
        repaired_chunks[(cx, cy)] = repaired
        # Repaired chunk normals match the one-shot global computation.
        np.testing.assert_allclose(
            repaired, reference[oy:oy + n, ox:ox + n], atol=1e-6)
    assert total_repaired > 0
    # Shared boundary vertices are component-wise identical across chunks.
    for (cx, cy), normals in repaired_chunks.items():
        if cx + 1 < grid.chunks_x:
            east = repaired_chunks[(cx + 1, cy)]
            assert np.array_equal(normals[:, -1], east[:, 0])
        if cy + 1 < grid.chunks_y:
            north = repaired_chunks[(cx, cy + 1)]
            assert np.array_equal(normals[-1, :], north[0, :])


def test_texture_boundaries_have_no_brightness_jump():
    grid = _grid()
    heights = _heights()
    n = grid.chunk_verts
    step = PARAMS["world_size_x"] / (grid.verts_x - 1)
    textures = {}
    for cx, cy in iter_chunks(grid):
        ox, oy = chunk_origin(grid, cx, cy)
        halo = chunk_normals_with_halo(heights, ox, oy, n, step, step)
        textures[(cx, cy)] = bake_texture(halo)
    max_diff = 0
    for (cx, cy), tex in textures.items():
        if cx + 1 < grid.chunks_x:
            east = textures[(cx + 1, cy)]
            diff = np.abs(tex[:, -1].astype(int) - east[:, 0].astype(int))
            max_diff = max(max_diff, int(diff.max()))
        if cy + 1 < grid.chunks_y:
            north = textures[(cx, cy + 1)]
            diff = np.abs(tex[-1, :].astype(int) - north[0, :].astype(int))
            max_diff = max(max_diff, int(diff.max()))
    assert max_diff <= 1  # tolerance: at most one 8-bit step


def _run_cli(args, expect_code=0):
    env = dict(os.environ, PYTHONPATH=REPO_ROOT)
    proc = subprocess.run(
        [sys.executable, "-m", "terrain_bake", *args],
        cwd=REPO_ROOT, env=env, capture_output=True, text=True)
    assert proc.returncode == expect_code, proc.stderr
    return proc


def test_crash_resume_is_byte_identical_to_one_shot(tmp_path):
    one_shot = tmp_path / "one_shot"
    resumed = tmp_path / "resumed"
    run_bake(PARAMS, str(one_shot))

    args = ["bake", "--out", str(resumed), "--seed", str(PARAMS["seed"]),
            "--chunk-verts", "9", "--chunks-x", "3", "--chunks-y", "2"]
    _run_cli(args + ["--crash-after", "2"], expect_code=1)  # killed mid-bake
    _run_cli(args + ["--crash-after", "4"], expect_code=1)  # killed again
    proc = _run_cli(args)  # resumes purely from committed snapshot
    report = json.loads(proc.stdout)
    assert report["resume_count"] == 2
    assert report["repaired_boundary_vertices"] > 0

    result = compare_bakes(str(one_shot), str(resumed))
    assert result["ok"], json.dumps(result, indent=2)


def test_corrupt_or_stale_checkpoint_is_discarded(tmp_path):
    out = tmp_path / "bake"
    run_bake(PARAMS, str(out))

    # Corrupt the snapshot: it must be ignored and the bake redone cleanly.
    snapshot = out / ".bake_checkpoint.json"
    snapshot.write_text("{ not json", encoding="utf-8")
    report = run_bake(PARAMS, str(out))
    assert report["resume_count"] == 0

    # A snapshot from different inputs must not be reused either.
    other = tmp_path / "other"
    run_bake(default_params(seed=1, chunk_verts=9, chunks_x=3, chunks_y=2),
             str(other))
    stale = (other / ".bake_checkpoint.json").read_bytes()
    snapshot.write_bytes(stale)
    report = run_bake(PARAMS, str(out))
    assert report["resume_count"] == 0
    assert report["seed"] == PARAMS["seed"]


def test_report_is_deterministic_except_elapsed(tmp_path):
    dir_a = tmp_path / "a"
    dir_b = tmp_path / "b"
    report_a = run_bake(PARAMS, str(dir_a))
    report_b = run_bake(PARAMS, str(dir_b))
    assert report_a.pop("elapsed_ms") is not None
    assert report_b.pop("elapsed_ms") is not None
    assert report_a == report_b
    assert list(report_a) == ["seed", "input_digest", "heights_digest",
                              "grid", "total_chunks", "total_vertices",
                              "repaired_boundary_vertices", "resume_count",
                              "texture_hashes"]
    assert len(report_a["texture_hashes"]) == report_a["total_chunks"]


def test_verify_cli_reports_structured_mismatch(tmp_path):
    dir_a = tmp_path / "a"
    dir_b = tmp_path / "b"
    run_bake(PARAMS, str(dir_a))
    run_bake(PARAMS, str(dir_b))

    proc = _run_cli(["verify", str(dir_a), str(dir_b)])
    assert json.loads(proc.stdout)["ok"]

    # Tamper with one chunk's normals: verify must flag chunk + field.
    target = dir_b / "chunks" / "chunk_1_0" / "normals.npy"
    arr = np.load(target)
    arr[0, 0] += 0.5
    with open(target, "wb") as fh:
        np.save(fh, arr)
    proc = _run_cli(["verify", str(dir_a), str(dir_b)], expect_code=1)
    result = json.loads(proc.stdout)
    assert not result["ok"]
    mismatch = result["mismatches"][0]
    assert mismatch["chunk"] == [1, 0]
    assert mismatch["field"] == "normals"
    assert mismatch["detail"]["byte_equal"] is False


def test_manifest_contents(tmp_path):
    out = tmp_path / "bake"
    run_bake(PARAMS, str(out))
    manifest = json.loads((out / "chunks.json").read_text())
    grid = _grid()
    assert manifest["grid"]["chunk_count"] == grid.chunk_count
    assert len(manifest["chunks"]) == grid.chunk_count
    for entry in manifest["chunks"]:
        assert entry["vertices"] == [grid.chunk_verts, grid.chunk_verts]
        assert set(entry["neighbors"]) == {"west", "east", "south", "north"}
        bbox = entry["bbox"]
        assert all(lo <= hi for lo, hi in zip(bbox["min"], bbox["max"]))
        assert set(entry["outputs"]) == {"positions", "normals", "uv",
                                         "texture"}
