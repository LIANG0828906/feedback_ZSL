"""需求 4: 误差传播报告与上界校验(E_ERR)。"""

import pytest

from a11y_media import errprop
from a11y_media.engine import run_full
from a11y_media.errors import E_ERR, ComponentError


def test_bounds_derived_from_params_only(project):
    """上界只由帧移与时间单位量化步长推导。"""
    bounds = errprop.cumulative_bounds(project)
    assert bounds["envelope"] == project.hop // 2
    assert bounds["window"] == project.hop // 2
    assert bounds["schedule"] == project.hop // 2 + 1
    assert bounds["total"] == 2 * len(project.subtitles) * (project.hop // 2) + 1


def test_measured_within_bounds_on_samples(projects):
    for p in projects:
        result = run_full(p)
        bounds = result.report["bounds"]
        for entry in result.report["sentences"]:
            assert entry["envelope_err_start"] <= bounds["envelope"]
            assert entry["envelope_err_end"] <= bounds["envelope"]
            assert entry["window_dev"] <= bounds["window"]
            num, den = entry["char_res_max"]
            assert num / den <= bounds["schedule"]
        assert result.report["total_read"]["deviation"] <= bounds["total"]


def test_e_err_points_to_sentence_and_item(project):
    entry = {
        "sub": 3,
        "segment": 0,
        "envelope_err_start": 0,
        "envelope_err_end": 0,
        "window_dev": 10**9,
        "char_res_max": [0, 1],
    }
    bounds = errprop.cumulative_bounds(project)
    with pytest.raises(ComponentError) as ei:
        errprop.check_sentence_bounds(entry, bounds)
    assert ei.value.code == E_ERR
    assert ei.value.context["sub"] == 3
    assert ei.value.context["item"] == "window_dev"


def test_e_err_total(project):
    bounds = errprop.cumulative_bounds(project)
    with pytest.raises(ComponentError) as ei:
        errprop.check_total_bound(10**9, 0, bounds)
    assert ei.value.code == E_ERR
    assert ei.value.context["item"] == "total_read"


def test_report_chain_fields(project):
    """报告须含逐句四级误差与整工程总时长偏差。"""
    result = run_full(project)
    for entry in result.report["sentences"]:
        for key in (
            "envelope_err_start", "envelope_err_end", "window_dev", "char_res_max"
        ):
            assert key in entry
    assert "total_read" in result.report
    assert result.report["time_unit"] == "samples"
