from __future__ import annotations

import time

import pytest

import vectorbool.service as service_module
from vectorbool.boolean import boolean_paths
from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.model import BooleanResult, Operation, Point
from vectorbool.parser import parse_path
from vectorbool.preview import rasterize_contours, render_preview
from vectorbool.service import BooleanTaskService

from .helpers import rectangle


def test_preview_is_byte_deterministic_and_uses_unified_grid() -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    first = render_preview(path_a, path_b, result, tolerance=1.0e-6)
    second = render_preview(path_a, path_b, result, tolerance=1.0e-6)
    assert first.width == 512
    assert first.height == 512
    assert first.png == second.png
    assert first.rgba == second.rgba
    assert first.png_sha256 == second.png_sha256
    assert len(set(first.rgba)) > 1


def test_degraded_result_cannot_render() -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    result.report.accepted = False
    result.report.status = "degraded"
    result.preview_allowed = False
    with pytest.raises(BusinessError) as captured:
        render_preview(path_a, path_b, result, tolerance=1.0e-6)
    assert captured.value.code == ErrorCode.RESULT_DEGRADED


def test_explicit_grid_rasterization_is_byte_deterministic() -> None:
    contours = [
        [Point(0.0, 0.0), Point(2.0, 0.0), Point(2.0, 2.0), Point(0.0, 2.0)]
    ]
    first = rasterize_contours(contours, (0.0, 0.0, 2.0, 2.0), width=64, height=64)
    second = rasterize_contours(contours, (0.0, 0.0, 2.0, 2.0), width=64, height=64)
    assert first.tobytes() == second.tobytes()
    assert len(set(first.tobytes())) > 1


def test_service_rejects_duplicate_path_pair(monkeypatch: pytest.MonkeyPatch) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)

    def slow_boolean(*args, **kwargs):
        time.sleep(0.25)
        return BooleanResult(
            operation=Operation.UNION,
            tolerance=1.0e-6,
            contours=[],
            intersection_count=0,
            report=None,
        )

    monkeypatch.setattr(service_module, "boolean_paths", slow_boolean)
    service = BooleanTaskService()
    token = service.submit(path_a, path_b, "union", tolerance=1.0e-6)
    with pytest.raises(BusinessError) as captured:
        service.submit(path_a, path_b, "intersect", tolerance=1.0e-6)
    assert captured.value.code == ErrorCode.BUSY
    service.cancel(token)
    deadline = time.time() + 3.0
    while service.active_count and time.time() < deadline:
        time.sleep(0.01)
    assert service.active_count == 0


def test_cancel_and_failure_keep_last_successful_preview(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    service = BooleanTaskService()
    outcome = service.submit_and_wait(path_a, path_b, "union", tolerance=1.0e-6)
    assert outcome.preview is not None
    previous_sha = service.last_preview.png_sha256

    def failing_boolean(*args, **kwargs):
        raise BusinessError(ErrorCode.BOOLEAN_FAILED, "injected worker failure")

    monkeypatch.setattr(service_module, "boolean_paths", failing_boolean)
    with pytest.raises(BusinessError):
        service.submit_and_wait(path_a, path_b, "xor", tolerance=1.0e-6)
    assert service.last_preview.png_sha256 == previous_sha

