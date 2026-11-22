"""Stable error codes shared by the core, CLI and UI."""

from __future__ import annotations

from typing import Any


class WorkbenchError(Exception):
    """An expected workbench failure with a machine-readable code."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        stage: str | None = None,
        param: str | None = None,
        **details: Any,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.stage = stage
        self.param = param
        self.details = details

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.stage is not None:
            payload["stage"] = self.stage
        if self.param is not None:
            payload["param"] = self.param
        payload.update(self.details)
        return payload

    def __str__(self) -> str:
        suffix = ""
        if self.stage:
            suffix += f" [阶段={self.stage}]"
        if self.param:
            suffix += f" [参数={self.param}]"
        return f"{self.code}{suffix}: {self.message}"


class PipelineCancelled(Exception):
    """Internal cooperative-cancellation signal."""

