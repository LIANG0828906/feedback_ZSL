from __future__ import annotations

from pathlib import Path

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.dependencies import DependencyGraph
from terrain_pipeline.loader import load_project


def test_dependency_graph_is_acyclic_and_topo_ordered(project_dir: Path) -> None:
    project = load_project(project_dir)
    graph = DependencyGraph(project)
    assert len(graph.edges) == 7
    positions = {tile_id: index for index, tile_id in enumerate(graph.topological_order)}
    for edge in graph.edges:
        assert positions[edge.source] < positions[edge.target]
    assert graph.dependencies["T0004"] == ("T0005",)
    assert "T0004" in graph.dependents["T0005"]


def test_edit_invalidates_exact_downstream_closure(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    pipeline = Pipeline(project_dir, tmp_path / "cache", params=params)
    report = pipeline.apply_material_edit("T0004", "rock", 1.75)
    assert set(report.affected) == {"T0004", "T0001", "T0003", "T0000"}
    assert set(report.reused) == {"T0002", "T0005"}
    assert report.trigger[0]["kind"] == "material_edit"
    assert report.reasons["T0000"]
    assert all(item["edge"] == "seam_normal+seam_material" for item in report.reasons["T0000"])


def test_local_height_edit_changes_only_that_tile_interior(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    pipeline = Pipeline(project_dir, tmp_path / "cache", params=params)
    report = pipeline.apply_height_edit("T0004", 4, 4, 14.5)
    assert report.changed == ["T0004"]
    assert "T0001" in report.affected
    assert "T0000" in report.affected
    assert "T0002" in report.reused


def test_seam_height_edit_updates_shared_world_vertex(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    pipeline = Pipeline(project_dir, tmp_path / "cache", params=params)
    report = pipeline.apply_height_edit("T0004", 0, 0, 7.25)
    assert set(report.changed) == {"T0004", "T0003", "T0001", "T0000"}
    for tile_id in report.changed:
        tile = pipeline.project.tiles[tile_id]
        world_x = 8
        world_y = 8
        x = world_x - tile.grid_x * 8
        y = world_y - tile.grid_y * 8
        assert tile.height[y, x] == 7.25


def test_invalidation_query_does_not_modify_project(
    project_dir: Path,
    params: BakeParams,
    tmp_path: Path,
) -> None:
    pipeline = Pipeline(project_dir, tmp_path / "cache", params=params)
    before = pipeline.project.global_content_hash
    report = pipeline.query_invalidation(["T0004"])
    assert set(report.affected) == {"T0004", "T0001", "T0003", "T0000"}
    assert pipeline.project.global_content_hash == before
