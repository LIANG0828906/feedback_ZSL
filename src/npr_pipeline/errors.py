from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

E_SCENE_INVALID = "E_SCENE_INVALID"
E_COMMIT_FAILED = "E_COMMIT_FAILED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_STALE_HANDLE = "E_STALE_HANDLE"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"
E_INVALID_REQUEST = "E_INVALID_REQUEST"


@dataclass
class PipelineError(Exception):
    code: str
    location: str
    detail: str = ""
    context: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        suffix = f" ({self.detail})" if self.detail else ""
        return f"{self.code} at {self.location}{suffix}"

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "location": self.location,
            "detail": self.detail,
        }
        if self.context:
            result["context"] = self.context
        return result


def fail(
    code: str,
    location: str,
    detail: str = "",
    **context: Any,
) -> None:
    raise PipelineError(code=code, location=location, detail=detail, context=context)

