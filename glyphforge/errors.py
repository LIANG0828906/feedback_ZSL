from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    E_GLYPH_INVALID = "E_GLYPH_INVALID"
    E_QUANT_INVALID = "E_QUANT_INVALID"
    E_BUSY = "E_BUSY"
    E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
    E_COVERAGE_MISMATCH = "E_COVERAGE_MISMATCH"
    E_ATLAS_OVERFLOW = "E_ATLAS_OVERFLOW"
    E_SNAPSHOT_MISMATCH = "E_SNAPSHOT_MISMATCH"
    E_CANCELLED = "E_CANCELLED"


@dataclass
class GlyphForgeError(Exception):
    code: ErrorCode
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, f"{self.code.value}: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "message": self.message,
            "details": self.details,
        }


class RasterCancelled(Exception):
    """Internal cooperative cancellation signal."""


class RunCancelled(Exception):
    """Raised when a workbench run stops at a tile checkpoint."""

    def __init__(self, snapshot_path: str | None = None):
        super().__init__("rasterization cancelled")
        self.snapshot_path = snapshot_path
