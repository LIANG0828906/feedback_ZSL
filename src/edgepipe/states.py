"""Per-block, per-stage four-state machine."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, List, Mapping, Optional, Set, Tuple

from .config import STAGES
from .errors import EdgePipeError, ErrorCode

PENDING = "PENDING"
PROCESSING = "PROCESSING"
COMMITTED = "COMMITTED"
FAILED = "FAILED"
VALID_STATES = (PENDING, PROCESSING, COMMITTED, FAILED)

LEGAL_TRANSITIONS: Dict[Tuple[str, str], Set[Tuple[str, str]]] = {
    (PENDING, PROCESSING),
    (PROCESSING, COMMITTED),
    (PROCESSING, FAILED),
    # Coordinator-only rollback when a batch is abandoned before commit.
    (PROCESSING, PENDING),
    (FAILED, PROCESSING),
}


@dataclass
class StageTaskState:
    block_id: str
    block_index: int
    stage: str
    stage_index: int
    state: str = PENDING
    attempts: int = 0
    error: Optional[Dict[str, Any]] = None

    def to_dict(self) -> Dict[str, Any]:
        return {
            "block_id": self.block_id,
            "block_index": self.block_index,
            "stage": self.stage,
            "stage_index": self.stage_index,
            "state": self.state,
            "attempts": self.attempts,
            "error": self.error,
        }


class BlockStageStateMachine:
    """Legal transitions include a coordinator-only uncommitted rollback."""

    def __init__(self, num_blocks: int, stages: Iterable[str] = STAGES) -> None:
        self._lock = threading.RLock()
        self._stages = tuple(stages)
        self._tasks: Dict[Tuple[str, str], StageTaskState] = {}
        for stage_index, stage in enumerate(self._stages):
            for block_index in range(num_blocks):
                block_id = f"b{block_index:04d}"
                key = (block_id, stage)
                self._tasks[key] = StageTaskState(
                    block_id=block_id,
                    block_index=block_index,
                    stage=stage,
                    stage_index=stage_index,
                )

    @property
    def total(self) -> int:
        return len(self._tasks)

    @property
    def committed_count(self) -> int:
        with self._lock:
            return sum(
                item.state == COMMITTED for item in self._tasks.values()
            )

    @property
    def progress(self) -> Dict[str, Any]:
        with self._lock:
            committed = sum(
                item.state == COMMITTED for item in self._tasks.values()
            )
            failed = sum(item.state == FAILED for item in self._tasks.values())
            processing = sum(
                item.state == PROCESSING for item in self._tasks.values()
            )
            pending = sum(item.state == PENDING for item in self._tasks.values())
            return {
                "committed": committed,
                "total": self.total,
                "fraction": committed / self.total if self.total else 0.0,
                "pending": pending,
                "processing": processing,
                "failed": failed,
            }

    def get(self, block_id: str, stage: str) -> StageTaskState:
        key = (block_id, stage)
        with self._lock:
            if key not in self._tasks:
                raise EdgePipeError(
                    ErrorCode.PARA,
                    "未知分块/阶段任务",
                    field="task",
                    details={"block_id": block_id, "stage": stage},
                )
            return self._tasks[key]

    def transition(
        self,
        block_id: str,
        stage: str,
        new_state: str,
        *,
        error: Optional[Dict[str, Any]] = None,
    ) -> StageTaskState:
        if new_state not in VALID_STATES:
            raise EdgePipeError(
                ErrorCode.STATE,
                f"未知状态: {new_state}",
                field="state",
                details={"allowed": list(VALID_STATES)},
            )
        key = (block_id, stage)
        with self._lock:
            if key not in self._tasks:
                raise EdgePipeError(
                    ErrorCode.STATE,
                    "未知分块/阶段任务",
                    field="task",
                    details={"block_id": block_id, "stage": stage},
                )
            task = self._tasks[key]
            old_state = task.state
            if (old_state, new_state) not in LEGAL_TRANSITIONS:
                raise EdgePipeError(
                    ErrorCode.STATE,
                    f"非法状态迁移 {old_state} -> {new_state}",
                    field="state",
                    details={
                        "block_id": block_id,
                        "stage": stage,
                        "current": old_state,
                        "requested": new_state,
                    },
                )
            task.state = new_state
            if new_state == PROCESSING:
                task.attempts += 1
                task.error = None
            elif new_state == PENDING:
                task.error = None
            elif new_state == FAILED:
                task.error = error or {
                    "code": ErrorCode.INTERNAL,
                    "message": "未提供错误详情",
                }
            elif new_state == COMMITTED:
                task.error = None
            return task

    def restore_committed(self, keys: Iterable[Tuple[str, str]]) -> None:
        """Replay only legal transitions for persisted committed tasks."""

        with self._lock:
            for block_id, stage in keys:
                task = self.get(block_id, stage)
                if task.state == COMMITTED:
                    continue
                self.transition(block_id, stage, PROCESSING)
                self.transition(block_id, stage, COMMITTED)

    def failed_tasks(self) -> List[Dict[str, Any]]:
        with self._lock:
            return [
                item.to_dict()
                for item in self._tasks.values()
                if item.state == FAILED
            ]

    def task_snapshot(self) -> List[Dict[str, Any]]:
        with self._lock:
            return [item.to_dict() for item in self._tasks.values()]

    def state_matrix(self, num_blocks: int) -> List[List[str]]:
        with self._lock:
            matrix: List[List[str]] = []
            for block_index in range(num_blocks):
                block_id = f"b{block_index:04d}"
                matrix.append(
                    [self._tasks[(block_id, stage)].state for stage in self._stages]
                )
            return matrix

    def reset_failed_to_pending(self) -> None:
        """Only for snapshot rebuild; normal retry still uses FAILED->PROCESSING."""

        with self._lock:
            for task in self._tasks.values():
                if task.state == FAILED:
                    task.state = PENDING
                    task.error = None


def state_matrix_with_headers(
    machine: BlockStageStateMachine, num_blocks: int
) -> Dict[str, Any]:
    return {
        "blocks": [f"b{index:04d}" for index in range(num_blocks)],
        "stages": list(STAGES),
        "states": machine.state_matrix(num_blocks),
        "tasks": machine.task_snapshot(),
    }
