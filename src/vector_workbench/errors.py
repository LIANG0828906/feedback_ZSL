from __future__ import annotations

from enum import Enum
from typing import Any, Mapping


class ErrorCode(str, Enum):
    E_DECODE = "E_DECODE"
    E_GEOM = "E_GEOM"
    E_PARA = "E_PARA"
    E_STATE = "E_STATE"
    E_SELF = "E_SELF"
    E_DEGEN = "E_DEGEN"
    E_CLIP = "E_CLIP"
    E_STAGE = "E_STAGE"
    E_DIFF = "E_DIFF"


class WorkbenchError(Exception):
    """Structured error carrying enough context for the diagnostics UI."""

    def __init__(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        path_id: str | None = None,
        segment: int | None = None,
        stage: str | None = None,
        field: str | None = None,
        pixel: tuple[int, int] | None = None,
        recoverable: bool | None = None,
        detail: Mapping[str, Any] | None = None,
    ) -> None:
        self.code = ErrorCode(code)
        self.message = message
        self.path_id = path_id
        self.segment = segment
        self.stage = stage
        self.field = field
        self.pixel = pixel
        self.recoverable = recoverable
        self.detail = dict(detail or {})
        super().__init__(self.__str__())

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "message": self.message,
            "path_id": self.path_id,
            "segment": self.segment,
            "stage": self.stage,
            "field": self.field,
            "pixel": list(self.pixel) if self.pixel else None,
            "recoverable": self.recoverable,
            "detail": self.detail,
        }

    def __str__(self) -> str:
        parts = [self.code.value, self.message]
        if self.path_id is not None:
            parts.append(f"path={self.path_id}")
        if self.segment is not None:
            parts.append(f"segment={self.segment}")
        if self.stage is not None:
            parts.append(f"stage={self.stage}")
        if self.field is not None:
            parts.append(f"field={self.field}")
        if self.pixel is not None:
            parts.append(f"pixel=({self.pixel[0]},{self.pixel[1]})")
        return " | ".join(parts)


class DiffError(WorkbenchError):
    def __init__(self, diff: Any) -> None:
        pixel = getattr(diff, "first_pixel", None)
        path_id = getattr(diff, "path_id", None)
        stage = getattr(diff, "stage", None)
        detail = diff.to_dict() if hasattr(diff, "to_dict") else {"diff": str(diff)}
        super().__init__(
            ErrorCode.E_DIFF,
            "render outputs differ",
            path_id=path_id,
            stage=stage,
            pixel=pixel,
            detail=detail,
        )

