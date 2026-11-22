from __future__ import annotations

import copy
import threading
from pathlib import Path

import numpy as np
import pytest

import glyph_workbench.engine as engine_module
from glyph_workbench.compare import assert_results_equal, diff_results
from glyph_workbench.engine import process_full, process_glyph
from glyph_workbench.errors import WorkbenchError
from glyph_workbench.params import RenderParams


def _params(workers: int = 2) -> RenderParams:
    return RenderParams(
        pixels_per_unit=0.25,
        tolerance=0.35,
        grid=8,
        max_workers=workers,
    )


@pytest.mark.parametrize("glyph_id", ["A", "O", "B"])
def test_full_incremental_and_recovered_paths_are_pixel_exact(
    document, tmp_path: Path, glyph_id: str
) -> None:
    glyph = document.find_glyph(glyph_id)
    params = _params(workers=4)
    full = process_full(glyph, params)
    assert full.complete

    pause = threading.Event()

    def pause_callback(index: int, result) -> None:
        if index == 0:
            pause.set()

    paused = process_glyph(
        glyph,
        params,
        persist_root=tmp_path / "live",
        pause_event=pause,
        on_commit=pause_callback,
    )
    assert paused.paused
    assert paused.commit_sequence == [0]
    resumed = process_glyph(glyph, params, persist_root=tmp_path / "live")
    assert resumed.loaded_from_checkpoint == 1
    assert resumed.complete

    crash_root = tmp_path / "crash"
    from glyph_workbench.engine import CrashInjected

    def crash(index: int, result) -> None:
        if index == 0:
            raise CrashInjected("stop")

    with pytest.raises(CrashInjected):
        process_glyph(
            glyph,
            params,
            persist_root=crash_root,
            on_commit=crash,
        )
    recovered = process_glyph(glyph, params, persist_root=crash_root)
    assert recovered.loaded_from_checkpoint == 1

    assert_results_equal(full, resumed)
    assert_results_equal(full, recovered)
    assert resumed.commit_sequence == recovered.commit_sequence == list(
        range(len(glyph.contours))
    )


def test_parallelism_does_not_change_pixels_or_commit_order(document, tmp_path: Path) -> None:
    glyph = document.find_glyph("B")
    serial = process_full(glyph, _params(workers=1))
    parallel = process_full(glyph, _params(workers=4))
    assert_results_equal(serial, parallel)
    assert serial.commit_sequence == parallel.commit_sequence == [0, 1, 2]


def test_diff_reports_first_pixel_segment_stage_and_raises_e_diff(document) -> None:
    glyph = document.find_glyph("B")
    left = process_full(glyph, _params(workers=2))
    right = copy.deepcopy(left)
    mismatch = right.raster.coverage_counts > 0
    candidates = np.argwhere(mismatch)
    row, col = int(candidates[0][0]), int(candidates[0][1])
    right.raster.coverage_counts[row, col] ^= 1
    report = diff_results(left, right)
    assert not report.equal
    assert report.pixel == (row, col)
    assert report.segment_index is not None
    assert report.state is not None
    with pytest.raises(WorkbenchError) as caught:
        assert_results_equal(left, right)
    assert caught.value.code == "E_DIFF"
    assert caught.value.context["pixel"] == [row, col]


def test_single_segment_failure_is_isolated_and_retryable(
    document, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    glyph = document.find_glyph("B")
    params = _params(workers=3)
    expected = process_full(glyph, params)
    original = engine_module.rasterize_flat_contour

    def fail_contour_one(flat, spec, parameters):
        if flat.contour_index == 1:
            raise WorkbenchError(
                "E_STATE",
                "injected single-segment failure",
                {"contour_index": flat.contour_index},
            )
        return original(flat, spec, parameters)

    monkeypatch.setattr(engine_module, "rasterize_flat_contour", fail_contour_one)
    persist = tmp_path / "persist"
    failed = process_glyph(glyph, params, persist_root=persist)
    assert failed.status == "failed"
    assert failed.commit_sequence == [0]
    assert failed.states["segments"][0]["state"] == "COMMITTED"
    assert failed.states["segments"][1]["state"] == "FAILED"
    assert failed.states["segments"][2]["state"] == "FILLED"
    assert failed.states["segments"][2]["committed"] is False
    assert len(failed.failures) == 1
    assert failed.failures[0]["code"] == "E_STATE"

    monkeypatch.setattr(engine_module, "rasterize_flat_contour", original)
    retried = process_glyph(glyph, params, persist_root=persist)
    assert retried.complete
    assert retried.loaded_from_checkpoint == 1
    assert retried.commit_sequence == [0, 1, 2]
    assert_results_equal(expected, retried)

