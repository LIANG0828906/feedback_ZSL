import os

import pytest

from subpipeline.envelope import detect_segments
from subpipeline.errors import E_GAP, E_TIMING, PipelineError
from subpipeline.loader import load_project
from subpipeline.timing import (
    extension_caps, extend_window, readable_duration_s, resolve_subtitle,
)


def test_envelope_matches_declared_segments(samples_root):
    for name in ("proj_main_20261123", "proj_narrow_20261124",
                 "proj_tight_20261125", "proj_tiny_20261126"):
        project, _m, wave, _h = load_project(
            os.path.join(samples_root, name))
        detected = detect_segments(
            wave, project.frame_length, project.frame_shift,
            project.energy_threshold)
        expected = [(s.start_sample, s.end_sample)
                    for s in project.segments]
        assert detected == expected, name


def test_windows_meet_readable_duration(main_project):
    project = main_project
    bounds = extension_caps(project.segments)
    seg_map = project.segment_index()
    for sub in project.subtitles:
        out = resolve_subtitle(sub, seg_map[sub.segment_ref],
                               project.params, bounds, project.sample_rate)
        duration = (out["end_sample"] - out["start_sample"]) / project.sample_rate
        assert duration + 1.0 / project.sample_rate >= out["required_duration_s"]


def test_extension_never_overlaps(main_project):
    project = main_project
    bounds = extension_caps(project.segments)
    seg_map = project.segment_index()
    windows = []
    for sub in project.subtitles:
        out = resolve_subtitle(sub, seg_map[sub.segment_ref],
                               project.params, bounds, project.sample_rate)
        windows.append((out["start_sample"], out["end_sample"]))
    windows.sort()
    for (a0, a1), (b0, b1) in zip(windows, windows[1:]):
        assert b0 >= a1


def test_tight_project_raises_egap(samples_root):
    project, _m, _w, _h = load_project(
        os.path.join(samples_root, "proj_tight_20261125"))
    bounds = extension_caps(project.segments)
    seg_map = project.segment_index()
    with pytest.raises(PipelineError) as exc:
        for sub in project.subtitles:
            resolve_subtitle(sub, seg_map[sub.segment_ref],
                             project.params, bounds, project.sample_rate)
    assert exc.value.code == E_GAP
    assert exc.value.subtitle_id is not None


def test_extend_window_bounds():
    # Window length must grow to required_end - original start (=100).
    s, e, short = extend_window(100, 120, 200, earliest=80, latest=210)
    assert s >= 80 and e <= 210
    assert short == 0
    assert (e - s) >= (200 - 100)


def test_extend_window_reports_shortage():
    s, e, short = extend_window(100, 120, 300, earliest=90, latest=130)
    assert short > 0


def test_readable_duration_decreases_with_more_screens(main_project):
    project = main_project
    sub = next(s for s in project.subtitles if len(s.text) >= 14)
    from subpipeline.wrapping import layout_text, reflow_with_screens
    base_screens = layout_text(sub.text, project.params)
    speed = sub.max_read_speed or project.params.default_read_speed
    base = readable_duration_s(base_screens, project.params, speed)
    assert sum(s.line_count for s in base_screens) > 1
    reflowed = reflow_with_screens(sub.text, project.params, 1)
    assert len(reflowed) > len(base_screens)
    reflow = readable_duration_s(reflowed, project.params, speed)
    assert reflow < base
