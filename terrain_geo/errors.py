"""Business error codes and the unified exception type."""

from __future__ import annotations

from dataclasses import dataclass


class ErrorCode:
    E_PARA = "E_PARA"          # parameter out of range / invalid
    E_GEOM = "E_GEOM"          # ground-truth re-projection over tolerance
    E_BAND = "E_BAND"          # overlap band / buffer limit unsatisfiable
    E_DIFF = "E_DIFF"          # incremental vs full recomputation mismatch
    E_DECODE = "E_DECODE"      # image decode failure / corruption
    E_INTERNAL = "E_INTERNAL"  # stage internal failure (retryable, isolated)
    E_SNAPSHOT = "E_SNAPSHOT"  # snapshot manifest corruption


@dataclass
class GeoError(Exception):
    """Exception carrying a business code, stage id and optional field name."""

    code: str
    message: str
    stage: str | None = None
    field: str | None = None
    detail: dict | None = None

    def __str__(self) -> str:
        loc = ":".join(part for part in (self.stage, self.field) if part)
        prefix = f"[{self.code}]"
        if loc:
            prefix += f" {loc}"
        return f"{prefix} {self.message}"
