from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


STRUCTURED_ERROR = "STRUCTURED_ERROR"
E_RESUME_MISMATCH = "E_RESUME_MISMATCH"
E_RENDER_DIFFERENCE = "E_RENDER_DIFFERENCE"
E_COMPLIANCE = "E_COMPLIANCE"


@dataclass(frozen=True)
class StructuredError:
    code: str
    channel: str
    scene: str
    field: str
    message: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "channel": self.channel,
            "scene": self.scene,
            "field": self.field,
            "message": self.message,
        }


@dataclass
class BusinessError(Exception):
    errors: list[StructuredError] = field(default_factory=list)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.errors[0].message if self.errors else "business error")

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": STRUCTURED_ERROR,
            "errors": [item.as_dict() for item in self.errors],
        }


class ResumeMismatch(Exception):
    """The committed snapshot cannot be used with the current inputs."""


class RenderDifference(Exception):
    def __init__(self, differences: list[dict[str, Any]]) -> None:
        self.differences = differences
        super().__init__("rendered output differs from reference")

