from __future__ import annotations

from pathlib import Path

import pytest

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.compare import compare_snapshots
from terrain_pipeline.errors import E_SCHEMA, PipelineError


def test_serial_and_parallel_full_bakes_match(
    project_dir: Path,
    tmp_path: Path,
) -> None:
    serial = Pipeline(
        project_dir,
        tmp_path / "serial",
        params=BakeParams(subdivisions=2, max_workers=1),
    ).run(mode="full")
    parallel = Pipeline(
        project_dir,
        tmp_path / "parallel",
        params=BakeParams(subdivisions=2, max_workers=4),
    ).run(mode="full")
    assert serial.success and parallel.success
    assert serial.invalidations.to_dict() == parallel.invalidations.to_dict()
    assert compare_snapshots(tmp_path / "serial", tmp_path / "parallel", raise_on_diff=False)[
        "equal"
    ]


def test_serial_and_parallel_edit_sequences_match(
    project_dir: Path,
    tmp_path: Path,
) -> None:
    from terrain_pipeline.loader import save_edits

    edits = tmp_path / "edits.json"
    prep = Pipeline(
        project_dir,
        tmp_path / "prep",
        params=BakeParams(subdivisions=2, max_workers=1),
    )
    prep.apply_height_edit("T0004", 4, 4, 17.0)
    prep.apply_material_edit("T0001", "rock", 1.9)
    save_edits(prep.project, edits)

    serial_pipeline = Pipeline(
        project_dir,
        tmp_path / "serial",
        params=BakeParams(subdivisions=2, max_workers=1),
        edits_file=edits,
    )
    serial = serial_pipeline.run(mode="full")
    parallel_pipeline = Pipeline(
        project_dir,
        tmp_path / "parallel",
        params=BakeParams(subdivisions=2, max_workers=4),
        edits_file=edits,
    )
    parallel = parallel_pipeline.run(mode="full")
    assert serial.success and parallel.success
    assert serial.invalidations.to_dict() == parallel.invalidations.to_dict()
    assert compare_snapshots(tmp_path / "serial", tmp_path / "parallel", raise_on_diff=False)[
        "equal"
    ]
    assert parallel.effective_workers <= 4


def test_worker_limit_is_enforced(tmp_path: Path) -> None:
    with pytest.raises(PipelineError) as caught:
        BakeParams(max_workers=5).validate()
    assert caught.value.code == E_SCHEMA
    assert caught.value.field == "max_workers"
