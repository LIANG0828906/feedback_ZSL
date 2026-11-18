"""Structured business errors with stable error codes."""

from __future__ import annotations

from typing import Any, Iterable


class BinauralBusError(Exception):
    """Base class for errors that can be rendered as stable JSON."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        errors: Iterable[dict[str, Any]] | None = None,
        details: dict[str, Any] | None = None,
        exit_code: int = 2,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.errors = list(errors or [])
        self.details = dict(details or {})
        self.exit_code = exit_code

    def payload(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "status": "error",
            "code": self.code,
            "message": self.message,
        }
        if self.errors:
            result["errors"] = self.errors
        if self.details:
            result["details"] = self.details
        return result


class ValidationFailure(BinauralBusError):
    """One or more project or input-file checks failed."""

    def __init__(self, errors: list[dict[str, Any]]) -> None:
        super().__init__(
            "E_INPUT_VALIDATION",
            "The project or one or more input channels are invalid; no bus output was produced.",
            errors=errors,
            exit_code=2,
        )


class ProcessingFailure(BinauralBusError):
    """A business acceptance check failed after input validation."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        channel: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        error = {"code": code}
        if channel is not None:
            error["channel"] = channel
        super().__init__(
            code,
            message,
            errors=[error],
            details=details,
            exit_code=3,
        )


def error_item(
    code: str,
    *,
    channel: str | None = None,
    message: str | None = None,
    **details: Any,
) -> dict[str, Any]:
    item: dict[str, Any] = {"code": code}
    if channel is not None:
        item["channel"] = channel
    if message is not None:
        item["message"] = message
    item.update(details)
    return item

