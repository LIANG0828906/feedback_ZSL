from __future__ import annotations

import json

import pytest

from vectorbool.boolean import boolean_paths
from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.parser import parse_path
from vectorbool.project import load_project, save_project

from .helpers import rectangle


def test_project_roundtrip_and_atomic_commit(tmp_path) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    filename = tmp_path / "project.vbproj.json"
    digest = save_project(
        filename,
        path_a=path_a,
        path_b=path_b,
        operation="union",
        tolerance=1.0e-6,
        result=result,
    )
    assert filename.exists()
    assert not (tmp_path / "project.vbproj.json.tmp").exists()
    loaded = load_project(filename)
    assert loaded.committed
    assert loaded.result is not None
    assert loaded.result_object is not None
    assert loaded.result_object.operation.value == "union"
    assert loaded.result_object.report.accepted
    assert loaded.content_hash == digest


def test_degraded_result_is_not_restored_as_committed(tmp_path) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    result.report.accepted = False
    result.report.status = "degraded"
    filename = tmp_path / "degraded.json"
    save_project(
        filename,
        path_a=path_a,
        path_b=path_b,
        operation="union",
        tolerance=1.0e-6,
        result=result,
    )
    loaded = load_project(filename)
    assert not loaded.committed
    assert loaded.result is None
    assert loaded.report["status"] == "degraded"


@pytest.mark.parametrize("mutation", ["version", "hash"])
def test_project_mismatch_is_rejected(tmp_path, mutation: str) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    filename = tmp_path / "project.json"
    save_project(
        filename,
        path_a=path_a,
        path_b=path_b,
        operation="union",
        tolerance=1.0e-6,
    )
    document = json.loads(filename.read_text(encoding="utf-8"))
    if mutation == "version":
        document["version"] = 999
    else:
        document["payload"]["tolerance"] = 2.0e-6
    filename.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(BusinessError) as captured:
        load_project(filename)
    assert captured.value.code == ErrorCode.PROJECT_MISMATCH

