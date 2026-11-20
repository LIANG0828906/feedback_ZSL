"""Stable business errors used by the rasterization workbench."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_GLYPH_INVALID = "E_GLYPH_INVALID"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_TILE_MISMATCH = "E_TILE_MISMATCH"
E_ATLAS_OVERFLOW = "E_ATLAS_OVERFLOW"
E_SNAPSHOT_MISMATCH = "E_SNAPSHOT_MISMATCH"
E_CANCELLED = "E_CANCELLED"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_IO = "E_IO"


@dataclass
class GlyphAtlasError(Exception):
    """An error with a business code and machine-readable context."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }

    def __str__(self) -> str:
        if not self.details:
            return f"{self.code}: {self.message}"
        return f"{self.code}: {self.message} ({self.details})"


def fail(code: str, message: str, **details: Any) -> None:
    raise GlyphAtlasError(code, message, details)

