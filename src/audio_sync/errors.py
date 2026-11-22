from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from typing import Any


E_SCHEMA = "E_SCHEMA"
E_DECODE = "E_DECODE"
E_PARA = "E_PARA"
E_STATE = "E_STATE"
E_BOUND = "E_BOUND"
E_DIFF = "E_DIFF"


@dataclass
class SyncError(Exception):
    """Structured domain error carrying enough context for automation."""

    code: str
    message: str
    channel: str | None = None
    field: str | None = None
    marker: str | None = None
    segment: int | None = None
    sample: int | None = None
    details: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        for key in ("channel", "field", "marker", "segment", "sample"):
            value = getattr(self, key)
            if value is not None:
                result[key] = value
        if self.details:
            result["details"] = self.details
        return result

    def __str__(self) -> str:
        context = []
        if self.channel is not None:
            context.append(f"channel={self.channel}")
        if self.field is not None:
            context.append(f"field={self.field}")
        if self.marker is not None:
            context.append(f"marker={self.marker}")
        if self.segment is not None:
            context.append(f"segment={self.segment}")
        if self.sample is not None:
            context.append(f"sample={self.sample}")
        suffix = f" ({', '.join(context)})" if context else ""
        return f"{self.code}: {self.message}{suffix}"

