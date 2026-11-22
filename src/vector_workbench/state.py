from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import ErrorCode, WorkbenchError
from .model import CellStatus, Stage, VectorPath


@dataclass
class PathStageRecord:
    statuses: dict[Stage, CellStatus] = field(
        default_factory=lambda: {stage: CellStatus.PENDING for stage in Stage.ordered()}
    )
    errors: dict[Stage, dict[str, Any]] = field(default_factory=dict)

    def table_row(self) -> dict[str, str | None]:
        return {
            **{stage.value: self.statuses[stage].value for stage in Stage.ordered()},
            **{
                f"{stage.value}_error": (
                    self.errors[stage].get("code") if stage in self.errors else None
                )
                for stage in Stage.ordered()
            },
        }


class PathStateMachine:
    """Path-major three-stage state machine with guarded transitions."""

    def __init__(self, paths: Iterable[VectorPath]) -> None:
        self._paths = tuple(paths)
        self._order = {path.id: index for index, path in enumerate(self._paths)}
        self._records = {path.id: PathStageRecord() for path in self._paths}
        self._lock = threading.RLock()

    @property
    def total_cells(self) -> int:
        return len(self._paths) * len(Stage.ordered())

    @property
    def committed_cells(self) -> int:
        with self._lock:
            return sum(
                1
                for record in self._records.values()
                for status in record.statuses.values()
                if status is CellStatus.COMMITTED
            )

    def status(self, path_id: str, stage: Stage) -> CellStatus:
        with self._lock:
            self._record(path_id)
            return self._records[path_id].statuses[stage]

    def errors(self, path_id: str) -> dict[Stage, dict[str, Any]]:
        with self._lock:
            self._record(path_id)
            return dict(self._records[path_id].errors)

    def _record(self, path_id: str) -> PathStageRecord:
        if path_id not in self._records:
            raise WorkbenchError(
                ErrorCode.E_STATE, "unknown path id", path_id=path_id
            )
        return self._records[path_id]

    def _require_committed_locked(self, path_id: str, stage: Stage) -> None:
        if self._records[path_id].statuses[stage] is not CellStatus.COMMITTED:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                f"prerequisite stage {stage.value} is not committed",
                path_id=path_id,
                stage=stage.value,
            )

    def _validate_begin_prerequisites_locked(self, path_id: str, stage: Stage) -> None:
        if stage is Stage.COVERAGE:
            self._require_committed_locked(path_id, Stage.FLATTEN)
        elif stage is Stage.COMPOSE:
            self._require_committed_locked(path_id, Stage.COVERAGE)
            index = self._order[path_id]
            if index > 0:
                previous_id = self._paths[index - 1].id
                self._require_committed_locked(previous_id, Stage.COMPOSE)

    def begin(self, path_id: str, stage: Stage) -> None:
        with self._lock:
            record = self._record(path_id)
            current = record.statuses[stage]
            if current not in (CellStatus.PENDING, CellStatus.FAILED):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    f"illegal transition {current.value} -> processing",
                    path_id=path_id,
                    stage=stage.value,
                )
            self._validate_begin_prerequisites_locked(path_id, stage)
            record.statuses[stage] = CellStatus.PROCESSING
            record.errors.pop(stage, None)

    def commit(self, path_id: str, stage: Stage) -> None:
        with self._lock:
            record = self._record(path_id)
            if record.statuses[stage] is not CellStatus.PROCESSING:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "only a processing cell may be committed",
                    path_id=path_id,
                    stage=stage.value,
                )
            record.statuses[stage] = CellStatus.COMMITTED

    def fail(self, path_id: str, stage: Stage, error: WorkbenchError) -> None:
        with self._lock:
            record = self._record(path_id)
            if record.statuses[stage] is not CellStatus.PROCESSING:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "only a processing cell may fail",
                    path_id=path_id,
                    stage=stage.value,
                )
            record.statuses[stage] = CellStatus.FAILED
            record.errors[stage] = error.to_dict()

    def restore_committed(self, path_id: str, stage: Stage) -> None:
        with self._lock:
            record = self._record(path_id)
            record.statuses[stage] = CellStatus.COMMITTED
            record.errors.pop(stage, None)

    def reset(self) -> None:
        with self._lock:
            for record in self._records.values():
                record.statuses = {
                    stage: CellStatus.PENDING for stage in Stage.ordered()
                }
                record.errors.clear()

    def rows(self) -> list[dict[str, Any]]:
        with self._lock:
            result: list[dict[str, Any]] = []
            for path in self._paths:
                record = self._records[path.id]
                result.append(
                    {
                        "path_id": path.id,
                        "name": path.name,
                        "kind": path.kind,
                        **record.table_row(),
                    }
                )
            return result

    def all_committed(self) -> bool:
        with self._lock:
            return all(
                record.statuses[stage] is CellStatus.COMMITTED
                for record in self._records.values()
                for stage in Stage.ordered()
            )

    def failed_errors(self) -> list[dict[str, Any]]:
        with self._lock:
            errors: list[dict[str, Any]] = []
            for path in self._paths:
                record = self._records[path.id]
                for stage in Stage.ordered():
                    if stage in record.errors:
                        errors.append(record.errors[stage])
            return errors

