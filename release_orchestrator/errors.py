"""Stable business errors used by the CLI and HTTP API."""

from __future__ import annotations

from typing import Any


class BusinessError(Exception):
    """An expected, machine-readable failure.

    ``code`` is part of the public contract and must remain stable.
    """

    status_code = 400

    def __init__(
        self,
        code: str,
        message: str,
        *,
        details: dict[str, Any] | None = None,
        status_code: int | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}
        if status_code is not None:
            self.status_code = status_code

    def to_dict(self) -> dict[str, Any]:
        error: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.details:
            error["details"] = self.details
        return {"ok": False, "error": error}


class PlanValidationError(BusinessError):
    """One or more plan issues; no release or batch is created."""

    status_code = 422

    def __init__(self, issues: list[dict[str, Any]]) -> None:
        if not issues:
            issues = [
                {
                    "code": "PLAN_SCHEMA_INVALID",
                    "message": "plan validation failed",
                }
            ]
        primary = issues[0]
        super().__init__(
            str(primary.get("code", "PLAN_SCHEMA_INVALID")),
            str(primary.get("message", "plan validation failed")),
            details={"issues": issues},
            status_code=422,
        )


def issue(
    code: str,
    message: str,
    *,
    batch: int | None = None,
    instance_id: str | None = None,
    **extra: Any,
) -> dict[str, Any]:
    """Build a stable validation issue, including batch/instance context."""

    payload: dict[str, Any] = {"code": code, "message": message}
    if batch is not None:
        payload["batch"] = batch
    if instance_id is not None:
        payload["instance_id"] = instance_id
    payload.update(extra)
    return payload
