"""Structured business errors used by the renderer and CLI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    quadrant: int | None = None
    piece_index: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def as_dict(self) -> dict[str, Any]:
        error: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.quadrant is not None:
            error["quadrant"] = self.quadrant
        if self.piece_index is not None:
            error["piece_index"] = self.piece_index
        if self.details:
            error["details"] = self.details
        return {"ok": False, "error": error}


class SceneValidationError(BusinessError):
    """Raised when a scene fails complete preflight validation."""


def scene_error(
    code: str,
    message: str,
    *,
    quadrant: int | None = None,
    piece_index: int | None = None,
    **details: Any,
) -> SceneValidationError:
    return SceneValidationError(
        code=code,
        message=message,
        quadrant=quadrant,
        piece_index=piece_index,
        details=details,
    )
