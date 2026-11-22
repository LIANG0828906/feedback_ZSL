import pytest

from subpipeline.errors import E_GAP, E_TIMING, PipelineError
from subpipeline.model import (
    AudioSegment, PresentationParams, Project, Subtitle,
)
from subpipeline.timing import extension_caps, resolve_subtitle


def _project(params, segments, subs):
    return Project(
        project_id="synthetic", sample_rate=1000, frame_length=100,
        frame_shift=50, energy_threshold=0.1, segments=tuple(segments),
        subtitles=tuple(subs), params=params, manifest_fingerprint="x")


def test_etiming_when_all_tradeoffs_exhausted():
    # Middle segment boxed in by neighbours with no usable gap; even after
    # margin compression and extra screens the window stays too short.
    params = PresentationParams(
        line_height_px=10, max_chars_per_line=20, vertical_margin_px=5,
        screen_height_px=40)
    segments = [AudioSegment("s0", 0, 980),
                AudioSegment("s1", 1000, 1100),  # 100 ms, 20 ms gaps
                AudioSegment("s2", 1120, 3000)]
    subs = [Subtitle(1, "一二三四五六七八九十一二三四五六", "s1",
                     max_read_speed=2.0)]
    project = _project(params, segments, subs)
    bounds = extension_caps(project.segments)
    with pytest.raises(PipelineError) as exc:
        resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    assert exc.value.code == E_TIMING
    assert "actual_s" in exc.value.details and "required_s" in exc.value.details


def test_egap_lists_conflicting_subtitle():
    # Two back-to-back segments with zero usable gap.
    params = PresentationParams(
        line_height_px=10, max_chars_per_line=20, vertical_margin_px=5,
        screen_height_px=40)
    segments = [AudioSegment("s0", 0, 1000),
                AudioSegment("s1", 1000, 1150),
                AudioSegment("s2", 1150, 1310)]
    subs = [Subtitle(1, "一二三四五六七八九十一二三四五六", "s1",
                     max_read_speed=1.0)]
    project = _project(params, segments, subs)
    bounds = extension_caps(project.segments)
    with pytest.raises(PipelineError) as exc:
        resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    assert exc.value.code == E_GAP
    assert exc.value.subtitle_id == 1


def test_tradeoff_records_ordered_and_explained():
    # Long text in a medium segment with limited gap forces margin/gap use.
    params = PresentationParams(
        line_height_px=10, max_chars_per_line=6, vertical_margin_px=5,
        screen_height_px=60)
    segments = [AudioSegment("s1", 1000, 4000),
                AudioSegment("s2", 4600, 7000)]
    subs = [Subtitle(1, "一二三四五六七八九十一二三四五六七八九十", "s1",
                     max_read_speed=20.0)]
    project = _project(params, segments, subs)
    bounds = extension_caps(project.segments)
    out = resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    actions = [t.action for t in out["tradeoffs"]]
    # Records follow the configured priority without duplicates out of order.
    priority = list(params.tradeoff_priority)
    assert actions == sorted(actions, key=priority.index)
    for record in out["tradeoffs"]:
        assert record.reason
        assert record.after_required_s <= record.before_required_s + 1e-9 or \
            record.action == "reclaim_gap"


def test_compress_margin_reduces_required_duration():
    params = PresentationParams(
        line_height_px=10, max_chars_per_line=4, vertical_margin_px=5,
        screen_height_px=40)
    segments = [AudioSegment("s1", 0, 100000)]
    subs = [Subtitle(1, "一二三四五六七八", "s1", max_read_speed=50.0)]
    project = _project(params, segments, subs)
    bounds = extension_caps(project.segments)
    out = resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    if any(t.action == "compress_margin" for t in out["tradeoffs"]):
        record = next(t for t in out["tradeoffs"]
                      if t.action == "compress_margin")
        assert record.after_required_s < record.before_required_s


def test_deterministic_repeat():
    params = PresentationParams(
        line_height_px=10, max_chars_per_line=6, vertical_margin_px=5,
        screen_height_px=60)
    segments = [AudioSegment("s1", 1000, 5000), AudioSegment("s2", 6000, 9000)]
    subs = [Subtitle(1, "一二三四五六七八九十一二三四五六", "s1",
                     max_read_speed=8.0)]
    project = _project(params, segments, subs)
    bounds = extension_caps(project.segments)
    first = resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    second = resolve_subtitle(subs[0], segments[0], params, bounds, 1000)
    assert (first["start_sample"], first["end_sample"]) == \
        (second["start_sample"], second["end_sample"])
    assert [s.lines for s in first["screens"]] == \
        [s.lines for s in second["screens"]]
