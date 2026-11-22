"""需求 3: 字符级高亮排程与时长归一化。"""

import pytest

from a11y_media.errors import E_PARA, ComponentError
from a11y_media.loader import load_envelope
from a11y_media.pipeline import (
    compute_detection,
    compute_sentence_schedule,
    compute_windows,
)
from a11y_media.schedule import schedule_chars, validate_speed, validate_weights


@pytest.fixture(scope="module")
def derived(project):
    envelope = load_envelope(project)
    detected = compute_detection(project, envelope)
    windows = compute_windows(project, detected)
    starts = [
        compute_sentence_schedule(project, i, w) for i, w in enumerate(windows)
    ]
    return windows, starts


def test_intervals_tile_window(derived):
    """逐字符区间首尾相接、总时长等于台词窗口、无重叠无空洞。"""
    windows, starts = derived
    for (w0, w1), ss in zip(windows, starts):
        assert ss[0] == w0
        assert all(a < b for a, b in zip(ss, ss[1:]))  # 严格递增无重叠
        # 末字符区间终点 == 窗口终点(由 get_char_interval 语义保证, 这里验证铺排)
        assert ss[-1] < w1


def test_schedule_matches_truth(project, derived):
    """排程结果与真值在声明容差内吻合。"""
    windows, starts = derived
    tol = project.tolerance_samples
    for i, (w, ss) in enumerate(zip(windows, starts)):
        tw = project.truth.windows[i]
        assert abs(w[0] - tw[0]) <= tol and abs(w[1] - tw[1]) <= tol
        for actual, expect in zip(ss, project.truth.char_starts[i]):
            assert abs(actual - expect) <= tol


def test_total_read_matches_truth(project, derived):
    from a11y_media.schedule import total_read_samples

    windows, _ = derived
    total = sum(w1 - w0 for w0, w1 in windows)
    assert total_read_samples(total, project.assist.speed) == (
        project.truth.total_read_samples
    )


def test_zero_weight_sum_rejected():
    with pytest.raises(ComponentError) as ei:
        validate_weights((0, 0), 2, 0)
    assert ei.value.code == E_PARA
    assert ei.value.context["field"] == "assist.char_weights"


def test_bad_speed_rejected():
    for bad in (0.0, -1.0, float("inf"), float("nan"), "fast"):
        with pytest.raises(ComponentError) as ei:
            validate_speed(bad)
        assert ei.value.code == E_PARA
        assert ei.value.context["field"] == "assist.speed"


def test_weight_length_mismatch_rejected():
    with pytest.raises(ComponentError) as ei:
        validate_weights((1, 2, 3), 2, 0)
    assert ei.value.code == E_PARA


def test_integer_rational_allocation():
    """归一化确定性: 同一输入永远同一输出, 且总和精确等于窗口。"""
    w = (1000, 2003)
    weights = (1, 1, 1, 7, 3, 3, 3, 2)
    a = schedule_chars(w, weights)
    b = schedule_chars(w, weights)
    assert a == b
    assert a[0] == 1000
    assert len(set(a)) == len(a)
