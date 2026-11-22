"""Four-state block lifecycle enforcement."""

from __future__ import annotations

from dataclasses import dataclass

from .errors import WorkbenchError

PENDING = "pending"
PROCESSING = "processing"
COMMITTED = "committed"
FAILED = "failed"
STATES = (PENDING, PROCESSING, COMMITTED, FAILED)

LEGAL_TRANSITIONS = {
    PENDING: frozenset({PROCESSING}),
    PROCESSING: frozenset({COMMITTED, FAILED}),
    COMMITTED: frozenset(),
    FAILED: frozenset({PROCESSING}),
}


@dataclass
class BlockStateMachine:
    block_index: int
    state: str = PENDING

    def __post_init__(self) -> None:
        if self.state not in STATES:
            raise WorkbenchError("E_STATE", "unknown block state", {"block_index": self.block_index, "state": self.state})

    def transition(self, target: str) -> str:
        current = self.state
        if target not in LEGAL_TRANSITIONS.get(current, frozenset()):
            raise WorkbenchError(
                "E_STATE",
                "illegal block state transition",
                {"block_index": self.block_index, "from": current, "to": target, "state": current},
            )
        self.state = target
        return self.state

    def can_transition(self, target: str) -> bool:
        return target in LEGAL_TRANSITIONS.get(self.state, frozenset())


class BlockStateMatrix:
    def __init__(self, total_blocks: int) -> None:
        self._machines = {index: BlockStateMachine(index) for index in range(total_blocks)}

    def transition(self, index: int, target: str) -> str:
        if index not in self._machines:
            raise WorkbenchError("E_STATE", "unknown block index", {"block_index": index})
        return self._machines[index].transition(target)

    def set_committed_from_checkpoint(self, indices: list[int]) -> None:
        for index in indices:
            machine = self._machines[index]
            if machine.state != PENDING:
                raise WorkbenchError("E_STATE", "checkpoint state conflicts with pending block", {"block_index": index})
            machine.state = COMMITTED

    def state(self, index: int) -> str:
        return self._machines[index].state

    def counts(self) -> dict[str, int]:
        result = {state: 0 for state in STATES}
        for machine in self._machines.values():
            result[machine.state] += 1
        return result

    def states(self) -> list[str]:
        return [self._machines[index].state for index in sorted(self._machines)]
