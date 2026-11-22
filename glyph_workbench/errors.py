from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

ERROR_TITLES = {
    "E_SCHEMA": "glyph schema validation failed",
    "E_CURVE": "unknown curve segment type",
    "E_DECODE": "JSON document could not be decoded",
    "E_PARA": "parameter outside supported range",
    "E_STATE": "illegal segment state transition",
    "E_DIFF": "raster outputs differ",
    "E_IO": "persistence or filesystem operation failed",
}


@dataclass(init=False)
class WorkbenchError(Exception):
    code: str
    message: str
    context: dict[str, Any] = field(default_factory=dict)

    def __init__(
        self,
        code: str,
        message: str,
        context: dict[str, Any] | None = None,
        **context_fields: Any,
    ) -> None:
        self.code = code
        self.message = message
        self.context = dict(context or {})
        self.context.update(context_fields)
        Exception.__init__(self, self.message)

    @property
    def title(self) -> str:
        return ERROR_TITLES.get(self.code, "workbench error")

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "title": self.title,
            "message": self.message,
            "context": self.context,
        }

    def __str__(self) -> str:
        location = ""
        if self.context:
            parts = []
            for key in ("glyph_id", "contour_index", "segment_index", "pixel", "path"):
                if key in self.context and self.context[key] is not None:
                    parts.append(f"{key}={self.context[key]}")
            if parts:
                location = " [" + ", ".join(parts) + "]"
        return f"{self.code}: {self.message}{location}"


def require(condition: bool, code: str, message: str, **context: Any) -> None:
    if not condition:
        raise WorkbenchError(code, message, context)

