"""Structured errors used by the workbench core and UI."""

from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from enum import Enum
from typing import Any, Iterable


class ErrorCode(str, Enum):
    E_SCHEMA = "E_SCHEMA"
    E_RANGE = "E_RANGE"
    E_REF = "E_REF"
    E_HEUR = "E_HEUR"
    E_PARA = "E_PARA"
    E_FORM = "E_FORM"
    E_ADV = "E_ADV"
    E_DIVERGE = "E_DIVERGE"
    E_NO_PATH = "E_NO_PATH"
    E_IO = "E_IO"
    E_STATE = "E_STATE"


@dataclass(frozen=True)
class ValidationIssue:
    code: ErrorCode
    field: str
    message: str
    coord: tuple[int, int] | None = None
    member_id: str | None = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code.value,
            "field": self.field,
            "message": self.message,
        }
        if self.coord is not None:
            result["coord"] = {"x": self.coord[0], "y": self.coord[1]}
        if self.member_id is not None:
            result["member_id"] = self.member_id
        return result


@dataclass
class WorkbenchError(Exception):
    code: ErrorCode
    message: str
    field: str | None = None
    coord: tuple[int, int] | tuple[int, int, int] | None = None
    member_id: str | None = None
    tick: int | None = None
    deviation: int | None = None
    pair: tuple[str, str] | None = None
    item: str | None = None
    left: Any = None
    right: Any = None
    issues: list[ValidationIssue] = dataclass_field(default_factory=list)
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)
        if not isinstance(self.code, ErrorCode):
            self.code = ErrorCode(self.code)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.value,
            "message": self.message,
        }
        optional = {
            "field": self.field,
            "coord": self.coord,
            "member_id": self.member_id,
            "tick": self.tick,
            "deviation": self.deviation,
            "pair": self.pair,
            "item": self.item,
            "left": self.left,
            "right": self.right,
        }
        payload.update({key: value for key, value in optional.items() if value is not None})
        if self.issues:
            payload["issues"] = [issue.to_dict() for issue in self.issues]
        if self.details:
            payload["details"] = self.details
        return payload


def first_issue_error(
    issues: Iterable[ValidationIssue],
    *,
    message: str = "scene validation failed",
) -> WorkbenchError:
    issue_list = list(issues)
    if not issue_list:
        return WorkbenchError(ErrorCode.E_SCHEMA, message)
    priority = {
        ErrorCode.E_SCHEMA: 0,
        ErrorCode.E_RANGE: 1,
        ErrorCode.E_REF: 2,
        ErrorCode.E_PARA: 3,
        ErrorCode.E_FORM: 4,
        ErrorCode.E_HEUR: 5,
        ErrorCode.E_ADV: 6,
        ErrorCode.E_DIVERGE: 7,
        ErrorCode.E_NO_PATH: 8,
        ErrorCode.E_IO: 9,
        ErrorCode.E_STATE: 10,
    }
    ordered = sorted(
        enumerate(issue_list),
        key=lambda item: (priority.get(item[1].code, 99), item[0]),
    )
    first = ordered[0][1]
    return WorkbenchError(
        first.code,
        first.message,
        field=first.field,
        coord=first.coord,
        member_id=first.member_id,
        issues=[item[1] for item in ordered],
    )
