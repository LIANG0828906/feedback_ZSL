"""分块阶段状态机。

每个块的每个阶段（grow/clean/stats）独立维护一个四态状态机：
``PENDING -> RUNNING -> COMMITTED``，任意阶段可 ``-> FAILED``，
``FAILED`` 允许重试回到 ``RUNNING``。非法迁移抛出 ``E_STATE`` 并保持
原状态不变。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Dict, Iterable, Tuple

from segworkbench.core.errors import ErrorCode, fail

PENDING = "PENDING"
RUNNING = "RUNNING"
COMMITTED = "COMMITTED"
FAILED = "FAILED"

PHASES = ("grow", "clean", "stats")

# 键为 (当前态, 目标态)。
LEGAL_TRANSITIONS = frozenset(
    {
        (PENDING, RUNNING),
        (RUNNING, COMMITTED),
        (RUNNING, FAILED),
        (FAILED, RUNNING),
        (PENDING, FAILED),
    }
)


@dataclass
class PhaseState:
    block_id: str
    phase: str
    state: str = PENDING
    error_code: str | None = None
    attempts: int = 0

    def transition(self, target: str) -> None:
        if (self.state, target) not in LEGAL_TRANSITIONS:
            raise fail(
                ErrorCode.E_STATE,
                f"非法状态迁移 {self.state} -> {target}",
                block_id=self.block_id,
                phase=self.phase,
                from_state=self.state,
                to_state=target,
            )
        self.state = target


class BlockStateMachine:
    """覆盖全部块 × 阶段的状态机集合。"""

    def __init__(self, block_ids: Iterable[str]):
        self._phases: Dict[Tuple[str, str], PhaseState] = {
            (block_id, phase): PhaseState(block_id, phase)
            for block_id in block_ids
            for phase in PHASES
        }

    def get(self, block_id: str, phase: str) -> PhaseState:
        return self._phases[(block_id, phase)]

    def move(self, block_id: str, phase: str, target: str) -> None:
        self.get(block_id, phase).transition(target)
        if target == RUNNING:
            self.get(block_id, phase).attempts += 1

    def mark_failed(self, block_id: str, phase: str, code: str) -> None:
        phase_state = self.get(block_id, phase)
        if phase_state.state != FAILED:
            phase_state.transition(FAILED)
        phase_state.error_code = code

    def is_committed(self, block_id: str, phase: str) -> bool:
        return self.get(block_id, phase).state == COMMITTED

    def snapshot(self) -> Dict[str, Dict[str, str]]:
        result: Dict[str, Dict[str, str]] = {}
        for (block_id, phase), phase_state in sorted(self._phases.items()):
            result.setdefault(block_id, {})[phase] = phase_state.state
        return result
