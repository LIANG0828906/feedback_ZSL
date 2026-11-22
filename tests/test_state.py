from __future__ import annotations

import pytest

from spatial_workbench.errors import ErrorCode, WorkbenchError
from spatial_workbench.state import FrameState, FrameStateMachine


def _params(frames, index=0):
    return frames[index]


def test_legal_lifecycle_and_commit(analyzed):
    _, frames, _ = analyzed
    machine = FrameStateMachine(1)
    committed = []
    assert machine.state(0) == FrameState.PENDING
    machine.estimate(0, lambda: _params(frames))
    assert machine.state(0) == FrameState.ESTIMATED
    machine.commit(0, lambda params, stats: committed.append(params.fingerprint()))
    assert machine.state(0) == FrameState.COMMITTED
    assert committed == [_params(frames).fingerprint()]


def test_illegal_transition_keeps_state(analyzed):
    _, frames, _ = analyzed
    machine = FrameStateMachine(1)
    with pytest.raises(WorkbenchError) as caught:
        machine.transition(0, FrameState.COMMITTED)
    assert caught.value.code == ErrorCode.E_STATE
    assert machine.state(0) == FrameState.PENDING


def test_committed_parameters_are_frozen(analyzed):
    _, frames, _ = analyzed
    machine = FrameStateMachine(1)
    machine.estimate(0, lambda: _params(frames))
    machine.commit(0, lambda params, stats: None)
    original = machine.params(0).fingerprint()
    with pytest.raises(WorkbenchError) as caught:
        machine.estimate(0, lambda: _params(frames, 1))
    assert caught.value.code == ErrorCode.E_STATE
    assert machine.state(0) == FrameState.COMMITTED
    assert machine.params(0).fingerprint() == original
    leaked = machine.params(0)
    leaked.direct_delays_samples[0] = 999_999
    assert machine.params(0).direct_delays_samples[0] != 999_999


def test_estimated_frame_can_be_reestimated(analyzed):
    _, frames, _ = analyzed
    machine = FrameStateMachine(1)
    machine.estimate(0, lambda: _params(frames, 0))
    first_hash = machine.params(0).fingerprint()
    machine.estimate(0, lambda: _params(frames, 1))
    assert machine.state(0) == FrameState.ESTIMATED
    assert machine.params(0).fingerprint() != first_hash


def test_failure_retry(analyzed):
    _, frames, _ = analyzed
    machine = FrameStateMachine(1)

    def fail():
        raise WorkbenchError(
            ErrorCode.E_FRAME,
            "injected",
            stage="test",
            frame_index=0,
        )

    with pytest.raises(WorkbenchError):
        machine.estimate(0, fail)
    assert machine.state(0) == FrameState.FAILED
    assert machine.error(0).code == ErrorCode.E_FRAME
    machine.retry(0)
    assert machine.state(0) == FrameState.PENDING
    machine.estimate(0, lambda: _params(frames))
    assert machine.state(0) == FrameState.ESTIMATED

