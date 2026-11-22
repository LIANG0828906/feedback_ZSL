import pytest

from subpipeline.errors import E_STATE, PipelineError
from subpipeline.model import SubtitleResult, Screen
from subpipeline.states import ProgressBoard, SubtitleState


def _result(sid=1):
    return SubtitleResult(
        subtitle_id=sid, segment_ref="seg01", start_sample=0,
        end_sample=100, screens=(Screen(("a", "b")),),
        required_duration_s=0.1, actual_duration_s=0.1,
        tradeoffs=(), read_speed_cps=10.0)


def test_legal_transitions():
    st = SubtitleState(1)
    st.transition("layout")
    st.commit(_result())
    assert st.state == "committed"
    assert st.result.subtitle_id == 1


def test_illegal_transition_keeps_state():
    st = SubtitleState(1)
    with pytest.raises(PipelineError) as exc:
        st.transition("committed")
    assert exc.value.code == E_STATE
    assert st.state == "pending"


def test_committed_cannot_change():
    st = SubtitleState(1)
    st.transition("layout")
    st.commit(_result())
    with pytest.raises(PipelineError) as exc:
        st.transition("layout")
    assert exc.value.code == E_STATE
    assert st.state == "committed"


def test_failed_retry_loop():
    st = SubtitleState(2)
    st.transition("layout")
    st.fail(RuntimeError("boom"))
    assert st.state == "failed"
    st.retry()
    assert st.state == "pending"


def test_progress_board():
    board = ProgressBoard(3)
    for sid in (1, 2, 3):
        board.register(sid)
    board.mark_layout(1)
    board.commit_result(1, _result(1))
    assert board.progress == (1, 3)
    assert board.is_committed(1)
    assert board.snapshot()[2] == "pending"
