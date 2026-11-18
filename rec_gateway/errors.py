from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


HTTP_STATUS_BY_CODE = {
    "INVALID_REQUEST": 400,
    "INVALID_PROTOCOL_VERSION": 400,
    "FEATURE_PROTOCOL_MISMATCH": 400,
    "FEATURE_COUNT_MISMATCH": 400,
    "FEATURE_COLUMN_COUNT_MISMATCH": 400,
    "FEATURE_COLUMN_ORDER_MISMATCH": 400,
    "FEATURE_COLUMN_MISSING": 400,
    "FEATURE_COLUMN_UNEXPECTED": 400,
    "FEATURE_TYPE_MISMATCH": 400,
    "UNKNOWN_TENANT": 404,
    "BATCH_LIMIT_EXCEEDED": 413,
    "IDEMPOTENCY_CONFLICT": 409,
    "IDEMPOTENCY_IN_PROGRESS": 409,
    "MODEL_RELOAD_IN_PROGRESS": 409,
    "UNKNOWN_VERSION": 404,
    "ROUTING_CONFIG_INVALID": 400,
    "FAULT_INJECTION_DISABLED": 400,
    "MODEL_NOT_READY": 503,
    "VERSION_UNAVAILABLE": 503,
    "INFERENCE_TIMEOUT": 504,
    "INFERENCE_ERROR": 500,
    "INTERNAL_ERROR": 500,
}


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)
    http_status: int | None = None

    def __post_init__(self) -> None:
        super().__init__(self.message)
        if self.http_status is None:
            self.http_status = HTTP_STATUS_BY_CODE.get(self.code, 400)

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
        }


class ModelLoadError(Exception):
    def __init__(self, code: str, message: str, details: dict[str, Any] | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}
