"""Business errors and deterministic scene validation failures."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class StructuredError(Exception):
    """An error intended to be serialized as machine-readable JSON."""

    code: str
    reason: str
    plate_id: int | None = None
    layer_id: int | None = None
    triangle_index: int | None = None
    light_id: str | int | None = None
    exit_code: int = 2
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.reason)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "plate_id": self.plate_id,
            "layer_id": self.layer_id,
            "reason": self.reason,
        }
        if self.triangle_index is not None:
            payload["triangle_index"] = self.triangle_index
        if self.light_id is not None:
            payload["light_id"] = self.light_id
        if self.details:
            payload.update(self.details)
        return payload


class ValidationError(StructuredError):
    """Scene validation error with a stable business code."""


class ResumeMismatchError(StructuredError):
    """Resumed output does not match a complete deterministic render."""

    def __init__(self, reason: str, differences: list[dict[str, Any]], total: int):
        super().__init__(
            code="E_RESUME_MISMATCH",
            reason=reason,
            exit_code=3,
            details={
                "difference_count": total,
                "differences": differences,
            },
        )
