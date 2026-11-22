"""需求 2: 包络检测与台词窗口推导。"""

from a11y_media.envelope import boundary_quant_bound, detect_segments
from a11y_media.loader import load_envelope
from a11y_media.pipeline import compute_detection, compute_windows


def test_detection_within_quant_bound(project):
    envelope = load_envelope(project)
    detected = compute_detection(project, envelope)
    assert len(detected) == len(project.segments)
    bound = boundary_quant_bound(project.hop)
    for (ds, de), seg in zip(detected, project.segments):
        assert abs(ds - seg.start) <= bound
        assert abs(de - seg.end) <= bound
        assert ds % project.hop == 0 and de % project.hop == 0  # hop 网格量化


def test_boundary_deviation_monotonic_in_hop(project):
    """帧移按整数倍收紧时, 段边界偏差单调不增。"""
    envelope = load_envelope(project)
    prev_err = None
    hop = project.hop
    while hop >= 2:
        detected = detect_segments(envelope, project.frame_len, hop)
        assert len(detected) == len(project.segments)
        err = sum(
            abs(ds - seg.start) + abs(de - seg.end)
            for (ds, de), seg in zip(detected, project.segments)
        )
        if prev_err is not None:
            assert err <= prev_err, f"hop={hop}: {err} > {prev_err}"
        prev_err = err
        hop //= 2


def test_windows_inside_segments_and_non_overlapping(project):
    envelope = load_envelope(project)
    detected = compute_detection(project, envelope)
    windows = compute_windows(project, detected)
    assert len(windows) == len(project.subtitles)
    by_segment = {}
    for sub, (w0, w1) in zip(project.subtitles, windows):
        ds, de = detected[sub.segment]
        assert ds <= w0 < w1 <= de, f"sub {sub.sub_id} 窗口越出语音段"
        by_segment.setdefault(sub.segment, []).append((w0, w1))
    for seg_id, ws in by_segment.items():
        ws.sort()
        for (a0, a1), (b0, b1) in zip(ws, ws[1:]):
            assert a1 <= b0, f"segment {seg_id} 窗口重叠"


def test_implicit_windows_tile_segment(project):
    """多句隐式窗口应首尾相接铺满检测段。"""
    envelope = load_envelope(project)
    detected = compute_detection(project, envelope)
    windows = compute_windows(project, detected)
    by_segment = {}
    for sub, w in zip(project.subtitles, windows):
        if sub.window is None:
            by_segment.setdefault(sub.segment, []).append(w)
    for seg_id, ws in by_segment.items():
        ws.sort()
        ds, de = detected[seg_id]
        assert ws[0][0] == ds and ws[-1][1] == de
        for (_, a1), (b0, _) in zip(ws, ws[1:]):
            assert a1 == b0
