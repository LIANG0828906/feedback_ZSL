from __future__ import annotations

from dataclasses import dataclass
from dataclasses import field as dataclass_field
from typing import Any

E_SCHEMA = "E_SCHEMA"
E_DECODE = "E_DECODE"
E_DIFF = "E_DIFF"
E_BAKE = "E_BAKE"
E_RESOURCE = "E_RESOURCE"
E_CACHE = "E_CACHE"
E_CANCEL = "E_CANCEL"
E_TRUTH = "E_TRUTH"
E_STATE = "E_STATE"


@dataclass
class PipelineError(Exception):
    """A stable, machine-readable pipeline failure.

    ``tile_id`` and ``field`` are populated whenever the failure can be tied to
    a specific tile or project field. Tests and the UI use these values instead
    of parsing the message.
    """

    code: str
    message: str
    tile_id: str | None = None
    field: str | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def __str__(self) -> str:
        context = []
        if self.tile_id is not None:
            context.append(f"tile={self.tile_id}")
        if self.field is not None:
            context.append(f"field={self.field}")
        prefix = f"{self.code}"
        if context:
            prefix += " " + " ".join(context)
        return f"{prefix}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "tile_id": self.tile_id,
            "field": self.field,
            "details": self.details,
        }


def schema_error(
    message: str,
    *,
    tile_id: str | None = None,
    field_name: str | None = None,
    details: dict[str, Any] | None = None,
) -> PipelineError:
    return PipelineError(
        E_SCHEMA,
        message,
        tile_id=tile_id,
        field=field_name,
        details=details or {},
    )


def decode_error(
    message: str,
    *,
    tile_id: str | None = None,
    field_name: str | None = None,
    details: dict[str, Any] | None = None,
) -> PipelineError:
    return PipelineError(
        E_DECODE,
        message,
        tile_id=tile_id,
        field=field_name,
        details=details or {},
    )
