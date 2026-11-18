"""Stable application errors exposed by the CLI and HTTP API."""

from __future__ import annotations

from typing import Any


class HandoverError(Exception):
    """A business error with a stable code and HTTP status."""

    def __init__(
        self,
        code: str,
        message: str,
        *,
        http_status: int = 400,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.http_status = http_status
        self.details = details or {}

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.details:
            payload["details"] = self.details
        return payload


def duplicate_timestamp_error(handover_id: str) -> HandoverError:
    return HandoverError(
        "DUPLICATE_HANDOVER_TIMESTAMP",
        "A snapshot already exists for this handover timestamp.",
        http_status=409,
        details={"handover_id": handover_id},
    )


def duplicate_id_error(handover_at: str) -> HandoverError:
    return HandoverError(
        "DUPLICATE_HANDOVER_ID",
        "A snapshot already exists for this handover identifier.",
        http_status=409,
        details={"handover_at": handover_at},
    )
