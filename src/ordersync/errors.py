from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class AppError(Exception):
    code: str
    message: str
    http_status: int = 400
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def payload(self) -> dict[str, Any]:
        error: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        error.update(self.details)
        return {"error": error}


class TopologyError(AppError):
    def __init__(self, reason: str, message: str) -> None:
        super().__init__(
            code="E_TOPOLOGY",
            message=message,
            http_status=503,
            details={"reason": reason},
        )


def invalid_request(message: str) -> AppError:
    return AppError("E_INVALID_REQUEST", message, 400)


def consistency_error(
    message: str,
    *,
    required_watermark: int | None = None,
    replica_applied_watermark: int | None = None,
    reason: str = "watermark_not_reached",
) -> AppError:
    details: dict[str, Any] = {
        "reason": reason,
        "required_watermark": (
            str(required_watermark) if required_watermark is not None else None
        ),
        "replica_applied_watermark": (
            str(replica_applied_watermark)
            if replica_applied_watermark is not None
            else None
        ),
    }
    return AppError("E_CONSISTENCY", message, 409, details)


def write_conflict(message: str = "Order key or line reference conflicts") -> AppError:
    return AppError("E_WRITE_CONFLICT", message, 409)


def write_unavailable(message: str = "Primary database is unavailable") -> AppError:
    return AppError("E_WRITE_UNAVAILABLE", message, 503)


def connection_pool_exhausted() -> AppError:
    return AppError(
        "E_CONN_POOL",
        "A configured database connection pool is exhausted",
        503,
    )


def read_unavailable(message: str = "Requested database is unavailable") -> AppError:
    return AppError("E_READ_UNAVAILABLE", message, 503)

