from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_DIALECT = "E_DIALECT"
E_READONLY = "E_READONLY"
E_CONN = "E_CONN"
E_TIMEOUT = "E_TIMEOUT"
E_INDEX_UNCOVERED = "E_INDEX_UNCOVERED"
E_INDEX_UNAVAILABLE = "E_INDEX_UNAVAILABLE"
E_PLAN_SCAN = "E_PLAN_SCAN"
E_PLAN_INDEX = "E_PLAN_INDEX"
E_PLAN_SORT_SPILL = "E_PLAN_SORT_SPILL"
E_PLAN_MISSING = "E_PLAN_MISSING"
E_SUMMARY_UNAVAILABLE = "E_SUMMARY_UNAVAILABLE"
E_SUMMARY_LIMIT = "E_SUMMARY_LIMIT"
E_SQL = "E_SQL"
E_REQUEST = "E_REQUEST"
E_FALLBACK = "E_FALLBACK"
E_TABLE = "E_TABLE"
E_INTERNAL = "E_INTERNAL"


@dataclass(slots=True)
class GatewayError(Exception):
    code: str
    message: str
    fragment: str | None = None
    offset: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        error: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.fragment is not None:
            error["fragment"] = self.fragment
        if self.offset is not None:
            error["offset"] = self.offset
        if self.details:
            error["details"] = self.details
        return error
