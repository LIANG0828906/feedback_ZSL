from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ApiError(Exception):
    code: str
    numeric_code: int
    message: str
    http_status: int = 409
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {
            "ok": False,
            "error": {
                "code": self.code,
                "numeric_code": self.numeric_code,
                "message": self.message,
            },
        }
        if self.details:
            body["error"]["details"] = self.details
        return body


INVALID_REQUEST = ("INVALID_REQUEST", 40001, 400)
UNKNOWN_MEMBER = ("UNKNOWN_MEMBER", 40002, 404)
MEMBER_NOT_ACTIVE = ("MEMBER_NOT_ACTIVE", 40003, 409)
PARTITION_NOT_OWNED = ("PARTITION_NOT_OWNED", 40004, 409)
OFFSET_REGRESSION = ("OFFSET_REGRESSION", 40005, 409)
PARTITION_PAUSED = ("PARTITION_PAUSED", 40006, 409)
REBALANCE_IN_PROGRESS = ("REBALANCE_IN_PROGRESS", 40007, 409)
STALE_GENERATION = ("STALE_GENERATION", 40008, 409)
OFFSET_NOT_AVAILABLE = ("OFFSET_NOT_AVAILABLE", 40009, 404)
KAFKA_UNAVAILABLE = ("KAFKA_UNAVAILABLE", 50001, 503)
BACKEND_ERROR = ("BACKEND_ERROR", 50002, 503)
CONFIGURATION_ERROR = ("CONFIGURATION_ERROR", 50003, 500)


def api_error(spec: tuple[str, int, int], message: str, **details: Any) -> ApiError:
    code, numeric, status = spec
    return ApiError(
        code=code,
        numeric_code=numeric,
        message=message,
        http_status=status,
        details=details,
    )

