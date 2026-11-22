"""Structured error model used by the CLI, UI, and tests."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

E_SCHEMA = "E_SCHEMA"
E_GEOM = "E_GEOM"
E_PARA = "E_PARA"
E_STATE = "E_STATE"
E_DIVERGE = "E_DIVERGE"
E_DECODE = "E_DECODE"


@dataclass
class PipelineError(Exception):
    code: str
    field: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(f"{self.code} [{self.field}]: {self.message}")

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": False,
            "error": {
                "code": self.code,
                "field": self.field,
                "message": self.message,
            },
        }
        if self.details:
            result["error"]["details"] = self.details
        return result


def schema_error(field: str, message: str, **details: Any) -> PipelineError:
    return PipelineError(E_SCHEMA, field, message, details)


def geometry_error(field: str, message: str, **details: Any) -> PipelineError:
    return PipelineError(E_GEOM, field, message, details)


def parameter_error(field: str, message: str, **details: Any) -> PipelineError:
    return PipelineError(E_PARA, field, message, details)


def state_error(block_id: int, current: str, target: str, reason: str | None) -> PipelineError:
    return PipelineError(
        E_STATE,
        f"blocks[{block_id}].status",
        f"illegal transition {current} -> {target}",
        {"block_id": block_id, "current": current, "target": target, "reason": reason},
    )


def decode_error(field: str, message: str, **details: Any) -> PipelineError:
    return PipelineError(E_DECODE, field, message, details)

