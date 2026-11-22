import pytest

from syncdemo.errors import ParamError
from syncdemo.frames import (MODE_AUTHORITATIVE, MODE_PREDICTED, FramePolicy,
                             FrameScheduler)


def test_invalid_params_raise_e_para():
    with pytest.raises(ParamError) as exc:
        FramePolicy(render_fps=0)
    assert exc.value.code == "E_PARA"
    assert exc.value.field == "render_fps"
    with pytest.raises(ParamError):
        FramePolicy(render_fps=50, tick_rate=20)  # not a multiple
    with pytest.raises(ParamError):
        FramePolicy(max_input_delay_frames=0)
    with pytest.raises(ParamError):
        FramePolicy(correction_window_frames=-1)


def test_predicted_frame_for_fresh_input():
    sched = FrameScheduler(FramePolicy())
    sched.note_input(frame=10)
    rec = sched.decide(frame=10, tick=4, has_unacked_inputs=True)
    assert rec.mode == MODE_PREDICTED
    assert rec.reason == "input_responsiveness"


def test_correction_applied_within_window():
    policy = FramePolicy(correction_window_frames=6)
    sched = FrameScheduler(policy)
    sched.note_correction(frame=5)
    rec = sched.decide(frame=5, tick=2, has_unacked_inputs=False)
    assert rec.mode == MODE_AUTHORITATIVE
    assert rec.reason == "correction_applied_within_window"
    assert sched.correction_frames() == [5]


def test_conflict_prioritises_correction_with_reason():
    sched = FrameScheduler(FramePolicy())
    sched.note_input(frame=7)
    sched.note_correction(frame=7)
    rec = sched.decide(frame=7, tick=3, has_unacked_inputs=True)
    assert rec.mode == MODE_AUTHORITATIVE
    assert rec.reason.startswith("conflict:correction_priority")


def test_input_delay_violations_empty_for_responsive_log():
    sched = FrameScheduler(FramePolicy())
    sched.note_input(frame=0)
    for frame in range(6):
        sched.decide(frame=frame, tick=frame // 3, has_unacked_inputs=True)
    assert sched.input_delay_violations([0]) == []
