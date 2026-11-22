"""Block/stage state machine with frozen committed results."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from enum import Enum
from typing import Any

from .errors import BusinessError


class Stage(str, Enum):
    GROWTH = "growth"
    CLEANUP = "cleanup"
    STATISTICS = "statistics"


class StageState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


STAGES: tuple[Stage, ...] = (Stage.GROWTH, Stage.CLEANUP, Stage.STATISTICS)
STAGE_ORDER = {stage: index for index, stage in enumerate(STAGES)}

_LEGAL = {
    (StageState.PENDING, StageState.PROCESSING),
    (StageState.PROCESSING, StageState.COMMITTED),
    (StageState.PROCESSING, StageState.FAILED),
    (StageState.FAILED, StageState.PROCESSING),
}


@dataclass(frozen=True)
class BlockGrid:
    width: int
    height: int
    tile_size: int

    @property
    def columns(self) -> int:
        return max(1, (self.width + self.tile_size - 1) // self.tile_size)

    @property
    def rows(self) -> int:
        return max(1, (self.height + self.tile_size - 1) // self.tile_size)

    @property
    def count(self) -> int:
        return self.rows * self.columns

    @property
    def stage_count(self) -> int:
        return self.count * len(STAGES)

    def rect(self, block_index: int) -> tuple[int, int, int, int]:
        if not 0 <= block_index < self.count:
            raise BusinessError(
                "E_STATE",
                "块编号越界",
                field="block_index",
                details={"actual": block_index, "block_count": self.count},
            )
        row, column = divmod(block_index, self.columns)
        x0 = column * self.tile_size
        y0 = row * self.tile_size
        x1 = min(self.width, x0 + self.tile_size)
        y1 = min(self.height, y0 + self.tile_size)
        return x0, y0, x1, y1

    def to_dict(self) -> dict[str, int]:
        return {
            "width": self.width,
            "height": self.height,
            "tile_size": self.tile_size,
            "rows": self.rows,
            "columns": self.columns,
            "block_count": self.count,
            "stage_count": self.stage_count,
        }


@dataclass
class StageRecord:
    payload: bytes
    payload_hash: str
    worker_id: str | None
    sequence: int | None


class BlockStateMachine:
    def __init__(self, grid: BlockGrid) -> None:
        self.grid = grid
        self._lock = threading.RLock()
        self._states: dict[tuple[int, Stage], StageState] = {
            (block_index, stage): StageState.PENDING
            for block_index in range(grid.count)
            for stage in STAGES
        }
        self._records: dict[tuple[int, Stage], StageRecord] = {}
        self._errors: dict[tuple[int, Stage], str] = {}
        self._invalidated_blocks: set[int] = set()

    def _key(self, block_index: int, stage: Stage | str) -> tuple[int, Stage]:
        if not 0 <= block_index < self.grid.count:
            raise BusinessError(
                "E_STATE",
                "块编号越界",
                field="block_index",
                details={"actual": block_index, "block_count": self.grid.count},
            )
        try:
            normalized = stage if isinstance(stage, Stage) else Stage(stage)
        except ValueError as exc:
            raise BusinessError(
                "E_STATE",
                "未知阶段",
                field="stage",
                details={"actual": str(stage)},
            ) from exc
        return block_index, normalized

    def state(self, block_index: int, stage: Stage | str) -> StageState:
        with self._lock:
            return self._states[self._key(block_index, stage)]

    def transition(
        self,
        block_index: int,
        stage: Stage | str,
        target: StageState,
    ) -> StageState:
        with self._lock:
            key = self._key(block_index, stage)
            current = self._states[key]
            if (current, target) not in _LEGAL:
                raise BusinessError(
                    "E_STATE",
                    "非法状态迁移",
                    field="stage_state",
                    details={
                        "block_index": block_index,
                        "stage": key[1].value,
                        "current": current.value,
                        "target": target.value,
                    },
                )
            if key[1] != Stage.GROWTH and target == StageState.PROCESSING:
                predecessor = STAGES[STAGE_ORDER[key[1]] - 1]
                if self._states[(block_index, predecessor)] != StageState.COMMITTED:
                    raise BusinessError(
                        "E_STATE",
                        "前一阶段尚未提交",
                        field="stage_order",
                        details={
                            "block_index": block_index,
                            "stage": key[1].value,
                            "predecessor": predecessor.value,
                            "predecessor_state": self._states[
                                (block_index, predecessor)
                            ].value,
                        },
                    )
            self._states[key] = target
            if target != StageState.FAILED:
                self._errors.pop(key, None)
            return target

    def begin(self, block_index: int, stage: Stage | str, worker_id: str) -> None:
        self.transition(block_index, stage, StageState.PROCESSING)
        with self._lock:
            record = self._records.get(self._key(block_index, stage))
            if record is not None:
                record.worker_id = worker_id

    def fail(self, block_index: int, stage: Stage | str, message: str) -> None:
        self.transition(block_index, stage, StageState.FAILED)
        with self._lock:
            self._errors[self._key(block_index, stage)] = message

    def commit(
        self,
        block_index: int,
        stage: Stage | str,
        payload: bytes,
        payload_hash: str,
        *,
        worker_id: str | None = None,
        sequence: int | None = None,
    ) -> None:
        key = self._key(block_index, stage)
        with self._lock:
            if self._states[key] != StageState.PROCESSING:
                raise BusinessError(
                    "E_STATE",
                    "只有处理中的阶段才能提交",
                    field="stage_state",
                    details={
                        "block_index": block_index,
                        "stage": key[1].value,
                        "current": self._states[key].value,
                    },
                )
            self._records[key] = StageRecord(
                payload=payload,
                payload_hash=payload_hash,
                worker_id=worker_id,
                sequence=sequence,
            )
            self._states[key] = StageState.COMMITTED

    def restore_committed(
        self,
        block_index: int,
        stage: Stage | str,
        payload: bytes,
        payload_hash: str,
        *,
        sequence: int | None = None,
    ) -> None:
        key = self._key(block_index, stage)
        with self._lock:
            if self._states[key] == StageState.COMMITTED:
                return
            if self._states[key] != StageState.PENDING:
                raise BusinessError(
                    "E_STATE",
                    "只能恢复待处理阶段",
                    field="stage_state",
                    details={
                        "block_index": block_index,
                        "stage": key[1].value,
                        "current": self._states[key].value,
                    },
                )
            if key[1] != Stage.GROWTH:
                predecessor = STAGES[STAGE_ORDER[key[1]] - 1]
                if self._states[(block_index, predecessor)] != StageState.COMMITTED:
                    raise BusinessError(
                        "E_STATE",
                        "恢复阶段顺序不连续",
                        field="stage_order",
                        details={
                            "block_index": block_index,
                            "stage": key[1].value,
                            "predecessor": predecessor.value,
                        },
                    )
            self._records[key] = StageRecord(
                payload=payload,
                payload_hash=payload_hash,
                worker_id="checkpoint",
                sequence=sequence,
            )
            self._states[key] = StageState.COMMITTED

    def record(self, block_index: int, stage: Stage | str) -> StageRecord | None:
        with self._lock:
            return self._records.get(self._key(block_index, stage))

    def error(self, block_index: int, stage: Stage | str) -> str | None:
        with self._lock:
            return self._errors.get(self._key(block_index, stage))

    def progress(self) -> tuple[int, int]:
        with self._lock:
            committed = sum(
                1 for value in self._states.values() if value == StageState.COMMITTED
            )
            return committed, self.grid.stage_count

    def is_complete(self) -> bool:
        committed, total = self.progress()
        return committed == total

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            blocks = []
            for block_index in range(self.grid.count):
                x0, y0, x1, y1 = self.grid.rect(block_index)
                stages = {}
                for stage in STAGES:
                    key = (block_index, stage)
                    stages[stage.value] = {
                        "state": self._states[key].value,
                        "error": self._errors.get(key),
                        "sequence": (
                            self._records[key].sequence
                            if key in self._records
                            else None
                        ),
                    }
                blocks.append(
                    {
                        "block_index": block_index,
                        "rect": [x0, y0, x1, y1],
                        "invalidated": block_index in self._invalidated_blocks,
                        "stages": stages,
                    }
                )
            committed, total = self.progress()
            return {
                "grid": self.grid.to_dict(),
                "blocks": blocks,
                "committed_stages": committed,
                "total_stages": total,
                "progress": f"{committed}/{total}",
            }

    def active_block_count(self) -> int:
        with self._lock:
            active = set()
            for key, value in self._states.items():
                if value in {StageState.PENDING, StageState.PROCESSING, StageState.FAILED}:
                    active.add(key[0])
            return len(active)

    def invalidate_uncommitted(self) -> list[int]:
        """Freeze committed stages and list every block with unfrozen stages."""

        with self._lock:
            invalidated = sorted(
                {
                    block_index
                    for (block_index, _stage), value in self._states.items()
                    if value != StageState.COMMITTED
                }
            )
            self._invalidated_blocks.update(invalidated)
            return invalidated

    def failed_keys(self) -> list[tuple[int, Stage]]:
        with self._lock:
            return sorted(
                [key for key, value in self._states.items() if value == StageState.FAILED],
                key=lambda key: (key[0], STAGE_ORDER[key[1]]),
            )

    def mark_cancelled_processing(self) -> None:
        with self._lock:
            for key, value in self._states.items():
                if value == StageState.PROCESSING:
                    self._states[key] = StageState.FAILED
                    self._errors[key] = "cancelled"
