from __future__ import annotations

import json
from pathlib import Path

import pytest

from mesh_repair.binary import file_sha256, load_mesh
from mesh_repair.boundary import extract_boundary_loops
from mesh_repair.commit import SessionLock
from mesh_repair.coordinator import BoundaryRegistry, RepairConfig, RepairService
from mesh_repair.errors import E_BOUNDARY_PENDING, E_BUSY
from mesh_repair.repair import fill_boundary_loops
from mesh_repair.weld import weld_vertices


def standard_config(**overrides) -> RepairConfig:
    values = dict(
        block_shape=(2, 2, 1),
        parallelism=2,
        global_budget_bytes=8 * 1024 * 1024,
        task_budget_bytes=4 * 1024 * 1024,
    )
    values.update(overrides)
    return RepairConfig(**values)


def test_end_to_end_repair_is_watertight_and_seam_consistent(
    sample_bundle: dict, tmp_path: Path
) -> None:
    output = tmp_path / "repaired.msh"
    report = RepairService(tmp_path / "work").repair_file(
        sample_bundle["path"], output, config=standard_config()
    )
    assert report["ok"] is True
    assert report["evaluation"]["status"] == "ok"
    assert report["artifact_vertices"] == 87
    assert report["artifact_faces"] == 170
    assert report["boundary_consistency"]["watertight"] is True
    assert report["boundary_consistency"]["seam_consistent"] is True
    assert report["boundary_consistency"]["shared_edge_count"] > 0
    assert report["hole_fill"]["filled_face_count"] == 4
    assert report["hole_fill"]["dependency_block_ids"] == [0, 1, 2, 3]
    assert report["nonmanifold_split"]["split_edge_count"] == 1
    assert report["fragment_removal"]["removed_component_count"] == 2
    assert output.exists()
    loaded, _ = load_mesh(output)
    assert loaded.vertex_count == 87
    assert loaded.face_count == 170
    trace = Path(report["trace_path"]).read_text()
    for event in (
        "partition_complete",
        "budget_grant",
        "hole_fill_complete",
        "nonmanifold_split_complete",
        "block_atomic_commit",
        "blocks_assembled",
        "run_complete",
    ):
        assert f'"event":"{event}"' in trace


def test_budget_rejection_writes_no_mesh(sample_bundle: dict, tmp_path: Path) -> None:
    output = tmp_path / "should-not-exist.msh"
    config = standard_config(task_budget_bytes=20_000, global_budget_bytes=100_000)
    with pytest.raises(Exception) as caught:
        RepairService(tmp_path / "work").repair_file(
            sample_bundle["path"], output, config=config
        )
    assert caught.value.code == "E_BUDGET_EXCEEDED"
    assert "block_id" in caught.value.location
    assert not output.exists()


def test_busy_lock_rejects_duplicate_start(sample_bundle: dict, tmp_path: Path) -> None:
    source_sha = file_sha256(sample_bundle["path"])
    work = tmp_path / "work"
    lock = SessionLock(work / "locks" / f"{source_sha}.lock")
    with lock:
        with pytest.raises(Exception) as caught:
            RepairService(work).repair_file(
                sample_bundle["path"],
                tmp_path / "busy.msh",
                config=standard_config(),
            )
    assert caught.value.code == E_BUSY
    assert not (tmp_path / "busy.msh").exists()


def test_cancelled_before_start_keeps_no_output(
    sample_bundle: dict, tmp_path: Path
) -> None:
    import threading

    cancel = threading.Event()
    cancel.set()
    with pytest.raises(Exception) as caught:
        RepairService(tmp_path / "work").repair_file(
            sample_bundle["path"],
            tmp_path / "cancelled.msh",
            config=standard_config(),
            cancel_event=cancel,
        )
    assert caught.value.code == "E_CANCELLED"
    assert not (tmp_path / "cancelled.msh").exists()


def test_cross_block_fill_requires_committed_boundary_maps(
    sample_bundle: dict,
) -> None:
    mesh, _ = load_mesh(sample_bundle["path"])
    welded = weld_vertices(mesh, 1e-6)
    boundary = extract_boundary_loops(welded.mesh)
    hole = boundary.loops[0]
    registry = BoundaryRegistry()
    with pytest.raises(Exception) as caught:
        fill_boundary_loops(
            welded.mesh,
            [hole],
            loop_block_ids={hole.loop_id: {0, 3}},
            readiness=registry.check,
        )
    assert caught.value.code == E_BOUNDARY_PENDING
    assert caught.value.context["missing_blocks"] == [0, 3]
