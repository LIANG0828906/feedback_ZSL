from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from enum import Enum
from typing import Any


class ErrorCode(str, Enum):
    META = "E_META"
    PARA = "E_PARA"
    FRAME = "E_FRAME"
    CHAN = "E_CHAN"
    ORDER = "E_ORDER"
    CLIP = "E_CLIP"
    BOUND = "E_BOUND"
    STATE = "E_STATE"
    DIVERGE = "E_DIVERGE"


@dataclass
class WorkbenchError(RuntimeError):
    """Typed, serializable error used by every public pipeline boundary."""

    code: ErrorCode
    message: str
    field: str | None = None
    channel: str | int | None = None
    frame_index: int | None = None
    sample_index: int | None = None
    excess: float | None = None
    details: dict[str, Any] = dc_field(default_factory=dict)

    def __post_init__(self) -> None:
        RuntimeError.__init__(self, f"{self.code.value}: {self.message}")

    def __str__(self) -> str:
        parts = [self.code.value, self.message]
        if self.field is not None:
            parts.append(f"field={self.field}")
        if self.channel is not None:
            parts.append(f"channel={self.channel}")
        if self.frame_index is not None:
            parts.append(f"frame={self.frame_index}")
        if self.sample_index is not None:
            parts.append(f"sample={self.sample_index}")
        if self.excess is not None:
            parts.append(f"excess={self.excess:.9g}")
        return " | ".join(parts)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code.value, "message": self.message}
        for key in (
            "field",
            "channel",
            "frame_index",
            "sample_index",
            "excess",
        ):
            value = getattr(self, key)
            if value is not None:
                result[key] = value
        if self.details:
            result["details"] = self.details
        return result


def meta_error(message: str, field: str, **kwargs: Any) -> WorkbenchError:
    return WorkbenchError(ErrorCode.META, message, field=field, **kwargs)


def para_error(message: str, field: str, **kwargs: Any) -> WorkbenchError:
    return WorkbenchError(ErrorCode.PARA, message, field=field, **kwargs)


def frame_error(
    message: str,
    *,
    channel: str | int | None = None,
    frame_index: int | None = None,
    **kwargs: Any,
) -> WorkbenchError:
    return WorkbenchError(
        ErrorCode.FRAME,
        message,
        channel=channel,
        frame_index=frame_index,
        **kwargs,
    )
