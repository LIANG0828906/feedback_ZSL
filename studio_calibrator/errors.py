from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Optional


E_PAIR_MISSING = "E_PAIR_MISSING"
E_BAND_UNRESOLVED = "E_BAND_UNRESOLVED"
E_BUDGET_SHORT = "E_BUDGET_SHORT"
E_PHASE_AMBIGUOUS = "E_PHASE_AMBIGUOUS"
E_CALIB_INFEASIBLE = "E_CALIB_INFEASIBLE"
E_BUSY = "E_BUSY"
E_CONFIG_INVALID = "E_CONFIG_INVALID"
E_IO = "E_IO"
E_LAYOUT_INVALID = "E_LAYOUT_INVALID"
E_CANCELLED = "E_CANCELLED"
E_SNAPSHOT_INVALID = "E_SNAPSHOT_INVALID"


@dataclass(frozen=True)
class BusinessError(Exception):
    code: str
    message: str
    source: Optional[str] = None
    receiver: Optional[str] = None
    band: Optional[str] = None
    details: Dict[str, Any] = field(default_factory=dict)
    severity: str = "error"

    def __str__(self) -> str:
        location = []
        if self.source is not None:
            location.append(f"source={self.source}")
        if self.receiver is not None:
            location.append(f"receiver={self.receiver}")
        if self.band is not None:
            location.append(f"band={self.band}")
        suffix = f" ({', '.join(location)})" if location else ""
        return f"{self.code}: {self.message}{suffix}"

    def to_dict(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "severity": self.severity,
        }
        if self.source is not None:
            payload["source"] = self.source
        if self.receiver is not None:
            payload["receiver"] = self.receiver
        if self.band is not None:
            payload["band"] = self.band
        if self.details:
            payload["details"] = self.details
        return payload


def error_list(errors: list[BusinessError]) -> list[dict[str, Any]]:
    return [error.to_dict() for error in errors]
