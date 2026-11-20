"""Business errors shared by the generator, validator and workbench."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_INPUT_INVALID = "E_INPUT_INVALID"
E_INPUT_TRUNCATED = "E_INPUT_TRUNCATED"
E_CONSTRAINT_INVALID = "E_CONSTRAINT_INVALID"
E_CONSTRAINT_LIMIT = "E_CONSTRAINT_LIMIT"
E_CONSTRAINT_CONFLICT = "E_CONSTRAINT_CONFLICT"
E_CONSTRAINT_UNSATISFIED = "E_CONSTRAINT_UNSATISFIED"
E_ROOM_OUT_OF_BOUNDS = "E_ROOM_OUT_OF_BOUNDS"
E_DOOR_DANGLING = "E_DOOR_DANGLING"
E_KEY_CYCLE = "E_KEY_CYCLE"
E_LEVEL_INVALID = "E_LEVEL_INVALID"
E_TRACE_INVALID = "E_TRACE_INVALID"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_IO = "E_IO"


def empty_evidence() -> dict[str, Any]:
    return {"rooms": [], "doors": []}


@dataclass
class LevelGenError(Exception):
    """A failure with a stable business code and optional trace evidence."""

    code: str
    message: str
    point: str | None = None
    stage: str | None = None
    constraint_ids: list[str] = field(default_factory=list)
    evidence: dict[str, Any] = field(default_factory=empty_evidence)
    injections_applied: list[dict[str, Any]] = field(default_factory=list)
    report: dict[str, Any] | None = None

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.point is not None:
            payload["point"] = self.point
        if self.stage is not None:
            payload["stage"] = self.stage
        if self.constraint_ids:
            payload["constraint_ids"] = list(self.constraint_ids)
        if self.evidence and (self.evidence.get("rooms") or self.evidence.get("doors")):
            payload["evidence"] = self.evidence
        if self.injections_applied:
            payload["injections_applied"] = list(self.injections_applied)
        if self.report is not None:
            payload["report"] = self.report
        return payload

    @classmethod
    def conflict(
        cls,
        message: str,
        *,
        stage: str,
        constraint_ids: list[str],
        evidence: dict[str, Any] | None = None,
        point: str = "request.constraints",
    ) -> "LevelGenError":
        report = {
            "attempts": 0,
            "constraint_ids": list(dict.fromkeys(constraint_ids)),
            "evidence": evidence or empty_evidence(),
            "message": message,
            "stage": stage,
        }
        return cls(
            code=E_CONSTRAINT_CONFLICT,
            message=message,
            point=point,
            stage=stage,
            constraint_ids=report["constraint_ids"],
            evidence=report["evidence"],
            report=report,
        )
