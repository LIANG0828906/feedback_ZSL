from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field as dataclass_field
from enum import Enum
from typing import Any, IO


class ErrorCode(str, Enum):
    FIELD_MISSING = "E_FIELD_MISSING"
    TYPE_INCOMPARABLE = "E_TYPE_INCOMPARABLE"
    SCALE_MISMATCH = "E_SCALE_MISMATCH"
    STDDEV_INSUFFICIENT_SAMPLES = "E_STDDEV_INSUFFICIENT_SAMPLES"
    SPILL_WRITE_FAILED = "E_SPILL_WRITE_FAILED"
    PRECISION_BOUND = "E_PRECISION_BOUND"
    NUMERIC_OVERFLOW = "E_NUMERIC_OVERFLOW"
    INVALID_CONFIG = "E_INVALID_CONFIG"
    INPUT_INVALID = "E_INPUT_INVALID"
    GROUP_KEY_INVALID = "E_GROUP_KEY_INVALID"
    PARTIAL_STATE_INVALID = "E_PARTIAL_STATE_INVALID"


@dataclass
class BusinessIssue:
    code: ErrorCode
    reason: str
    group_key: Any = None
    field: str | None = None
    aggregate: str | None = None
    recoverable: bool = True
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code.value,
            "reason": self.reason,
            "group_key": self.group_key,
            "recoverable": self.recoverable,
        }
        if self.field is not None:
            payload["field"] = self.field
        if self.aggregate is not None:
            payload["aggregate"] = self.aggregate
        if self.details:
            payload["details"] = self.details
        return payload


class NumericParseError(ValueError):
    def __init__(
        self,
        code: ErrorCode,
        reason: str,
        *,
        raw: Any = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.raw = raw
        self.details = details or {}


def issue_json(issue: BusinessIssue) -> str:
    return json.dumps(
        issue.to_dict(),
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def write_issues(
    issues: list[BusinessIssue],
    stream: IO[str] | None = None,
) -> None:
    target = stream or sys.stderr
    for issue in issues:
        target.write(issue_json(issue))
        target.write("\n")
    target.flush()
