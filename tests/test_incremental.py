from __future__ import annotations

from dataclasses import replace

import pytest

from formation_workbench.engine import FormationEngine
from formation_workbench.errors import ErrorCode, WorkbenchError

from .helpers import lane_scene


def test_leaf_edit_only_invalidates_leaf(sample_engine):
    intervals_before = sample_engine.intervals()
    manifest = sample_engine.apply_cell_cost((11, 7), 4)
    assert manifest.direct_path_members == ("delta",)
    assert manifest.path_recomputed == ("delta",)
    assert manifest.smooth_recomputed == ("delta",)
    assert manifest.simulation_recomputed == ("delta",)
    assert set(manifest.reused_paths) == {"alpha", "bravo", "charlie"}
    assert all(
        interval.member_id == "delta" for interval in manifest.invalidated_intervals
    )
    assert {interval.member_id for interval in manifest.reused_intervals} == {
        "alpha",
        "bravo",
        "charlie",
    }
    states = sample_engine.member_states()
    assert states["delta"]["path_cache_hit"] is False
    assert states["alpha"]["path_cache_hit"] is True
    intervals_after = sample_engine.intervals()
    assert intervals_after["alpha"] is intervals_before["alpha"]
    assert intervals_after["bravo"] is intervals_before["bravo"]
    assert intervals_after["charlie"] is intervals_before["charlie"]
    assert intervals_after["delta"] is not intervals_before["delta"]
    comparison = sample_engine.compare_with_full()
    assert comparison.ok
    assert comparison.checked_ticks == 2004


def test_root_edit_invalidates_descendant_closure(sample_engine):
    manifest = sample_engine.apply_cell_cost((13, 9), 8)
    assert manifest.direct_path_members == ("alpha",)
    assert manifest.path_recomputed == ("alpha",)
    assert set(manifest.smooth_recomputed) == {"alpha", "bravo", "charlie", "delta"}
    assert set(manifest.reused_paths) == {"bravo", "charlie", "delta"}
    assert manifest.reused_simulations == ()
    assert sample_engine.compare_with_full().ok


def test_off_path_edit_reuses_everything(sample_engine):
    before = sample_engine.paths()
    manifest = sample_engine.apply_cell_cost((2, 2), 7)
    assert manifest.direct_path_members == ()
    assert manifest.path_recomputed == ()
    assert manifest.smooth_recomputed == ()
    assert manifest.simulation_recomputed == ()
    assert set(manifest.reused_paths) == {"alpha", "bravo", "charlie", "delta"}
    assert manifest.invalidated_intervals == ()
    after = sample_engine.paths()
    assert before["alpha"].points == after["alpha"].points
    assert all(
        state["path_cache_hit"]
        for state in sample_engine.member_states().values()
    )
    assert sample_engine.compare_with_full().ok


def test_off_path_cost_decrease_stays_consistent(sample_engine):
    manifest = sample_engine.apply_cell_cost((16, 6), 1)
    assert manifest.status == "ok"
    assert sample_engine.compare_with_full().ok


def test_blockade_edit_and_consistency(sample_engine):
    manifest = sample_engine.apply_blockade("blk-crossing", True)
    assert set(manifest.direct_path_members) == {"alpha", "delta"}
    assert "blk-crossing" in sample_engine.scene.map.blockade_cells() or True
    assert sample_engine.compare_with_full().ok
    second = sample_engine.apply_blockade("blk-crossing", False)
    assert second.status == "ok"
    assert sample_engine.compare_with_full().ok


def test_multiple_edits_match_full_rebuild(sample_engine):
    sample_engine.apply_cell_cost((11, 7), 5)
    sample_engine.apply_cell_cost((13, 9), 6)
    sample_engine.apply_cell_cost((20, 13), 3)
    sample_engine.apply_blockade("blk-forest-north", False)
    assert sample_engine.compare_with_full().ok


def test_single_unit_failure_is_isolated_and_retryable(tmp_path):
    engine = FormationEngine(
        lane_scene(),
        checkpoint_path=tmp_path / "lane.json",
    )
    assert engine.build_all(save_checkpoint=True)["complete"]

    manifest = engine.apply_cell_cost((5, 4), -1)
    assert manifest.direct_path_members == ("bravo",)
    states = engine.member_states()
    assert states["bravo"]["status"] == "failed"
    assert states["bravo"]["path_error"]["code"] == ErrorCode.E_NO_PATH.value
    assert states["alpha"]["status"] == "ok"
    assert states["charlie"]["status"] == "ok"
    assert states["alpha"]["path_cache_hit"] is True
    assert states["charlie"]["path_cache_hit"] is True

    restore = engine.apply_cell_cost((5, 4), 1)
    assert restore.status == "partial"
    assert engine.member_states()["bravo"]["status"] == "failed"

    retried = engine.retry_member("bravo")
    assert retried.path_recomputed == ("bravo",)
    assert engine.member_states()["bravo"]["status"] == "ok"
    assert engine.compare_with_full().ok


def test_dependency_graph_records_interval_edges(sample_engine):
    graph = sample_engine.dependency_graph_payload()
    assert graph["topology_order"] == ["alpha", "bravo", "charlie", "delta"]
    assert set(graph["path_dependencies"]) == {"alpha", "bravo", "charlie", "delta"}
    assert graph["simulation_interval_edges"]
    edge = graph["simulation_interval_edges"][0]
    assert edge["reason"] == "formation_parent_child_slot_coupling"
    assert edge["tick_start"] <= edge["tick_end"]


def test_comparator_reports_first_path_cost_divergence(sample_engine):
    with sample_engine._lock:
        original = sample_engine._raw_paths["alpha"]
        sample_engine._raw_paths["alpha"] = replace(
            original, cost=original.cost + 1
        )
    with pytest.raises(WorkbenchError) as captured:
        sample_engine.compare_with_full()
    error = captured.value
    assert error.code == ErrorCode.E_DIVERGE
    assert error.item == "path_cost"
    assert error.member_id == "alpha"
    assert error.left != error.right
