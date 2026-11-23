"""Structured exceptions with an inspectable error chain."""

from __future__ import annotations

from dataclasses import dataclass
from dataclasses import field as dataclass_field
from typing import Any


@dataclass
class ErrorLink:
    stage: str
    code: str
    message: str
    field: str | None = None
    frame_index: int | None = None
    channel: str | None = None
    context: dict[str, Any] = dataclass_field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "code": self.code,
            "message": self.message,
            "field": self.field,
            "frame_index": self.frame_index,
            "channel": self.channel,
            "context": self.context,
        }


class WorkbenchError(Exception):
    """A failure that can be reported consistently by CLI, GUI, and tests."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        field: str | None = None,
        frame_index: int | None = None,
        channel: str | None = None,
        context: dict[str, Any] | None = None,
        chain: list[ErrorLink] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.field = field
        self.frame_index = frame_index
        self.channel = channel
        self.context = context or {}
        self.chain = list(chain or [])

    def add_stage(self, stage: str, *, context: dict[str, Any] | None = None) -> "WorkbenchError":
        merged = dict(self.context)
        if context:
            merged.update(context)
        link = ErrorLink(
            stage=stage,
            code=self.code,
            message=self.message,
            field=self.field,
            frame_index=self.frame_index,
            channel=self.channel,
            context=merged,
        )
        self.chain.append(link)
        self.context = merged
        return self

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "field": self.field,
            "frame_index": self.frame_index,
            "channel": self.channel,
            "context": self.context,
            "chain": [link.to_dict() for link in self.chain],
        }

    def __str__(self) -> str:
        location = []
        if self.field:
            location.append(f"field={self.field}")
        if self.frame_index is not None:
            location.append(f"frame={self.frame_index}")
        if self.channel:
            location.append(f"channel={self.channel}")
        suffix = f" ({', '.join(location)})" if location else ""
        return f"{self.code}: {self.message}{suffix}"


def error(
    code: str,
    message: str,
    *,
    stage: str,
    field: str | None = None,
    frame_index: int | None = None,
    channel: str | None = None,
    context: dict[str, Any] | None = None,
) -> WorkbenchError:
    exc = WorkbenchError(
        code,
        message,
        field=field,
        frame_index=frame_index,
        channel=channel,
        context=context,
    )
    exc.add_stage(stage)
    return exc
