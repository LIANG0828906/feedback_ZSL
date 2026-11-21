from __future__ import annotations

import hashlib
import json

import numpy as np
import pytest

from tactical_scene.camera import Camera
from tactical_scene.errors import ErrorCode, TacticalError
from tactical_scene.generator import generate_sample_suite, verify_sample_suite
from tactical_scene.query import BudgetConfig, QueryEngine
from tactical_scene.scene import AABBShape, Scene, SceneUnit
from tactical_scene.trace import TraceRecorder, TraceReplayer, load_trace
from tactical_scene.versioning import SceneRepository


def _camera() -> Camera:
    return Camera.orthographic(
        left=0,
        right=100,
        bottom=0,
        top=100,
        width=100,
        height=100,
        near=0.1,
        far=1000,
    )


def _transform(x: float, y: float, z: float) -> tuple[float, ...]:
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 3] = (x, y, z)
    return tuple(float(value) for value in matrix.reshape(-1))


def _scene() -> Scene:
    units = tuple(
        SceneUnit(
            index + 1,
            f"u{index + 1}",
            AABBShape((-2, -2, -2), (2, 2, 2)),
            _transform(10 + index * 8, 20 + index * 5, 100 + index),
        )
        for index in range(8)
    )
    return Scene(1, units)


def _budget() -> BudgetConfig:
    return BudgetConfig(budget_ms=1e12, memory_budget_bytes=2**40, bucket_grids=(64,))


def test_trace_is_byte_deterministic_and_replays(tmp_path):
    scene = _scene()
    camera = _camera()
    repository = SceneRepository(scene)
    engine = QueryEngine(scene, camera, budget_config=_budget())
    selection = engine.select_rect(0, 0, 70, 80)
    hover = engine.hover(20, 80)
    trace_a = tmp_path / "a.jsonl"
    trace_b = tmp_path / "b.jsonl"
    for path in (trace_a, trace_b):
        with TraceRecorder(path) as recorder:
            recorder.record_selection(selection, camera)
            recorder.record_hover(hover, camera)
    assert trace_a.read_bytes() == trace_b.read_bytes()

    records = load_trace(trace_a)
    assert len(records) == 2
    replayed = TraceReplayer(repository.history_snapshot).replay(trace_a)
    assert replayed == [record["event"] for record in records]


def test_tampered_trace_is_invalid(tmp_path):
    scene = _scene()
    camera = _camera()
    selection = QueryEngine(scene, camera, budget_config=_budget()).select_rect(0, 0, 70, 80)
    path = tmp_path / "trace.jsonl"
    with TraceRecorder(path) as recorder:
        recorder.record_selection(selection, camera)
    record = json.loads(path.read_text(encoding="utf-8"))
    record["event"]["selected"] = []
    path.write_text(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    with pytest.raises(TacticalError) as exc_info:
        load_trace(path)
    assert exc_info.value.code == ErrorCode.TRACE_INVALID


def test_trace_with_missing_version_is_invalid_even_with_valid_hash(tmp_path):
    scene = _scene()
    camera = _camera()
    selection = QueryEngine(scene, camera, budget_config=_budget()).select_rect(0, 0, 70, 80)
    path = tmp_path / "trace.jsonl"
    with TraceRecorder(path) as recorder:
        recorder.record_selection(selection, camera)
    record = json.loads(path.read_text(encoding="utf-8"))
    record["event"].pop("scene_version")
    unsigned = dict(record)
    unsigned.pop("record_hash", None)
    encoded = json.dumps(unsigned, sort_keys=True, separators=(",", ":")).encode("utf-8")
    record["record_hash"] = hashlib.sha256(encoded).hexdigest()
    path.write_text(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    with pytest.raises(TacticalError) as exc_info:
        load_trace(path)
    assert exc_info.value.code == ErrorCode.TRACE_INVALID


def test_small_sample_suite_verifies_and_is_reproducible(tmp_path):
    first = tmp_path / "first"
    second = tmp_path / "second"
    generate_sample_suite(first, seed=91, sizes=(12, 32))
    generate_sample_suite(second, seed=91, sizes=(12, 32))
    first_suite = json.loads((first / "suite.json").read_text(encoding="utf-8"))
    second_suite = json.loads((second / "suite.json").read_text(encoding="utf-8"))
    assert first_suite == second_suite
    for case in first_suite["cases"]:
        name = case["name"]
        assert (first / name / "trace.jsonl").read_bytes() == (second / name / "trace.jsonl").read_bytes()
    verification = verify_sample_suite(first)
    assert len(verification["cases"]) == len(first_suite["cases"])
    assert any(case["expected_errors"] for case in verification["cases"])
