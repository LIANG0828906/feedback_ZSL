"""Stable business error codes shared by every gateway component.

All failures are returned as HTTP 200 envelopes (never 503) so that clients
can distinguish business-level rejection from transport failure.
"""

from __future__ import annotations

from enum import Enum


class ErrorCode(str, Enum):
    OK = "OK"
    INVALID_REQUEST = "INVALID_REQUEST"
    UNKNOWN_PROTOCOL = "UNKNOWN_PROTOCOL"
    PROTOCOL_MISMATCH = "FEATURE_PROTOCOL_MISMATCH"
    FEATURE_COUNT_MISMATCH = "FEATURE_COUNT_MISMATCH"
    FEATURE_COLUMN_MISMATCH = "FEATURE_COLUMN_ORDER_MISMATCH"
    FEATURE_TYPE_MISMATCH = "FEATURE_TYPE_MISMATCH"
    VERSION_UNAVAILABLE = "VERSION_UNAVAILABLE"
    MODEL_LOADING = "MODEL_LOADING"
    MODEL_LOAD_FAILED = "MODEL_LOAD_FAILED"
    READY_WAIT_TIMEOUT = "READY_WAIT_TIMEOUT"
    QUEUE_FULL = "QUEUE_FULL"
    QUEUE_WAIT_TIMEOUT = "QUEUE_WAIT_TIMEOUT"
    INFERENCE_FAILED = "INFERENCE_FAILED"
    INTERNAL_ERROR = "INTERNAL_ERROR"


class GatewayError(Exception):
    """Business error carrying a stable code and a field-locatable message."""

    def __init__(self, code: ErrorCode, message: str, field: str | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.field = field

    def to_dict(self) -> dict:
        body = {
            "code": self.code.value,
            "message": self.message,
        }
        if self.field is not None:
            body["field"] = self.field
        return body
