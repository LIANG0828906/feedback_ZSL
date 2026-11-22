from __future__ import annotations

import pytest

from transparent_pipeline.errors import E_STATE, PipelineError
from transparent_pipeline.state import BlockStateMachine, BlockStatus


def test_legal_state_progression():
    machine = BlockStateMachine([0])
    assert machine.status(0) == "pending_intersect"
    machine.transition(0, BlockStatus.TRANSMISSION_ACCUMULATING)
    machine.transition(0, BlockStatus.SCATTERING_ACCUMULATING)
    machine.transition(0, BlockStatus.COMMITTED, media_versions={"m": 1}, light_versions={"l": 1})
    assert machine.status(0) == "committed"
    assert machine.progress() == {"committed": 1, "total": 1, "failed": 0}


def test_illegal_state_preserves_status():
    machine = BlockStateMachine([0])
    with pytest.raises(PipelineError) as caught:
        machine.transition(0, BlockStatus.COMMITTED)
    assert caught.value.code == E_STATE
    assert machine.status(0) == "pending_intersect"


def test_committed_cannot_advance_without_invalidation():
    machine = BlockStateMachine([0])
    machine.transition(0, BlockStatus.TRANSMISSION_ACCUMULATING)
    machine.transition(0, BlockStatus.SCATTERING_ACCUMULATING)
    machine.transition(0, BlockStatus.COMMITTED)
    with pytest.raises(PipelineError) as caught:
        machine.transition(0, BlockStatus.SCATTERING_ACCUMULATING)
    assert caught.value.code == E_STATE
    assert machine.status(0) == "committed"
    machine.invalidate(0)
    assert machine.status(0) == "pending_intersect"


def test_failed_retry():
    machine = BlockStateMachine([0])
    machine.transition(0, BlockStatus.FAILED, failure="boom")
    assert machine.status(0) == "failed"
    machine.retry(0)
    assert machine.status(0) == "pending_intersect"

