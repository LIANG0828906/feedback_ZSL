"""Error codes shared across the OCR pipeline.

Every failure raised by the pipeline carries a stable machine-readable code:
  E_VER    unknown schema version or missing required field
  E_COORD  coordinate conversion out of image bounds
  E_MAP    detection/recognition mapping validation failure
  E_STATE  illegal interval state transition
  E_DIFF   incremental vs full recognition mismatch
E_LOSSY is not an exception: it is a warning record emitted during export.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


class PipelineError(Exception):
    code = "E_UNKNOWN"

    def __init__(self, message: str, **context: Any) -> None:
        super().__init__(message)
        self.context = context

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        ctx = " ".join(f"{k}={v!r}" for k, v in self.context.items())
        return f"[{self.code}] {super().__str__()} {ctx}".strip()


class VersionError(PipelineError):
    code = "E_VER"

    def __init__(self, message: str, *, field_path: str) -> None:
        super().__init__(message, field_path=field_path)
        self.field_path = field_path


class CoordError(PipelineError):
    code = "E_COORD"

    def __init__(self, message: str, *, line_id: int, point_index: int) -> None:
        super().__init__(message, line_id=line_id, point_index=point_index)
        self.line_id = line_id
        self.point_index = point_index


class MapError(PipelineError):
    code = "E_MAP"

    def __init__(self, message: str, *, image_id: str, line_id: int) -> None:
        super().__init__(message, image_id=image_id, line_id=line_id)
        self.image_id = image_id
        self.line_id = line_id


class StateError(PipelineError):
    code = "E_STATE"

    def __init__(self, message: str, *, current: str, target: str) -> None:
        super().__init__(message, current=current, target=target)
        self.current = current
        self.target = target


class DiffError(PipelineError):
    code = "E_DIFF"

    def __init__(self, message: str, *, image_id: str, line_id: int, field: str) -> None:
        super().__init__(message, image_id=image_id, line_id=line_id, field=field)
        self.image_id = image_id
        self.line_id = line_id
        self.field = field


@dataclass(frozen=True)
class LossyWarning:
    """E_LOSSY record: a field the target schema version cannot express."""

    code: str
    field_path: str
    original_value: Any
    disposition: str


def lossy(field_path: str, original_value: Any, disposition: str) -> LossyWarning:
    return LossyWarning(
        code="E_LOSSY",
        field_path=field_path,
        original_value=original_value,
        disposition=disposition,
    )
