from __future__ import annotations

from mesh_repair.binary import load_mesh
from mesh_repair.boundary import compare_boundary_tolerances, extract_boundary_loops
from mesh_repair.topology import build_topology
from mesh_repair.weld import weld_vertices


def test_weld_rebuilds_adjacency_and_closes_seams(sample_bundle: dict) -> None:
    mesh, _ = load_mesh(sample_bundle["path"])
    raw = build_topology(mesh)
    welded = weld_vertices(mesh, 1e-6)
    rebuilt = build_topology(welded.mesh)
    assert welded.duplicate_count == 6
    assert welded.mesh.vertex_count == 91
    assert rebuilt.edge_count < raw.edge_count
    assert len(rebuilt.boundary_edges) == 10
    assert len(rebuilt.nonmanifold_edges) == 1


def test_boundary_loops_include_counts_and_bbox(sample_bundle: dict) -> None:
    mesh, _ = load_mesh(sample_bundle["path"])
    welded = weld_vertices(mesh, 1e-6)
    result = extract_boundary_loops(welded.mesh)
    assert [loop.vertex_count for loop in result.loops] == [4, 4]
    assert all(len(loop.bbox_min) == 3 and len(loop.bbox_max) == 3 for loop in result.loops)
    assert len(result.open_chains) == 2


def test_tolerance_change_reports_boundary_difference(sample_bundle: dict) -> None:
    mesh, _ = load_mesh(sample_bundle["variants"]["tolerance_asset"]["path"])
    result = compare_boundary_tolerances(mesh, 1e-6, 1e-3)
    assert result["old_tolerance"] == 1e-6
    assert result["new_tolerance"] == 1e-3
    assert result["old_loop_count"] > result["new_loop_count"]
    assert result["loop_count_delta"] < 0
    assert "changed_loops" in result
