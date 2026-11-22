from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
import threading

from .errors import StateError


class Stage(str, Enum):
    GRADIENT = "gradient"
    THRESHOLD = "threshold"
    OPEN = "open"
    CLOSE = "close"
    THIN = "thin"
    PRUNE = "prune"

    @property
    def label(self) -> str:
        return {
            Stage.GRADIENT: "梯度幅值",
            Stage.THRESHOLD: "双阈值边缘",
            Stage.OPEN: "开运算",
            Stage.CLOSE: "闭运算",
            Stage.THIN: "细化",
            Stage.PRUNE: "短线段剪枝",
        }[self]


STAGE_ORDER: tuple[Stage, ...] = (
    Stage.GRADIENT,
    Stage.THRESHOLD,
    Stage.OPEN,
    Stage.CLOSE,
    Stage.THIN,
    Stage.PRUNE,
)


class BlockState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"

    @property
    def label(self) -> str:
        return {
            BlockState.PENDING: "待处理",
            BlockState.PROCESSING: "处理中",
            BlockState.COMMITTED: "已提交",
            BlockState.FAILED: "失败",
        }[self]


LEGAL_TRANSITIONS: dict[BlockState, frozenset[BlockState]] = {
    BlockState.PENDING: frozenset({BlockState.PROCESSING}),
    BlockState.PROCESSING: frozenset({BlockState.COMMITTED, BlockState.FAILED}),
    BlockState.COMMITTED: frozenset(),
    BlockState.FAILED: frozenset({BlockState.PROCESSING}),
}


@dataclass
class StageCell:
    block_id: int
    stage: Stage
    state: BlockState = BlockState.PENDING
    attempts: int = 0
    error: str | None = None


@dataclass
class StateSnapshot:
    cells: dict[tuple[Stage, int], StageCell]
    total: int
    committed: int
    failed: list[tuple[Stage, int]]

    @property
    def progress(self) -> float:
        return self.committed / self.total if self.total else 1.0

    def matrix(self, block_count: int) -> list[dict[str, object]]:
        rows: list[dict[str, object]] = []
        for block_id in range(block_count):
            row: dict[str, object] = {"block_id": block_id}
            for stage in STAGE_ORDER:
                cell = self.cells[(stage, block_id)]
                row[stage.value] = {
                    "state": cell.state.value,
                    "attempts": cell.attempts,
                    "error": cell.error,
                }
            rows.append(row)
        return rows


@dataclass
class BlockStateMachine:
    block_ids: tuple[int, ...]
    stages: tuple[Stage, ...] = STAGE_ORDER
    _cells: dict[tuple[Stage, int], StageCell] = field(init=False)
    _lock: threading.RLock = field(default_factory=threading.RLock, init=False)

    def __post_init__(self) -> None:
        self._cells = {
            (stage, block_id): StageCell(block_id=block_id, stage=stage)
            for stage in self.stages
            for block_id in self.block_ids
        }

    def cell(self, stage: Stage, block_id: int) -> StageCell:
        with self._lock:
            return self._cells[(stage, block_id)]

    def transition(
        self,
        stage: Stage,
        block_id: int,
        target: BlockState,
        *,
        error: str | None = None,
    ) -> StageCell:
        with self._lock:
            cell = self._cells[(stage, block_id)]
            current = cell.state
            if target not in LEGAL_TRANSITIONS[current]:
                raise StateError(
                    f"非法状态迁移 {current.value} -> {target.value}",
                    block_id=block_id,
                    stage=stage.value,
                    details={
                        "current": current.value,
                        "target": target.value,
                        "allowed": sorted(state.value for state in LEGAL_TRANSITIONS[current]),
                    },
                )
            cell.state = target
            if target is BlockState.PROCESSING:
                cell.attempts += 1
                cell.error = None
            elif target is BlockState.FAILED:
                cell.error = error or "unknown failure"
            return cell

    def restore_committed(self, stage: Stage, block_id: int) -> None:
        with self._lock:
            cell = self._cells[(stage, block_id)]
            cell.state = BlockState.COMMITTED
            cell.attempts = max(1, cell.attempts)
            cell.error = None

    def snapshot(self) -> StateSnapshot:
        with self._lock:
            cells = {
                key: StageCell(
                    block_id=cell.block_id,
                    stage=cell.stage,
                    state=cell.state,
                    attempts=cell.attempts,
                    error=cell.error,
                )
                for key, cell in self._cells.items()
            }
        committed = sum(1 for cell in cells.values() if cell.state is BlockState.COMMITTED)
        failed = [
            (cell.stage, cell.block_id)
            for cell in cells.values()
            if cell.state is BlockState.FAILED
        ]
        return StateSnapshot(
            cells=cells,
            total=len(cells),
            committed=committed,
            failed=failed,
        )

    @property
    def total(self) -> int:
        return len(self._cells)

    @property
    def committed(self) -> int:
        with self._lock:
            return sum(
                1 for cell in self._cells.values() if cell.state is BlockState.COMMITTED
            )

