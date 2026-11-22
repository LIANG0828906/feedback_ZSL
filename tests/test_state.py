from __future__ import annotations

import pytest

from glyph_workbench.errors import WorkbenchError
from glyph_workbench.state import SegmentState, StateMachine


def test_strict_state_progression_and_progress() -> None:
    machine = StateMachine(2)
    assert machine.to_dict()["progress"] == "0/2"
    machine.transition(0, SegmentState.FLATTENED)
    machine.transition(0, SegmentState.FILLED)
    machine.transition(0, SegmentState.COMMITTED)
    assert machine.committed_count == 1
    assert machine.to_dict()["progress"] == "1/2"
    assert machine.to_dict()["segments"][0]["committed"] is True
    assert machine.to_dict()["segments"][1]["committed"] is False


def test_illegal_transition_returns_e_state_and_keeps_original() -> None:
    machine = StateMachine(1)
    with pytest.raises(WorkbenchError) as caught:
        machine.transition(0, SegmentState.COMMITTED)
    assert caught.value.code == "E_STATE"
    assert machine.to_dict()["segments"][0]["state"] == "PENDING_FLATTEN"
    assert machine.committed_count == 0


def test_committed_is_terminal_and_failure_can_retry() -> None:
    machine = StateMachine(2)
    machine.transition(0, SegmentState.FLATTENED)
    machine.transition(0, SegmentState.FILLED)
    machine.transition(0, SegmentState.COMMITTED)
    with pytest.raises(WorkbenchError):
        machine.fail(0, "E_STATE", "cannot fail committed")
    assert machine.to_dict()["segments"][0]["state"] == "COMMITTED"

    machine.fail(1, "E_STATE", "injected")
    assert machine.to_dict()["segments"][1]["state"] == "FAILED"
    machine.retry(1)
    assert machine.to_dict()["segments"][1]["state"] == "PENDING_FLATTEN"


def test_restore_only_committed_prefix() -> None:
    machine = StateMachine(3)
    machine.restore_committed(2)
    payload = machine.to_dict()
    assert payload["commit_sequence"] == [0, 1]
    assert payload["segments"][2]["state"] == "PENDING_FLATTEN"

