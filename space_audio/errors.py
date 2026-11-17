"""Structured business error codes and error container."""

from __future__ import annotations

from dataclasses import dataclass

DUPLICATE_SOURCE_ID = "DUPLICATE_SOURCE_ID"
PATH_TIME_ORDER = "PATH_TIME_ORDER"
TRANSMISSION_RANGE = "TRANSMISSION_RANGE"
DRY_FILE_MISSING = "DRY_FILE_MISSING"
SAMPLE_RATE_MISMATCH = "SAMPLE_RATE_MISMATCH"
INVALID_SCENE = "INVALID_SCENE"
PARAM_RANGE = "PARAM_RANGE"
RESUME_MISMATCH = "E_RESUME_MISMATCH"
MASTER_MISMATCH = "MASTER_MISMATCH"
GRACEFUL_INTERRUPT = "GRACEFUL_INTERRUPT"

ALL_CODES = {
    DUPLICATE_SOURCE_ID,
    PATH_TIME_ORDER,
    TRANSMISSION_RANGE,
    DRY_FILE_MISSING,
    SAMPLE_RATE_MISMATCH,
    INVALID_SCENE,
    PARAM_RANGE,
    RESUME_MISMATCH,
    MASTER_MISMATCH,
    GRACEFUL_INTERRUPT,
}


@dataclass(frozen=True)
class BusinessError(Exception):
    """A single structured validation / runtime business error."""

    code: str
    field_path: str
    message: str

    def __str__(self) -> str:
        return f"{self.code} at {self.field_path}: {self.message}"

    def to_dict(self) -> dict:
        return {"code": self.code, "field_path": self.field_path, "message": self.message}

