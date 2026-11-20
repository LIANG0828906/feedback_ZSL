from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class AcousticError(Exception):
    """Structured error with a stable public error code."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "details": self.details}


E_ALIGN_FAIL = "E_ALIGN_FAIL"
E_BAND_INVALID = "E_BAND_INVALID"
E_STITCH_FAIL = "E_STITCH_FAIL"
E_FIELD_STALE = "E_FIELD_STALE"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_WAV_INVALID = "E_WAV_INVALID"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_FACTOR_INVALID = "E_FACTOR_INVALID"
E_RENDER_FAIL = "E_RENDER_FAIL"
E_CACHE_CORRUPT = "E_CACHE_CORRUPT"


def fail(code: str, message: str, **details: Any) -> None:
    raise AcousticError(code=code, message=message, details=details)

