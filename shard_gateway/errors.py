"""Business errors exposed by the gateway."""

from __future__ import annotations

from typing import Any


class GatewayError(Exception):
    """An expected business failure with a stable error code."""

    exit_code = 2

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.details:
            result["details"] = self.details
        return result


E_SESSION_NOT_FOUND = "E_SESSION_NOT_FOUND"
E_SESSION_ABORTED = "E_SESSION_ABORTED"
E_SESSION_COMMITTED = "E_SESSION_COMMITTED"
E_SESSION_CONFLICT = "E_SESSION_CONFLICT"
E_INVALID_ARGUMENT = "E_INVALID_ARGUMENT"
E_INVALID_SHARD = "E_INVALID_SHARD"
E_SHARD_GAP = "E_SHARD_GAP"
E_SHARD_DIGEST_MISMATCH = "E_SHARD_DIGEST_MISMATCH"
E_MANIFEST_INCOMPLETE = "E_MANIFEST_INCOMPLETE"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_METADATA_CORRUPT = "E_METADATA_CORRUPT"
