import pytest

from ocr_pipeline.errors import StateError
from ocr_pipeline.state import IntervalState, build_intervals, progress


def test_legal_transition_path():
    iv = build_intervals("img", 3, 2)[0]
    iv.transition(IntervalState.RECOGNIZING)
    iv.commit(12.0, 0.6, "3.0")
    assert iv.state is IntervalState.COMMITTED
    assert iv.frozen_params == (12.0, 0.6)
    assert iv.frozen_source_version == "3.0"


def test_illegal_transition_raises_e_state_and_keeps_state():
    iv = build_intervals("img", 3, 2)[0]
    with pytest.raises(StateError) as exc:
        iv.transition(IntervalState.COMMITTED)
    assert exc.value.code == "E_STATE"
    assert iv.state is IntervalState.PENDING


def test_committed_is_terminal():
    iv = build_intervals("img", 3, 2)[0]
    iv.transition(IntervalState.RECOGNIZING)
    iv.commit(12.0, 0.6, "3.0")
    for target in (IntervalState.PENDING, IntervalState.RECOGNIZING,
                   IntervalState.FAILED):
        with pytest.raises(StateError):
            iv.transition(target)
        assert iv.state is IntervalState.COMMITTED


def test_failed_can_retry():
    iv = build_intervals("img", 3, 2)[0]
    iv.transition(IntervalState.RECOGNIZING)
    iv.transition(IntervalState.FAILED)
    iv.transition(IntervalState.PENDING)
    assert iv.state is IntervalState.PENDING


def test_interval_partition_and_progress():
    intervals = build_intervals("img", 5, 2)
    assert [(iv.line_start, iv.line_end) for iv in intervals] == [
        (0, 2), (2, 4), (4, 5)]
    assert progress(intervals) == (0, 3)
    for iv in intervals:
        iv.transition(IntervalState.RECOGNIZING)
        iv.commit(1.0, 0.5, "3.0")
    assert progress(intervals) == (3, 3)
