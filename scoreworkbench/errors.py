"""Stable error codes shared by every subsystem.

Every error carries a machine-readable ``code`` plus a structured ``details``
mapping so that the GUI and the test-suite can assert on precise diagnostics
(file path, schema version, first offending field path, event id ...).
"""

from __future__ import annotations

from typing import Any, Mapping


class WorkbenchError(Exception):
    """Base class for all workbench failures."""

    code = "E_UNKNOWN"

    def __init__(self, message: str, details: Mapping[str, Any] | None = None):
        super().__init__(message)
        self.message = message
        self.details: dict[str, Any] = dict(details or {})

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}

    def __str__(self) -> str:  # pragma: no cover - trivial
        return f"{self.code}: {self.message} {self.details}"


class SchemaError(WorkbenchError):
    """Unknown version, missing required field or wrong field type."""

    code = "E_SCHEMA"


class DecodeError(WorkbenchError):
    """File unreadable, missing, or JSON content corrupted."""

    code = "E_DECODE"


class SemanticError(WorkbenchError):
    """Document parses but its content conflicts with domain rules."""

    code = "E_SEMANTIC"


class DanglingError(WorkbenchError):
    """An expression event points at a note that does not exist."""

    code = "E_DANGLE"


class ParameterError(WorkbenchError):
    """Beat / velocity / time argument outside of the legal range."""

    code = "E_PARA"


class RoundtripError(WorkbenchError):
    """Re-exported document no longer matches the loaded source document."""

    code = "E_ROUNDTRIP"


class SnapshotError(WorkbenchError):
    """Snapshot envelope missing, torn, or version-incompatible."""

    code = "E_SNAPSHOT"


class LossyError(WorkbenchError):
    """Aggregated E_LOSSY warnings used when exporting without confirmation."""

    code = "E_LOSSY"

    @classmethod
    def from_warnings(cls, warnings: list[dict[str, Any]]) -> "LossyError":
        return cls(
            "Downgrade would drop unsupported fields; confirmation required",
            {"warnings": warnings},
        )
        return clsError


CODE_TO_CLASS = {
    "E_SCHEMA": SchemaError,
    "E_DECODE": DecodeError,
    "E_SEMANTIC": SemanticError,
    "E_DANGLE": DanglingError,
    "E_PARA": ParameterError,
    "E_ROUNDTRIP": RoundtripError,
    "E_SNAPSHOT": SnapshotError,
    "E_LOSSY": LossyError,
}
