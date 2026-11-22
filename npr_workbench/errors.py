"""Typed error chain used by the workbench and its CLI/tests."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class NprError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


class ParameterError(NprError):
    def __init__(self, field: str, value: Any, reason: str) -> None:
        super().__init__(
            "E_PARA",
            f"invalid parameter {field}: {reason}",
            {"field": field, "value": value, "reason": reason},
        )


class VersionConflictError(NprError):
    def __init__(self, expected: int, actual: int, reason: str) -> None:
        super().__init__(
            "E_VER",
            f"parameter snapshot conflict: {reason}",
            {
                "expected_version": expected,
                "actual_version": actual,
                "conflict_versions": [expected, actual],
                "reason": reason,
            },
        )


class SeedDivergenceError(NprError):
    def __init__(self, stroke_id: int, reason: str) -> None:
        super().__init__(
            "E_SEED",
            f"stroke seed diverged at stroke {stroke_id}: {reason}",
            {"stroke_id": stroke_id, "reason": reason},
        )


class DivergenceError(NprError):
    def __init__(
        self,
        report: Any,
        preview: Any,
        exact: Any,
        diagnostics: dict[str, str] | None = None,
    ) -> None:
        super().__init__(
            "E_DIVERGE",
            "preview/exact consistency exceeded declared tolerances",
            {"report": report, "diagnostics": diagnostics or {}},
        )
        self.report = report
        self.preview = preview
        self.exact = exact


class PathConsistencyError(NprError):
    def __init__(
        self,
        report: Any,
        diagnostics: dict[str, str] | None = None,
    ) -> None:
        super().__init__(
            "E_PATH",
            "interactive, resumed and one-shot paths diverged",
            {"report": report, "diagnostics": diagnostics or {}},
        )
        self.report = report


class DecodeError(NprError):
    def __init__(
        self,
        path: str,
        reason: str,
        *,
        field: str | None = None,
        cause: str | None = None,
    ) -> None:
        details: dict[str, Any] = {"path": path, "reason": reason}
        if field is not None:
            details["field"] = field
        if cause is not None:
            details["cause"] = cause
        super().__init__("E_DECODE", f"cannot decode scene {path}: {reason}", details)


class TileRenderError(NprError):
    def __init__(self, tile_id: int, reason: str, *, injected: bool = False) -> None:
        super().__init__(
            "E_TILE",
            f"tile {tile_id} failed: {reason}",
            {"tile_id": tile_id, "reason": reason, "injected": injected},
        )


class ResourceError(NprError):
    def __init__(self, field: str, reason: str) -> None:
        super().__init__(
            "E_RESOURCE",
            f"resource configuration rejected at {field}: {reason}",
            {"field": field, "reason": reason},
        )

