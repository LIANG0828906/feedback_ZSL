import numpy as np
import pytest

from caption_pipeline.envelope import detect_segments
from caption_pipeline.errors import E_PARA, E_STATE, PipelineError
from caption_pipeline.states import LineState, LineStateMachine


def test_state_machine_legal_path():
    sm = LineStateMachine()
    sm.deps_met = True
    sm.transition(LineState.LAYING_OUT)
    sm.transition(LineState.COMMITTED)
    assert sm.state == LineState.COMMITTED


def test_state_machine_illegal_transition_keeps_state():
    sm = LineStateMachine()
    with pytest.raises(PipelineError) as ei:
        sm.transition(LineState.COMMITTED)
    assert ei.value.code == E_STATE
    assert sm.state == LineState.PENDING


def test_state_machine_deps_not_met():
    sm = LineStateMachine()
    with pytest.raises(PipelineError) as ei:
        sm.transition(LineState.LAYING_OUT)
    assert ei.value.code == E_STATE
    assert sm.state == LineState.PENDING


def test_envelope_detects_synthetic_segments():
    sr = 16000
    audio = np.random.default_rng(1).normal(0, 0.01, sr * 4)
    audio[sr:sr + sr // 2] += np.random.default_rng(2).normal(0, 0.5, sr // 2)
    audio[sr * 2:sr * 3] += np.random.default_rng(3).normal(0, 0.5, sr)
    segs = detect_segments(audio, 400, 160)
    assert len(segs) == 2
    assert abs(segs[0][0] - sr) < 800
    assert abs(segs[1][1] - sr * 3) < 800


@pytest.mark.parametrize("frame_len,hop,field", [
    (0, 160, "frame_len"),
    (400, 0, "hop"),
    (400, 800, "hop"),
])
def test_envelope_param_errors(frame_len, hop, field):
    audio = np.zeros(16000)
    with pytest.raises(PipelineError) as ei:
        detect_segments(audio, frame_len, hop)
    assert ei.value.code == E_PARA and ei.value.field == field
